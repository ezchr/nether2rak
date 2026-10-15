package bridge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/ezchr/nether2rak/proxy"
	"github.com/google/uuid"
	"github.com/sandertv/go-raknet"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// ProbeConfig says which backend ProbeBackend joins and as whom.
type ProbeConfig struct {
	RakNetAddress    string // or
	NetherNetAddress string
	Name, XUID       string
	Stay             time.Duration
	// Identity mints the NetherNet client identity, for backends that require one (Geyser, BDS).
	Identity func(ctx context.Context) (*nethernet.Identity, error)
}

// ProbeReport is what a backend sent at a join.
type ProbeReport struct {
	EntityRuntimeID       uint64
	EntityUniqueID        int64
	Dimension             int32
	GameMode              int32
	Movement              protocol.PlayerMovementSettings
	AuthoritativeInv      bool
	BlockIDHashes         bool
	BlockChecksum         uint64
	BaseGameVersion       string
	GameVersion           string
	Experiments           []string
	CustomBlocks          []string
	Items                 []protocol.ItemEntry
	CustomActors          []string
	ActorCount            int
	BiomeCount            int
	Packs                 []string
	PackStack             []string
	PacksRequired         bool
	Counts                map[string]int
	ItemsFingerprint      string
	CustomItemNames       []string
	FirstAfterStartGame   []string
	SpawnAfter            time.Duration
	ChunkPacketsWhileStay int
}

// ProbeBackend logs a made-up player into a backend through the relay's own backend login and
// reports what the backend sent. See cmd/backendprobe.
func ProbeBackend(ctx context.Context, cfg ProbeConfig) (*ProbeReport, error) {
	var conn backendConn
	var err error
	var transportKey *ecdsa.PrivateKey
	switch {
	case cfg.RakNetAddress != "":
		conn, err = raknet.Dial(cfg.RakNetAddress)
	case cfg.NetherNetAddress != "":
		dctx, cancel := context.WithTimeout(ctx, backendDialTimeout)
		defer cancel()
		var ident *nethernet.Identity
		if cfg.Identity != nil {
			if ident, err = cfg.Identity(dctx); err != nil {
				return nil, fmt.Errorf("client identity: %w", err)
			}
		}
		conn, err = DialNetherNetBackend(dctx, cfg.NetherNetAddress, ident, nil)
		if ident != nil {
			transportKey = ident.PrivateKey
		}
	default:
		return nil, fmt.Errorf("no backend address")
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	id := login.IdentityData{
		XUID:        cfg.XUID,
		DisplayName: cfg.Name,
		Identity:    uuid.NewMD5(uuid.NameSpaceOID, []byte("n2rprobe:"+cfg.XUID)).String(),
	}
	start := time.Now()
	c := proxy.NewProxyConn(conn, false)
	if err := c.ForwardIdentity(protocol.CurrentProtocol, id, ProbeClientData(cfg.Name), false, transportKey); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	sp, err := joinBackend(c, 8, 30*time.Second)
	rep := sp.report(c.Pool())
	rep.SpawnAfter = time.Since(start)
	if err != nil {
		return rep, err
	}
	// The spawn is sent, so the client would now say it is initialised.
	_ = c.WritePacket(&packet.SetLocalPlayerAsInitialised{EntityRuntimeID: sp.StartGame.EntityRuntimeID})
	end := time.Now().Add(cfg.Stay)
	if d, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(end)
	}
	for time.Now().Before(end) {
		pks, err := c.ReadPackets()
		if err != nil {
			break
		}
		for _, raw := range pks {
			if id, _, err := splitPacket(raw); err == nil {
				rep.Counts[packetName(c.Pool(), id)]++
				if id == packet.IDLevelChunk || id == packet.IDSubChunk {
					rep.ChunkPacketsWhileStay++
				}
			}
		}
	}
	return rep, nil
}

func packetName(pool packet.Pool, id uint32) string {
	if f, ok := pool[id]; ok {
		return strings.TrimPrefix(fmt.Sprintf("%T", f()), "*packet.")
	}
	return fmt.Sprintf("id%d", id)
}

func (sp *backendSpawn) report(pool packet.Pool) *ProbeReport {
	rep := &ProbeReport{Counts: map[string]int{}}
	for id, n := range sp.Counts {
		rep.Counts[packetName(pool, id)] += n
	}
	for i, raw := range sp.After {
		if i >= 40 {
			break
		}
		if id, _, err := splitPacket(raw); err == nil {
			rep.FirstAfterStartGame = append(rep.FirstAfterStartGame, packetName(pool, id))
		}
	}
	if g := sp.StartGame; g != nil {
		rep.EntityRuntimeID, rep.EntityUniqueID = g.EntityRuntimeID, g.EntityUniqueID
		rep.Dimension, rep.GameMode = g.Dimension, g.PlayerGameMode
		rep.Movement = g.PlayerMovementSettings
		rep.AuthoritativeInv = g.ServerAuthoritativeInventory
		rep.BlockIDHashes = g.UseBlockNetworkIDHashes
		rep.BlockChecksum = g.ServerBlockStateChecksum
		rep.BaseGameVersion, rep.GameVersion = g.BaseGameVersion, g.GameVersion
		for _, e := range g.Experiments {
			rep.Experiments = append(rep.Experiments, fmt.Sprintf("%s=%v", e.Name, e.Enabled))
		}
		for _, b := range g.Blocks {
			rep.CustomBlocks = append(rep.CustomBlocks, b.Name)
		}
		sort.Strings(rep.CustomBlocks)
	}
	if sp.ItemRegistry != nil {
		rep.Items = sp.ItemRegistry.Items
		h := sha256.New()
		for _, it := range sp.ItemRegistry.Items {
			fmt.Fprintf(h, "%s=%d;", it.Name, it.RuntimeID)
			if !strings.HasPrefix(it.Name, "minecraft:") || it.ComponentBased {
				rep.CustomItemNames = append(rep.CustomItemNames, fmt.Sprintf("%s=%d", it.Name, it.RuntimeID))
			}
		}
		rep.ItemsFingerprint = fmt.Sprintf("%x", h.Sum(nil)[:8])
	}
	if sp.Actors != nil {
		var m map[string]any
		if nbt.NewDecoderWithEncoding(bytes.NewReader(sp.Actors.SerialisedEntityIdentifiers), nbt.NetworkLittleEndian).Decode(&m) == nil {
			if list, ok := m["idlist"].([]any); ok {
				rep.ActorCount = len(list)
				for _, e := range list {
					if em, ok := e.(map[string]any); ok {
						if s, _ := em["id"].(string); !strings.HasPrefix(s, "minecraft:") {
							rep.CustomActors = append(rep.CustomActors, s)
						}
					}
				}
			}
		}
	}
	if sp.Biomes != nil {
		rep.BiomeCount = len(sp.Biomes.BiomeDefinitions)
	}
	if p := sp.PacksInfo; p != nil {
		rep.PacksRequired = p.TexturePackRequired
		for _, t := range p.TexturePacks {
			rep.Packs = append(rep.Packs, fmt.Sprintf("%s_%s (%d bytes)", t.UUID, t.Version, t.Size))
		}
	}
	if s := sp.PackStack; s != nil {
		for _, t := range s.TexturePacks {
			rep.PackStack = append(rep.PackStack, t.UUID+"_"+t.Version)
		}
	}
	return rep
}

// Summary is a short human-readable form of the report.
func (r *ProbeReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "spawned after %v\nself runtime id %d, unique id %d, dimension %d, game mode %d\n", r.SpawnAfter.Round(time.Millisecond), r.EntityRuntimeID, r.EntityUniqueID, r.Dimension, r.GameMode)
	fmt.Fprintf(&b, "movement %+v\nauthoritative inventory %v, block id hashes %v, block checksum %d\n", r.Movement, r.AuthoritativeInv, r.BlockIDHashes, r.BlockChecksum)
	fmt.Fprintf(&b, "versions base %q game %q, experiments %v\n", r.BaseGameVersion, r.GameVersion, r.Experiments)
	fmt.Fprintf(&b, "custom blocks (%d): %v\n", len(r.CustomBlocks), r.CustomBlocks)
	fmt.Fprintf(&b, "items %d, fingerprint %s, custom/component items (%d): %v\n", len(r.Items), r.ItemsFingerprint, len(r.CustomItemNames), r.CustomItemNames)
	fmt.Fprintf(&b, "actors %d, custom: %v\nbiomes %d\n", r.ActorCount, r.CustomActors, r.BiomeCount)
	fmt.Fprintf(&b, "packs (required %v): %v\nstack: %v\n", r.PacksRequired, r.Packs, r.PackStack)
	fmt.Fprintf(&b, "first packets after StartGame: %v\n", r.FirstAfterStartGame)
	keys := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("packet counts:")
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, r.Counts[k])
	}
	fmt.Fprintf(&b, "\nchunk packets while staying: %d\n", r.ChunkPacketsWhileStay)
	return b.String()
}

// ProbeClientData is client data for a made-up player: a plain skin and an Android device.
func ProbeClientData(name string) login.ClientData {
	d := login.ClientData{
		ThirdPartyName:      name,
		ServerAddress:       "127.0.0.1:19132",
		DeviceOS:            protocol.DeviceAndroid,
		DefaultInputMode:    packet.InputModeTouch,
		CurrentInputMode:    packet.InputModeTouch,
		GameVersion:         protocol.CurrentVersion,
		ClientRandomID:      rand.Int64(),
		LanguageCode:        "en_GB",
		PlayFabID:           fmt.Sprintf("%016x", rand.Uint64()),
		AnimatedImageData:   []login.SkinAnimation{},
		PersonaPieces:       []login.PersonaPiece{},
		PieceTintColours:    []login.PersonaPieceTintColour{},
		SelfSignedID:        uuid.NewString(),
		SkinID:              uuid.NewString(),
		SkinData:            base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0, 0, 0, 255}, 64*64)),
		SkinImageHeight:     64,
		SkinImageWidth:      64,
		SkinResourcePatch:   base64.StdEncoding.EncodeToString([]byte(`{"geometry":{"default":"geometry.humanoid.custom"}}`)),
		SkinGeometryVersion: base64.StdEncoding.EncodeToString([]byte("0.0.0")),
		ArmSize:             "wide",
		SkinColour:          "#0",
	}
	d.DeviceID = d.ExpectedDeviceIDFormat().Generate()
	return d
}
