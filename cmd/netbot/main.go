// Command netbot is a test client for a relay in network mode: it joins through the relay's
// direct-IP door with a real Xbox account, plays the client's side of the pack screen, the join and
// of every server move (acknowledging dimension changes like a real client), moves between servers
// with /server, and checks the chunks it gets against the block palette the relay gave it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/ezchr/nether2rak/bridge"
	"github.com/ezchr/nether2rak/proxy"
	"github.com/ezchr/nether2rak/session"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

type savingTokenSource struct {
	src  oauth2.TokenSource
	path string
	last string
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err == nil && t.RefreshToken != "" && t.RefreshToken != s.last {
		if b, err := json.Marshal(t); err == nil && os.WriteFile(s.path, b, 0o600) == nil {
			s.last = t.RefreshToken
		}
	}
	return t, err
}

func main() {
	addr := flag.String("relay", "http://127.0.0.1:19190", "relay direct-IP signaling address")
	tokenFile := flag.String("token", "token.json", "token.json of the Xbox account to join as")
	route := flag.String("route", "paper,dragonfly", "servers to move to, in order, with /server")
	stay := flag.Duration("stay", 12*time.Second, "time on each server")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b, err := os.ReadFile(*tokenFile)
	if err != nil {
		fail("token: %v", err)
	}
	tok := new(oauth2.Token)
	if err := json.Unmarshal(b, tok); err != nil {
		fail("token: %v", err)
	}
	src := &savingTokenSource{src: auth.AndroidConfig.RefreshTokenSource(tok), path: *tokenFile, last: tok.RefreshToken}
	sess, err := session.SessionFromTokenSource(src, auth.AndroidConfig, context.Background())
	if err != nil {
		fail("session: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ident, err := bridge.NewClientIdentity(ctx, sess)
	if err != nil {
		fail("identity: %v", err)
	}
	conn, err := bridge.DialNetherNetBackend(ctx, *addr, ident, log)
	cancel()
	if err != nil {
		fail("dial: %v", err)
	}
	c := proxy.NewProxyConn(conn, false)
	cd := bridge.ProbeClientData("netbot")
	cd.ServerAddress = "127.0.0.1:19190"
	if err := c.Login(cd, sess, protocol.CurrentProtocol); err != nil {
		fail("login: %v", err)
	}
	log.Info("logged in", "name", c.IdentityData().DisplayName)
	bot := &bot{c: c, log: log, counts: map[string]int{}, packs: map[string]*packDL{}}
	go bot.read()
	if !bot.waitSpawn(60 * time.Second) {
		fail("no spawn: %v", bot.err())
	}
	bot.report("joined")
	time.Sleep(*stay)
	bot.report("first server")
	for _, server := range strings.Split(*route, ",") {
		bot.reset()
		log.Info("moving", "to", server)
		_ = c.WritePacket(&packet.CommandRequest{CommandLine: "/server " + server, CommandOrigin: protocol.CommandOrigin{Origin: protocol.CommandOriginPlayer, UUID: [16]byte{1}}, Version: "latest"})
		time.Sleep(*stay)
		bot.report("after /server " + server)
		bot.chunkReport()
		if *runCmds != "" {
			for _, line := range strings.Split(*runCmds, ",") {
				bot.mu.Lock()
				before := len(bot.messages)
				bot.mu.Unlock()
				_ = c.WritePacket(&packet.CommandRequest{CommandLine: "/" + line, CommandOrigin: protocol.CommandOrigin{Origin: protocol.CommandOriginPlayer, UUID: [16]byte{1}}, Version: "latest"})
				time.Sleep(4 * time.Second)
				bot.mu.Lock()
				got := append([]string(nil), bot.messages[before:]...)
				bot.mu.Unlock()
				fmt.Printf("CMD /%s => %q\n", line, got)
			}
		}
		bot.colReport()
		if e := bot.err(); e != nil {
			fail("disconnected: %v", e)
		}
	}
	bot.mu.Lock()
	for k, n := range bot.mismatch {
		if strings.Contains(k, "oresplus") || strings.Contains(k, "UNKNOWN") {
			fmt.Println("ITEMCHECK", k, n)
		}
	}
	bot.mu.Unlock()
	_ = conn.Close()
	fmt.Println("RESULT ok")
}

func fail(f string, a ...any) {
	fmt.Printf("RESULT fail: "+f+"\n", a...)
	os.Exit(1)
}

type packDL struct {
	info *packet.ResourcePackDataInfo
	got  int
}

type bot struct {
	c   *proxy.ProxyConn
	log *slog.Logger

	mu         sync.Mutex
	counts     map[string]int
	spawned    bool
	runtimeID  uint64
	palette    []string // runtime id -> block name, from the client's StartGame
	itemNames  map[int32]string
	mismatch   map[string]int // item -> block its stacks point at, when the block isn't the item's own
	paletteErr error
	chunkNames map[string]int
	badIDs     int
	dims       []int32
	messages   []string
	disc       error
	packs      map[string]*packDL
	stack      bool
	// waitCol is the chunk column the bot must get terrain for before it says a dimension change
	// is done, as a real client does (it stays on the loading screen until then).
	waitCol   *[2]int32
	waitSince time.Time
	stuck     int
	// dimSeq counts dimension changes; chunkSeen[{dimSeq, x, z}] counts LevelChunks per column.
	dimSeq    int32
	chunkSeen map[[3]int32]int
	chunkDim  map[[3]int32]int32
	colNames  map[[3]int32]map[string]bool
}

// terrainArrived acknowledges a pending dimension change once the bot's own column has terrain.
func (b *bot) terrainArrived(x, z int32) {
	b.mu.Lock()
	if b.waitCol == nil || b.waitCol[0] != x || b.waitCol[1] != z {
		b.mu.Unlock()
		return
	}
	b.waitCol = nil
	rid := b.runtimeID
	b.mu.Unlock()
	go func() {
		time.Sleep(*doneDelay) // a slow client finishing its dimension change
		tl("-> dimension change done")
		_ = b.c.WritePacket(&packet.PlayerAction{EntityRuntimeID: rid, ActionType: protocol.PlayerActionDimensionChangeDone})
	}()
}

var runCmds = flag.String("cmds", "", "comma-separated commands to run (without /) on the last server, logging the replies")

var doneDelay = flag.Duration("donedelay", 0, "wait this long before confirming a dimension change")

func (b *bot) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.disc
}

func (b *bot) reset() {
	b.mu.Lock()
	b.counts = map[string]int{}
	b.chunkNames = map[string]int{}
	b.badIDs = 0
	b.dims = nil
	b.messages = nil
	b.mu.Unlock()
}

func (b *bot) waitSpawn(d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		b.mu.Lock()
		ok, e := b.spawned, b.disc
		b.mu.Unlock()
		if ok {
			return true
		}
		if e != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (b *bot) report(stage string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.counts))
	for k := range b.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, " %s=%d", k, b.counts[k])
	}
	type kv struct {
		k string
		v int
	}
	var top []kv
	for k, v := range b.chunkNames {
		top = append(top, kv{k, v})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].v > top[j].v })
	if len(top) > 8 {
		top = top[:8]
	}
	fmt.Printf("== %s\n packets:%s\n dimension changes: %v\n chunk blocks (top): %v\n palette ids outside the client palette: %d\n messages: %q\n",
		stage, sb.String(), b.dims, top, b.badIDs, b.messages)
}

func (b *bot) read() {
	pool := b.c.Pool()
	for {
		pks, err := b.c.ReadPackets()
		if err != nil {
			b.mu.Lock()
			if b.disc == nil {
				b.disc = err
			}
			b.mu.Unlock()
			return
		}
		for _, raw := range pks {
			buf := bytes.NewBuffer(raw)
			var h packet.Header
			if h.Read(buf) != nil {
				continue
			}
			f, ok := pool[h.PacketID]
			if !ok {
				continue
			}
			pk := f()
			if !decode(pk, buf.Bytes()) {
				b.mu.Lock()
				b.counts[fmt.Sprintf("UNDECODABLE_%T", pk)]++
				b.mu.Unlock()
				continue
			}
			b.handle(pk)
		}
	}
}

func decode(pk packet.Packet, payload []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	pk.Marshal(protocol.NewReader(bytes.NewBuffer(payload), 0, false))
	return true
}

func (b *bot) handle(pk packet.Packet) {
	b.mu.Lock()
	b.counts[strings.TrimPrefix(fmt.Sprintf("%T", pk), "*packet.")]++
	b.mu.Unlock()
	switch pk := pk.(type) {
	case *packet.ItemRegistry:
		if f, err := os.Create("/tmp/union_items.json"); err == nil {
			_ = json.NewEncoder(f).Encode(pk.Items)
			f.Close()
		}
		b.mu.Lock()
		b.itemNames = map[int32]string{}
		for _, it := range pk.Items {
			b.itemNames[int32(it.RuntimeID)] = it.Name
			if strings.Contains(it.Name, "amber") {
				fmt.Printf("REGITEM %s id=%d comp=%v data=%v\n", it.Name, it.RuntimeID, it.ComponentBased, it.Data)
			}
		}
		b.mu.Unlock()
	case *packet.InventoryContent:
		var names []string
		for _, it := range pk.Content {
			b.checkItem(it.Stack)
			if it.Stack.NetworkID != 0 {
				b.mu.Lock()
				blk := "-"
				if r := int(it.Stack.BlockRuntimeID); r > 0 && r < len(b.palette) {
					blk = b.palette[r]
				}
				names = append(names, fmt.Sprintf("%s#%d block=%s", b.itemNames[it.Stack.NetworkID], it.Stack.NetworkID, blk))
				b.mu.Unlock()
			}
		}
		if len(names) > 0 {
			fmt.Println("INVENTORY window", pk.WindowID, names)
		}
	case *packet.ContainerOpen:
		tl("container open window %d type %d", pk.WindowID, pk.ContainerType)
	case *packet.CommandOutput:
		for _, m := range pk.OutputMessages {
			fmt.Printf("CMDOUT success=%v %s %v\n", m.Success, m.Message, m.Parameters)
		}
	case *packet.InventorySlot:
		b.checkItem(pk.NewItem.Stack)
		if st := pk.NewItem.Stack; st.NetworkID != 0 {
			b.mu.Lock()
			fmt.Printf("SLOT window %d slot %d: %s block=%d\n", pk.WindowID, pk.Slot, b.itemNames[st.NetworkID], st.BlockRuntimeID)
			b.mu.Unlock()
		}
	case *packet.CreativeContent:
		for _, it := range pk.Items {
			b.checkItem(it.Item)
		}
	case *packet.NetworkStackLatency:
		if pk.NeedsResponse {
			_ = b.c.WritePacket(&packet.NetworkStackLatency{Timestamp: pk.Timestamp})
		}
	case *packet.Disconnect:
		b.mu.Lock()
		b.disc = fmt.Errorf("kicked: %s", pk.Message)
		b.mu.Unlock()
	case *packet.ResourcePacksInfo:
		var ids []string
		for _, t := range pk.TexturePacks {
			ids = append(ids, t.UUID.String()+"_"+t.Version)
		}
		b.log.Info("packs offered", "packs", ids)
		if len(ids) == 0 {
			_ = b.c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseAllPacksDownloaded})
			return
		}
		_ = b.c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseSendPacks, PacksToDownload: ids})
	case *packet.ResourcePackDataInfo:
		b.mu.Lock()
		b.packs[pk.UUID] = &packDL{info: pk}
		b.mu.Unlock()
		for i := uint32(0); i < pk.ChunkCount; i++ {
			_ = b.c.WritePacket(&packet.ResourcePackChunkRequest{UUID: pk.UUID, ChunkIndex: int32(i)})
		}
	case *packet.ResourcePackChunkData:
		b.mu.Lock()
		d := b.packs[pk.UUID]
		d.got++
		done := true
		for _, p := range b.packs {
			if uint32(p.got) < p.info.ChunkCount {
				done = false
			}
		}
		b.mu.Unlock()
		if done {
			b.log.Info("packs downloaded", "count", len(b.packs))
			_ = b.c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseAllPacksDownloaded})
		}
	case *packet.ResourcePackStack:
		var st []string
		for _, t := range pk.TexturePacks {
			st = append(st, t.UUID+"_"+t.Version)
		}
		fmt.Println("PACK STACK", st)
		_ = b.c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseCompleted})
	case *packet.StartGame:
		if f, err := os.Create("/tmp/netbot_startgame_blocks.json"); err == nil {
			_ = json.NewEncoder(f).Encode(pk.Blocks)
			f.Close()
		}
		names, err := bridge.PaletteNames(pk.Blocks)
		b.mu.Lock()
		b.runtimeID = pk.EntityRuntimeID
		b.palette, b.paletteErr = names, err
		b.mu.Unlock()
		b.log.Info("start game", "runtimeID", pk.EntityRuntimeID, "customBlocks", len(pk.Blocks), "paletteSize", len(names), "experiments", len(pk.Experiments))
		_ = b.c.WritePacket(&packet.RequestChunkRadius{ChunkRadius: 8, MaxChunkRadius: 8})
	case *packet.PlayStatus:
		if pk.Status == packet.PlayStatusPlayerSpawn {
			b.mu.Lock()
			first := !b.spawned
			b.spawned = true
			rid := b.runtimeID
			b.mu.Unlock()
			if first {
				_ = b.c.WritePacket(&packet.SetLocalPlayerAsInitialised{EntityRuntimeID: rid})
			}
		}
	case *packet.ChangeDimension:
		b.mu.Lock()
		b.dimSeq++
		tl("changedimension %d at %v", pk.Dimension, pk.Position)
		fmt.Printf("DIMCHANGE %d: dimension %d at %v\n", b.dimSeq, pk.Dimension, pk.Position)
		b.mu.Unlock()
		col := [2]int32{int32(math.Floor(float64(pk.Position[0]))) >> 4, int32(math.Floor(float64(pk.Position[2]))) >> 4}
		b.mu.Lock()
		b.dims = append(b.dims, pk.Dimension)
		b.waitCol, b.waitSince = &col, time.Now()
		b.mu.Unlock()
		go func(want *[2]int32) {
			time.Sleep(20 * time.Second)
			b.mu.Lock()
			if b.waitCol == want {
				b.stuck++
				b.messages = append(b.messages, fmt.Sprintf("STUCK: no terrain at column %v 20 s after a dimension change", *want))
			}
			b.mu.Unlock()
		}(&col)
		b.mu.Lock()
		b.waitCol = &col
		b.mu.Unlock()
	case *packet.AvailableCommands:
		// Every command list the server sends, numbered, for comparing what reaches a client.
		b.mu.Lock()
		b.counts["cmdlists"]++
		n := b.counts["cmdlists"]
		b.mu.Unlock()
		if f, err := os.Create(fmt.Sprintf("/tmp/netbot_cmds_%d.json", n)); err == nil {
			_ = json.NewEncoder(f).Encode(pk)
			f.Close()
		}
		log.Printf("command list %d: %d commands, %d enums, %d enum values", n, len(pk.Commands), len(pk.Enums), len(pk.EnumValues))
	case *packet.Text:
		b.mu.Lock()
		b.messages = append(b.messages, pk.Message)
		b.mu.Unlock()
	case *packet.SubChunk:
		x, z := pk.Position[0], pk.Position[2]
		for _, e := range pk.SubChunkEntries {
			if e.Result == protocol.SubChunkResultSuccess || e.Result == protocol.SubChunkResultSuccessAllAir {
				b.terrainArrived(x+int32(e.Offset[0]), z+int32(e.Offset[2]))
			}
		}
	case *packet.NetworkChunkPublisherUpdate:
		b.mu.Lock()
		tl("publisher %v r%d", pk.Position, pk.Radius)
		fmt.Printf("PUBLISHER after dimension change %d: center %v radius %d\n", b.dimSeq, pk.Position, pk.Radius)
		b.mu.Unlock()
	case *packet.Respawn:
		tl("respawn state %d pos %v", pk.State, pk.Position)
		// Dead (death screen): press respawn, as a client does.
		if pk.State == packet.RespawnStateSearchingForSpawn || pk.State == packet.RespawnStateReadyToSpawn {
			b.mu.Lock()
			rid := b.runtimeID
			b.mu.Unlock()
			_ = b.c.WritePacket(&packet.Respawn{State: packet.RespawnStateClientReadyToSpawn, EntityRuntimeID: rid})
			log.Printf("respawn requested (state %d)", pk.State)
		}
	case *packet.LevelChunk:
		if pk.Position[0] >= -1 && pk.Position[0] <= 1 && pk.Position[1] >= -1 && pk.Position[1] <= 1 {
			tl("chunk %v dim %d subchunks %d", pk.Position, pk.Dimension, pk.SubChunkCount)
			_ = os.WriteFile(fmt.Sprintf("/tmp/chunkdump_%d_%d.bin", pk.Position[0], pk.Position[1]), pk.RawPayload, 0o644)
		}
		b.mu.Lock()
		if b.chunkSeen == nil {
			b.chunkSeen, b.chunkDim = map[[3]int32]int{}, map[[3]int32]int32{}
		}
		k := [3]int32{b.dimSeq, pk.Position[0], pk.Position[1]}
		b.chunkSeen[k]++
		b.chunkDim[k] = pk.Dimension
		b.mu.Unlock()
		if _, req := pk.SubChunkLimit.Value(); !req {
			b.terrainArrived(pk.Position[0], pk.Position[1])
		}
		if _, req := pk.SubChunkLimit.Value(); req {
			// As a real client does: ask for the chunk's sub-chunks.
			_ = b.c.WritePacket(&packet.SubChunkRequest{
				Dimension: pk.Dimension,
				Position:  protocol.SubChunkPos{pk.Position[0], 0, pk.Position[1]},
				Offsets:   []protocol.SubChunkOffset{{0, -4, 0}, {0, 0, 0}, {0, 4, 0}},
			})
			return
		}
		if pk.SubChunkCount == 0 {
			return
		}
		if bes, beErr := chunkBlockEntities(pk.RawPayload, int(pk.SubChunkCount)); pk.Position[0] >= -2 && pk.Position[0] <= 1 && pk.Position[1] >= -2 && pk.Position[1] <= 1 {
			fmt.Printf("BE chunk %v dimchange %d: %d block entities, err=%v %v\n", pk.Position, b.dimSeq, len(bes), beErr, bes)
		}
		ids, err := bridge.ChunkPaletteIDs(pk.RawPayload, int(pk.SubChunkCount))
		b.mu.Lock()
		defer b.mu.Unlock()
		if err != nil {
			b.counts["CHUNK_PARSE_ERROR"]++
			return
		}
		if b.chunkNames == nil {
			b.chunkNames = map[string]int{}
		}
		if b.colNames == nil {
			b.colNames = map[[3]int32]map[string]bool{}
		}
		ck := [3]int32{b.dimSeq, pk.Position[0], pk.Position[1]}
		if b.colNames[ck] == nil {
			b.colNames[ck] = map[string]bool{}
		}
		for _, id := range ids {
			if int(id) >= len(b.palette) {
				b.badIDs++
				b.colNames[ck][fmt.Sprintf("BAD_ID_%d", id)] = true
				continue
			}
			b.chunkNames[b.palette[id]]++
			b.colNames[ck][b.palette[id]] = true
		}
	}
}

var _ = nethernet.Identity{}

// checkItem records custom items whose stacks point at a block other than their own, and items
// the client's item list doesn't have.
func (b *bot) checkItem(st protocol.ItemStack) {
	if st.NetworkID == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mismatch == nil {
		b.mismatch = map[string]int{}
	}
	name, ok := b.itemNames[st.NetworkID]
	if !ok {
		b.mismatch["UNKNOWN ITEM"]++
		return
	}
	if strings.HasPrefix(name, "minecraft:") {
		return
	}
	blkName := "none"
	if st.BlockRuntimeID != 0 && int(st.BlockRuntimeID) < len(b.palette) {
		blkName = b.palette[st.BlockRuntimeID]
	}
	b.mismatch["seen "+name+" block="+blkName]++
	if st.BlockRuntimeID == 0 {
		for _, n := range b.palette {
			if n == name {
				b.mismatch[name+" -> NO BLOCK"]++
				break
			}
		}
		return
	}
	if int(st.BlockRuntimeID) >= len(b.palette) {
		return
	}
	if blk := b.palette[st.BlockRuntimeID]; blk != name {
		b.mismatch[name+" -> "+blk]++
	}
}

// chunkReport prints, for the last dimension the bot ended in, which columns around the spawn
// (-4..3 on both axes) never got a LevelChunk.
func (b *bot) chunkReport() {
	b.mu.Lock()
	defer b.mu.Unlock()
	var missing []string
	got := 0
	for x := int32(-4); x <= 3; x++ {
		for z := int32(-4); z <= 3; z++ {
			if b.chunkSeen[[3]int32{b.dimSeq, x, z}] > 0 {
				got++
			} else {
				missing = append(missing, fmt.Sprintf("(%d,%d)", x, z))
			}
		}
	}
	early := 0
	for k := range b.chunkSeen {
		if k[0] < b.dimSeq {
			early++
		}
	}
	fmt.Printf("CHUNKS after dimension change %d: %d/64 around spawn; missing %v; columns received before it: %d\n", b.dimSeq, got, missing, early)
}

// colReport writes every block name in each column around spawn (final dimension) to
// /tmp/netbot_cols.txt.
func (b *bot) colReport() {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out strings.Builder
	for x := int32(-2); x <= 1; x++ {
		for z := int32(-2); z <= 1; z++ {
			names := b.colNames[[3]int32{b.dimSeq, x, z}]
			list := make([]string, 0, len(names))
			for n := range names {
				list = append(list, n)
			}
			sort.Strings(list)
			fmt.Fprintf(&out, "chunk (%d,%d) %d kinds: %s\n", x, z, len(list), strings.Join(list, " "))
		}
	}
	_ = os.WriteFile("/tmp/netbot_cols.txt", []byte(out.String()), 0o644)
}

// tl prints a timestamped timeline line (chunk 0,0 hole diagnosis).
func tl(f string, a ...any) {
	fmt.Printf("TL %s "+f+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}
