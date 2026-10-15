package bridge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/ezchr/nether2rak/proxy"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// A Network is a set of backend servers the relay moves players between without them leaving the
// world (and its Friends-tab session): the relay stays connected to the player and swaps the server
// behind them, the way WaterdogPE does for Bedrock networks. See switchsession.go for the session
// and transfer.go for the move itself.
//
// The servers may differ in custom content. A Bedrock client takes resource packs, custom blocks,
// items, entity identifiers and experiments only once, at its first join, so the relay gives every
// player the union of all servers' content and renumbers per server where needed (rewrite.go). That
// union comes from what each server sent the last time anyone joined it, kept on disk (Dir), and the
// resource packs are downloaded from each server once and served by the relay itself.
type Network struct {
	// Servers in order; Default is where players join. Each needs a relay-mode door (a backend that
	// accepts the relay's re-signed logins).
	Servers []ServerConfig
	Default string
	// Dir holds the per-server content cache and downloaded packs.
	Dir string
	// Identity mints NetherNet client identities for NetherNet servers.
	Identity func(ctx context.Context) (*nethernet.Identity, error)
	// Admins may use /broadcast. Listing switches the Friends-tab listing; nil disables /broadcast.
	Admins  map[string]bool
	Listing Listing
	Log     *slog.Logger

	mu       sync.Mutex
	content  map[string]*serverContent
	packs    map[string]*cachedPack // uuid_version -> pack
	sessions sync.Map               // xuid -> *switchSession
	fetching map[string]bool
	// requestMode: servers seen sending chunks in sub-chunk request mode (server name -> true).
	// Geyser never does, Dragonfly does; a client must not request sub-chunks from a server that
	// doesn't use them (Geyser kicks it: "unused packet SubChunkRequestPacket").
	requestMode sync.Map
}

// usesRequestMode reports whether a server was seen sending chunks in sub-chunk request mode.
func (n *Network) usesRequestMode(server string) bool {
	v, ok := n.requestMode.Load(server)
	return ok && v.(bool)
}

// Listing is the Friends-tab listing switch (the broadcast), owned by the main package.
type Listing interface {
	On() bool
	Set(on bool) error
}

// ServerConfig is one server of a Network.
type ServerConfig struct {
	Name string `json:"name"`
	// Address is a NetherNet signaling URL (http://127.0.0.1:19133) or, with Transport "raknet", a
	// RakNet host:port.
	Address   string `json:"address"`
	Transport string `json:"transport,omitempty"`
}

// serverContent is what a server sent at its last join: what the union is built from.
type serverContent struct {
	StartGame *packet.StartGame
	Items     *packet.ItemRegistry
	Actors    *packet.AvailableActorIdentifiers
	Biomes    *packet.BiomeDefinitionList
	PacksInfo *packet.ResourcePacksInfo
	PackStack *packet.ResourcePackStack
}

type cachedPack struct {
	info protocol.TexturePackInfo
	data []byte
	hash []byte
}

// contentFiles names the cached packets of a server, in Dir/<server>/.
var contentFiles = map[uint32]string{
	packet.IDStartGame:                 "start_game.bin",
	packet.IDItemRegistry:              "item_registry.bin",
	packet.IDAvailableActorIdentifiers: "actor_identifiers.bin",
	packet.IDBiomeDefinitionList:       "biome_definitions.bin",
	packet.IDResourcePacksInfo:         "resource_packs_info.bin",
	packet.IDResourcePackStack:         "resource_pack_stack.bin",
}

// Init loads the cache. Call once before serving.
func (n *Network) Init() error {
	if n.Log == nil {
		n.Log = slog.Default()
	}
	n.content = map[string]*serverContent{}
	n.packs = map[string]*cachedPack{}
	n.fetching = map[string]bool{}
	if n.server(n.Default) == nil {
		return fmt.Errorf("default server %q is not one of the servers", n.Default)
	}
	for _, s := range n.Servers {
		c := &serverContent{}
		for id, file := range contentFiles {
			b, err := os.ReadFile(filepath.Join(n.Dir, s.Name, file))
			if err != nil {
				continue
			}
			c.set(id, b)
		}
		n.content[s.Name] = c
	}
	entries, _ := os.ReadDir(filepath.Join(n.Dir, "packs"))
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".zip")
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(n.Dir, "packs", e.Name()))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		n.packs[key] = &cachedPack{data: data, hash: sum[:]}
	}
	return nil
}

func (n *Network) server(name string) *ServerConfig {
	for i := range n.Servers {
		if strings.EqualFold(n.Servers[i].Name, name) {
			return &n.Servers[i]
		}
	}
	return nil
}

// set decodes and keeps a cached packet payload.
func (c *serverContent) set(id uint32, payload []byte) {
	var pk packet.Packet
	switch id {
	case packet.IDStartGame:
		pk = &packet.StartGame{}
	case packet.IDItemRegistry:
		pk = &packet.ItemRegistry{}
	case packet.IDAvailableActorIdentifiers:
		pk = &packet.AvailableActorIdentifiers{}
	case packet.IDBiomeDefinitionList:
		pk = &packet.BiomeDefinitionList{}
	case packet.IDResourcePacksInfo:
		pk = &packet.ResourcePacksInfo{}
	case packet.IDResourcePackStack:
		pk = &packet.ResourcePackStack{}
	default:
		return
	}
	if decodeInto(pk, payload) != nil {
		return
	}
	switch pk := pk.(type) {
	case *packet.StartGame:
		c.StartGame = pk
	case *packet.ItemRegistry:
		c.Items = pk
	case *packet.AvailableActorIdentifiers:
		c.Actors = pk
	case *packet.BiomeDefinitionList:
		c.Biomes = pk
	case *packet.ResourcePacksInfo:
		c.PacksInfo = pk
	case *packet.ResourcePackStack:
		c.PackStack = pk
	}
}

// record keeps what a server just sent at a join, on disk too when it changed, and starts fetching
// resource packs the relay does not have yet.
func (n *Network) record(server string, id uint32, payload []byte) {
	file, ok := contentFiles[id]
	if !ok {
		return
	}
	path := filepath.Join(n.Dir, server, file)
	if old, err := os.ReadFile(path); err == nil && (bytes.Equal(old, payload) || fingerprint(id, old) == fingerprint(id, payload)) {
		return
	}
	n.mu.Lock()
	c := n.content[server]
	if c == nil {
		c = &serverContent{}
		n.content[server] = c
	}
	c.set(id, payload)
	n.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		n.Log.Warn("network: could not cache server content", "server", server, "file", file, "err", err)
	} else {
		n.Log.Info("network: server content changed, cached for the next joins", "server", server, "file", file)
	}
	if id == packet.IDResourcePacksInfo {
		go n.fetchPacks(server)
	}
}

// union is the content a joining client gets: every server's, the default server's first.
type union struct {
	packsInfo *packet.ResourcePacksInfo
	packStack *packet.ResourcePackStack
	blocks    []protocol.BlockEntry
	palette   *blockPalette
	items     *packet.ItemRegistry
	actors    *packet.AvailableActorIdentifiers
	exps      []protocol.ExperimentData
	missing   []string // servers with nothing cached yet
}

func (n *Network) orderedServers() []ServerConfig {
	out := []ServerConfig{*n.server(n.Default)}
	for _, s := range n.Servers {
		if !strings.EqualFold(s.Name, n.Default) {
			out = append(out, s)
		}
	}
	return out
}

func (n *Network) buildUnion() (*union, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	u := &union{
		packsInfo: &packet.ResourcePacksInfo{},
		packStack: &packet.ResourcePackStack{BaseGameVersion: "*"},
		items:     &packet.ItemRegistry{},
	}
	seenPack := map[string]bool{}
	seenStack := map[string]bool{}
	seenBlock := map[string]bool{}
	seenExp := map[string]bool{}
	itemByName := map[string]bool{}
	itemByID := map[int16]string{}
	actorSeen := map[string]bool{}
	var actors []any
	for i, s := range n.orderedServers() {
		c := n.content[s.Name]
		if c == nil || c.StartGame == nil {
			u.missing = append(u.missing, s.Name)
			continue
		}
		if p := c.PacksInfo; p != nil {
			u.packsInfo.TexturePackRequired = u.packsInfo.TexturePackRequired || p.TexturePackRequired
			u.packsInfo.HasScripts = u.packsInfo.HasScripts || p.HasScripts
			u.packsInfo.HasAddons = u.packsInfo.HasAddons || p.HasAddons
			for _, t := range p.TexturePacks {
				key := t.UUID.String() + "_" + t.Version
				if seenPack[t.UUID.String()] {
					continue
				}
				if n.packs[key] == nil {
					n.Log.Warn("network: a resource pack is not downloaded yet, players joining now go without it", "server", s.Name, "pack", key)
					continue
				}
				seenPack[t.UUID.String()] = true
				t.DownloadURL = ""
				u.packsInfo.TexturePacks = append(u.packsInfo.TexturePacks, t)
			}
		}
		if c.PackStack == nil && c.PacksInfo != nil {
			// The stack (which downloaded packs the client applies) isn't known yet, for example
			// after the cache was cleared: apply every pack the server lists, or the client
			// downloads them and shows none.
			for _, t := range c.PacksInfo.TexturePacks {
				id := t.UUID.String()
				if !seenStack[id] && seenPack[id] {
					seenStack[id] = true
					u.packStack.TexturePacks = append(u.packStack.TexturePacks, protocol.StackResourcePack{UUID: id, Version: t.Version})
				}
			}
		}
		if st := c.PackStack; st != nil {
			if i == 0 && st.BaseGameVersion != "" {
				u.packStack.BaseGameVersion = st.BaseGameVersion
			}
			for _, t := range st.TexturePacks {
				if !seenStack[t.UUID] && (seenPack[t.UUID] || !isServerPack(c, t.UUID)) {
					seenStack[t.UUID] = true
					u.packStack.TexturePacks = append(u.packStack.TexturePacks, t)
				}
			}
		}
		for _, b := range c.StartGame.Blocks {
			if !seenBlock[b.Name] {
				seenBlock[b.Name] = true
				u.blocks = append(u.blocks, b)
			}
		}
		for _, e := range c.StartGame.Experiments {
			if !seenExp[e.Name] && e.Enabled {
				seenExp[e.Name] = true
				u.exps = append(u.exps, e)
			}
		}
		if c.Items != nil {
			for _, it := range c.Items.Items {
				if itemByName[it.Name] {
					continue
				}
				if other, ok := itemByID[it.RuntimeID]; ok {
					n.Log.Warn("network: two servers number different items the same, players switching may see the wrong item",
						"server", s.Name, "item", it.Name, "id", it.RuntimeID, "other", other)
					continue
				}
				itemByName[it.Name], itemByID[it.RuntimeID] = true, it.Name
				if seenBlock[it.Name] && !it.ComponentBased {
					// A custom block's own item: drawn as the block, as Geyser sends it, so the
					// 2D icon Dragonfly gives the Ores Plus ones (128 px pre-renders) goes. Its
					// display name stays: the pack only names these blocks by their addon ids.
					it.Data = withoutItemIcon(it.Data)
				}
				u.items.Items = append(u.items.Items, it)
			}
		}
		if c.Actors != nil {
			var m map[string]any
			if nbt.NewDecoderWithEncoding(bytes.NewReader(c.Actors.SerialisedEntityIdentifiers), nbt.NetworkLittleEndian).Decode(&m) == nil {
				list, _ := m["idlist"].([]any)
				for _, e := range list {
					em, _ := e.(map[string]any)
					id, _ := em["id"].(string)
					if id != "" && !actorSeen[id] {
						actorSeen[id] = true
						actors = append(actors, e)
					}
				}
			}
		}
	}
	if len(u.missing) == len(n.Servers) {
		return nil, errors.New("no server has been joined yet: run backendprobe on each server once")
	}
	u.packStack.TexturePackRequired = u.packsInfo.TexturePackRequired
	u.packStack.Experiments = u.exps
	u.packStack.ExperimentsPreviouslyToggled = len(u.exps) > 0
	b, err := nbtEncode(map[string]any{"idlist": actors})
	if err != nil {
		return nil, err
	}
	u.actors = &packet.AvailableActorIdentifiers{SerialisedEntityIdentifiers: b}
	u.blocks = uniqueBlockIDs(u.blocks) // see blockids.go
	if u.palette, err = newBlockPalette(u.blocks); err != nil {
		return nil, err
	}
	return u, nil
}

// isServerPack reports whether a stack entry is one of the server's own packs (as opposed to a
// vanilla pack the stack names).
func isServerPack(c *serverContent, id string) bool {
	if c.PacksInfo == nil {
		return false
	}
	for _, t := range c.PacksInfo.TexturePacks {
		if t.UUID.String() == id {
			return true
		}
	}
	return false
}

func nbtEncode(v any) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	err := nbt.NewEncoderWithEncoding(buf, nbt.NetworkLittleEndian).Encode(v)
	return buf.Bytes(), err
}

// dialServer opens a backend leg to a server and logs the player into it with the relay's re-signed
// copy of their identity. What follows (packs, StartGame) is the link's to handle.
func (n *Network) dialServer(ctx context.Context, s ServerConfig, protocolID int32, id login.IdentityData, cd login.ClientData) (*proxy.ProxyConn, backendConn, error) {
	cfg := Config{BackendTransport: s.Transport, NetherNetIdentity: n.Identity, Log: n.Log}
	switch strings.ToLower(s.Transport) {
	case TransportRakNet:
		cfg.GeyserAddress = s.Address
	default:
		cfg.BackendTransport = TransportNetherNet
		cfg.NetherNetBackendAddress = s.Address
	}
	conn, key, err := dialBackend(ctx, cfg, n.Log)
	if err != nil {
		return nil, nil, err
	}
	c := proxy.NewProxyConn(conn, false)
	done := make(chan error, 1)
	go func() { done <- c.ForwardIdentity(protocolID, id, cd, false, key) }()
	select {
	case err = <-done:
	case <-time.After(backendDialTimeout):
		err = errors.New("login timed out")
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("log in: %w", err)
	}
	return c, conn, nil
}

// fetchPacks downloads the resource packs of a server the relay has not got yet, logging in as a
// made-up player who never gets past the pack screen (so never joins the world).
func (n *Network) fetchPacks(server string) {
	n.mu.Lock()
	if n.fetching[server] {
		n.mu.Unlock()
		return
	}
	n.fetching[server] = true
	c := n.content[server]
	var want []protocol.TexturePackInfo
	if c != nil && c.PacksInfo != nil {
		for _, t := range c.PacksInfo.TexturePacks {
			if n.packs[t.UUID.String()+"_"+t.Version] == nil {
				want = append(want, t)
			}
		}
	}
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.fetching, server)
		n.mu.Unlock()
	}()
	if c != nil && c.PacksInfo != nil && len(want) == 0 {
		return
	}
	if err := n.downloadPacks(server); err != nil {
		n.Log.Warn("network: could not download resource packs", "server", server, "err", err)
	}
}

// FetchAllPacks downloads every server's missing packs (and learns their pack lists), and joins
// each server the relay has nothing cached for once, to learn its content; run at start.
func (n *Network) FetchAllPacks() {
	for _, s := range n.Servers {
		n.mu.Lock()
		c := n.content[s.Name]
		known := c != nil && c.StartGame != nil
		n.mu.Unlock()
		if known {
			go n.fetchPacks(s.Name)
			continue
		}
		go func(name string) {
			if err := n.learn(name); err != nil {
				n.Log.Warn("network: could not learn a server's content (retried at the next start or join)", "server", name, "err", err)
			}
		}(name(s))
	}
}

func name(s ServerConfig) string { return s.Name }

// learn joins a server once as a made-up player ("n2rprobe") and caches what it sends, for a server
// nobody has joined through this relay yet. The made-up player is in the world for a few seconds.
func (n *Network) learn(server string) error {
	s := n.server(server)
	if s == nil {
		return fmt.Errorf("no server %q", server)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := login.IdentityData{XUID: learnerXUID, DisplayName: "n2rprobe", Identity: "6e327061-636b-3300-8000-000000000003"}
	c, conn, err := n.dialServer(ctx, *s, protocol.CurrentProtocol, id, ProbeClientData("n2rprobe"))
	if err != nil {
		return err
	}
	defer conn.Close()
	sp, err := joinBackend(c, 4, 30*time.Second)
	if err != nil {
		return err
	}
	_ = c.WritePacket(&packet.SetLocalPlayerAsInitialised{EntityRuntimeID: sp.StartGame.EntityRuntimeID})
	for _, pk := range []packet.Packet{sp.PacksInfo, sp.PackStack, sp.StartGame, sp.ItemRegistry, sp.Actors, sp.Biomes} {
		if reflectNil(pk) {
			continue
		}
		if _, payload, err := splitPacket(encodePacket(pk)); err == nil {
			n.record(server, pk.ID(), payload)
		}
	}
	// Some servers send their registries after the spawn.
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		pks, err := c.ReadPackets()
		if err != nil {
			break
		}
		for _, raw := range pks {
			if pid, payload, err := splitPacket(raw); err == nil {
				if _, ok := contentFiles[pid]; ok && !(pid == packet.IDItemRegistry && sp.ItemRegistry != nil) {
					// (A second item list after the spawn is an update, not the registry.)
					n.record(server, pid, payload)
				}
			}
		}
	}
	n.Log.Info("network: learned a server's content", "server", server)
	return nil
}

func reflectNil(pk packet.Packet) bool {
	switch pk := pk.(type) {
	case *packet.ResourcePacksInfo:
		return pk == nil
	case *packet.ResourcePackStack:
		return pk == nil
	case *packet.StartGame:
		return pk == nil
	case *packet.ItemRegistry:
		return pk == nil
	case *packet.AvailableActorIdentifiers:
		return pk == nil
	case *packet.BiomeDefinitionList:
		return pk == nil
	}
	return pk == nil
}

// Made-up players the relay logs in as to download packs and learn a server's content. Different
// XUIDs, since both can run at once and servers refuse a second login of one player.
const (
	packFetcherXUID = "2535400000000002"
	learnerXUID     = "2535400000000003"
)

func (n *Network) downloadPacks(server string) error {
	s := n.server(server)
	if s == nil {
		return fmt.Errorf("no server %q", server)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := login.IdentityData{XUID: packFetcherXUID, DisplayName: "n2rpacks", Identity: "6e327061-636b-3300-8000-000000000002"}
	c, conn, err := n.dialServer(ctx, *s, protocol.CurrentProtocol, id, ProbeClientData("n2rpacks"))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))

	type pending struct {
		info   *packet.ResourcePackDataInfo
		data   []byte
		chunks uint32
	}
	downloads := map[string]*pending{}
	var info *packet.ResourcePacksInfo
	got := 0
	for {
		pks, err := c.ReadPackets()
		if err != nil {
			return err
		}
		for _, raw := range pks {
			pid, payload, err := splitPacket(raw)
			if err != nil {
				return err
			}
			switch pid {
			case packet.IDDisconnect:
				var pk packet.Disconnect
				_ = decodeInto(&pk, payload)
				return fmt.Errorf("disconnected: %s", pk.Message)
			case packet.IDResourcePacksInfo:
				info = &packet.ResourcePacksInfo{}
				if err := decodeInto(info, payload); err != nil {
					return err
				}
				n.record(server, pid, payload)
				var ids []string
				n.mu.Lock()
				for _, t := range info.TexturePacks {
					key := t.UUID.String() + "_" + t.Version
					if n.packs[key] == nil && t.DownloadURL == "" {
						ids = append(ids, key)
					}
				}
				n.mu.Unlock()
				if len(ids) == 0 {
					return nil
				}
				if err := c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseSendPacks, PacksToDownload: ids}); err != nil {
					return err
				}
				got = len(ids)
			case packet.IDResourcePackStack:
				return nil // the server had nothing to send
			case packet.IDResourcePackDataInfo:
				pk := &packet.ResourcePackDataInfo{}
				if err := decodeInto(pk, payload); err != nil {
					return err
				}
				downloads[pk.UUID] = &pending{info: pk, data: make([]byte, 0, pk.Size)}
				for i := uint32(0); i < pk.ChunkCount; i++ {
					if err := c.WritePacket(&packet.ResourcePackChunkRequest{UUID: pk.UUID, ChunkIndex: int32(i)}); err != nil {
						return err
					}
				}
			case packet.IDResourcePackChunkData:
				var pk packet.ResourcePackChunkData
				if err := decodeInto(&pk, payload); err != nil {
					return err
				}
				d := downloads[pk.UUID]
				if d == nil {
					continue
				}
				d.data = append(d.data, pk.Data...)
				d.chunks++
				if d.chunks < d.info.ChunkCount {
					continue
				}
				sum := sha256.Sum256(d.data)
				if len(d.info.Hash) > 0 && !bytes.Equal(sum[:], d.info.Hash) {
					return fmt.Errorf("pack %s: hash mismatch", pk.UUID)
				}
				key := pk.UUID
				if !strings.Contains(key, "_") {
					for _, t := range info.TexturePacks {
						if t.UUID.String() == key {
							key += "_" + t.Version
						}
					}
				}
				_ = os.MkdirAll(filepath.Join(n.Dir, "packs"), 0o755)
				if err := os.WriteFile(filepath.Join(n.Dir, "packs", key+".zip"), d.data, 0o644); err != nil {
					return err
				}
				n.mu.Lock()
				n.packs[key] = &cachedPack{data: d.data, hash: sum[:]}
				n.mu.Unlock()
				n.Log.Info("network: downloaded resource pack", "server", server, "pack", key, "bytes", len(d.data))
				got--
				if got == 0 {
					return nil
				}
			}
		}
	}
}

// packChunkSize is the size of the pack chunks the relay serves.
const packChunkSize = 1024 * 128

func (n *Network) pack(key string) *cachedPack {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.packs[key]
}

// Sessions lists who is on which server.
func (n *Network) Sessions() map[string]string {
	out := map[string]string{}
	n.sessions.Range(func(k, v any) bool {
		out[k.(string)] = v.(*switchSession).serverName()
		return true
	})
	return out
}

// Switch moves a player (by XUID) to a server.
func (n *Network) Switch(xuid, server string) error {
	v, ok := n.sessions.Load(xuid)
	if !ok {
		return fmt.Errorf("player %s is not connected through this relay", xuid)
	}
	s := n.server(server)
	if s == nil {
		return fmt.Errorf("no server named %q", server)
	}
	return v.(*switchSession).requestSwitch(*s, "")
}

// ServerNames lists the servers.
func (n *Network) ServerNames() []string {
	out := make([]string, 0, len(n.Servers))
	for _, s := range n.Servers {
		out = append(out, s.Name)
	}
	return out
}

var _ *ecdsa.PrivateKey

// fingerprint is what the union uses of a cached packet, so a server that encodes the same content
// differently each time (NBT maps have no order) does not count as changed.
func fingerprint(id uint32, payload []byte) string {
	c := &serverContent{}
	c.set(id, payload)
	var b strings.Builder
	switch {
	case c.StartGame != nil:
		g := c.StartGame
		fmt.Fprintf(&b, "%d|%d|%+v|%v|%v|", g.EntityRuntimeID, g.EntityUniqueID, g.PlayerMovementSettings, g.ServerAuthoritativeInventory, g.UseBlockNetworkIDHashes)
		for _, bl := range g.Blocks {
			b.WriteString(bl.Name + ",")
		}
		for _, e := range g.Experiments {
			fmt.Fprintf(&b, "%s=%v,", e.Name, e.Enabled)
		}
	case c.Items != nil:
		for _, it := range c.Items.Items {
			fmt.Fprintf(&b, "%s=%d,", it.Name, it.RuntimeID)
		}
	case c.Actors != nil:
		var m map[string]any
		_ = nbt.NewDecoderWithEncoding(bytes.NewReader(c.Actors.SerialisedEntityIdentifiers), nbt.NetworkLittleEndian).Decode(&m)
		list, _ := m["idlist"].([]any)
		var ids []string
		for _, e := range list {
			em, _ := e.(map[string]any)
			idv, _ := em["id"].(string)
			ids = append(ids, idv)
		}
		slices.Sort(ids)
		b.WriteString(strings.Join(ids, ","))
	case c.Biomes != nil:
		fmt.Fprintf(&b, "%d", len(c.Biomes.BiomeDefinitions))
	case c.PacksInfo != nil:
		for _, t := range c.PacksInfo.TexturePacks {
			fmt.Fprintf(&b, "%s_%s,", t.UUID, t.Version)
		}
	case c.PackStack != nil:
		for _, t := range c.PackStack.TexturePacks {
			fmt.Fprintf(&b, "%s_%s,", t.UUID, t.Version)
		}
	default:
		return string(payload)
	}
	return b.String()
}

// withoutItemIcon copies item component data without item_properties' minecraft:icon.
func withoutItemIcon(data map[string]any) map[string]any {
	comps, ok := data["components"].(map[string]any)
	if !ok {
		return data
	}
	props, ok := comps["item_properties"].(map[string]any)
	if !ok {
		return data
	}
	np := make(map[string]any, len(props))
	for k, v := range props {
		if k != "minecraft:icon" {
			np[k] = v
		}
	}
	nc := make(map[string]any, len(comps))
	for k, v := range comps {
		nc[k] = v
	}
	nc["item_properties"] = np
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	out["components"] = nc
	return out
}
