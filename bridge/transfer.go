package bridge

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Moving a player to another server without a reconnect, ported from WaterdogPE
// (SwitchDownstreamHandler, TransferCallback, PlayerRewriteUtils). The client can't be sent a second
// StartGame, so the relay logs the player into the new server itself, then:
//
//  1. Undoes what the old server gave the client (entities, players, boss bars, scoreboards, effects,
//     open windows, fog, input locks, hidden HUD, weather) and applies the new server's game mode,
//     difficulty, game rules and time.
//  2. Sends the client through two dimension changes (Bedrock only rebuilds its world on one, and
//     refuses one into the dimension it is in): first to another dimension, with empty chunks, then
//     into the new server's dimension. The new server's packets wait in a queue until the first change
//     is done, then flow; the second change finishes on the client's acknowledgement.
//  3. Tells the new server the player is initialised once it spawned them.

const transferTimeout = 60 * time.Second

type transfer struct {
	start    time.Time
	answered time.Duration // until the new server's StartGame
	phase1   time.Duration // until the client left the old world
	phase2   time.Duration // until it was in the new one
	l        *link
	target   int32 // the new server's dimension
	spawn    mgl32.Vec3
	rot      mgl32.Vec2

	mu         sync.Mutex
	phase      int // 1: waiting for the first dimension change, 2: for the second, 0: done
	queueing   bool
	queue      [][]byte
	wired      bool // the client's packets go to the new server
	playStatus bool
	finalized  bool
	timer      *time.Timer
	// ground is closed once the server's own sub-chunks for the spawn column have passed, on a
	// server that sends them on request (nil otherwise): see phase2Done.
	ground chan struct{}
	// replay holds the newest real chunk for each column the placeholders cover (around the spawn),
	// as it came from the new server during the move. Clients ignore chunks that arrive while they
	// are still changing dimension, so the placeholder could stay (an invisible, solid hole at the
	// spawn); they are sent again once the client is in the new world.
	replay map[[2]int32][]byte
}

const (
	dimOverworld = 0
	dimNether    = 1
	dimEnd       = 2
)

// otherDimension is the dimension to pass through: the target itself when the client is elsewhere.
func otherDimension(from, to int32) int32 {
	if from == to {
		if from == dimOverworld {
			return dimNether
		}
		return dimOverworld
	}
	return to
}

// switchTo moves the player to server. reason is why their old server sent them away, if it did.
func (s *switchSession) switchTo(server ServerConfig, reason string) {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	s.mu.Lock()
	busy := s.transfer != nil
	already := reason == "" && s.cur != nil && strings.EqualFold(s.cur.server.Name, server.Name)
	s.mu.Unlock()
	if busy || already {
		return // a second /server while the first move runs, or one to where the player already is
	}
	start := time.Now()
	if reason != "" {
		s.message("<red>%s</red>", reason)
	}
	s.message("<grey>Connecting you to %s...</grey>", server.Name)
	l, err := s.openLink(server)
	if err == nil {
		select {
		case <-l.started:
		case <-l.closed:
			err = l.err
		case <-time.After(backendDialTimeout):
			l.close()
			err = fmt.Errorf("no answer")
		case <-s.ctx.Done():
			l.close()
			return
		}
	}
	if err != nil {
		s.log.Warn("network: move failed", "server", server.Name, "err", err)
		s.mu.Lock()
		gone := s.cur == nil || isClosed(s.cur)
		s.mu.Unlock()
		if gone {
			s.disconnect(fmt.Sprintf("Could not connect to %s: %s", server.Name, errText(err)))
			return
		}
		s.message("<red>Could not connect to %s: %s</red>", server.Name, errText(err))
		return
	}
	sg := l.startGame
	answered := time.Since(start)
	s.mu.Lock()
	old := s.cur
	if old != nil {
		if bad := incompatible(old.startGame, sg); bad != "" {
			s.mu.Unlock()
			l.close()
			s.log.Warn("network: move refused, the servers' StartGame settings differ", "server", server.Name, "differences", bad)
			s.message("<red>Can't move you to %s: its settings are incompatible.</red>", server.Name)
			return
		}
	}
	bp, err := newBlockPalette(sg.Blocks)
	if err != nil {
		s.mu.Unlock()
		l.close()
		s.message("<red>Could not connect to %s: %s</red>", server.Name, err)
		return
	}
	var bm *blockMap
	if !bp.sameAs(s.palette) {
		bm = newBlockMap(bp, s.palette)
		for _, name := range bp.custom {
			if _, ok := s.palette.index[blockState{Name: name, Properties: encodeProperties(nil)}]; !ok && !s.paletteHas(name) {
				s.log.Warn("network: the new server has a custom block this player's client doesn't know, it shows as air", "server", server.Name, "block", name)
			}
		}
	}
	l.rw = newRewriter(sg.EntityRuntimeID, sg.EntityUniqueID, s.clientRID, s.clientUID, bm)
	s.setItems(l)
	t := &transfer{start: start, answered: answered, l: l, target: sg.Dimension, spawn: sg.PlayerPosition, rot: mgl32.Vec2{sg.Pitch, sg.Yaw}, phase: 1, queueing: true}
	s.transfer = t
	s.cur = l
	if s.n.usesRequestMode(server.Name) {
		s.settleLocked(sg.PlayerPosition) // see settle.go
	} else {
		s.settling.Store(false)
	}
	s.forgetColumns()
	var injects []packet.Packet
	injects = append(injects, s.resetClientLocked(sg)...)
	newDim := otherDimension(s.dimension, sg.Dimension)
	s.dimension = newDim
	s.mu.Unlock()

	if old != nil {
		old.close()
	}
	s.log.Info("network: moving player", "from", nameOf(old), "to", server.Name, "blocksRenumbered", bm != nil)
	// No backend leg in /ping: the servers are on this machine, and the NetherNet reading of
	// that hop is wrong (around 46 ms on loopback). ms is then the player's own link.
	go s.readBackend(l, false)
	_ = l.c.WritePacket(&packet.RequestChunkRadius{ChunkRadius: s.chunkRadius, MaxChunkRadius: s.maxRadius})

	t.timer = time.AfterFunc(transferTimeout, func() { t.fail(s, "timed out") })
	s.sendClient(injects...)
	if newDim != sg.Dimension {
		fake := sg.PlayerPosition.Add(mgl32.Vec3{2000, 0, 2000})
		s.sendClient(s.position(fake, t.rot))
		s.sendClient(s.dimensionChange(newDim, fake, true, s.subChunkMode)...)
	} else {
		// The client is in another dimension already: one change, straight into the target.
		s.sendClient(s.position(sg.PlayerPosition, t.rot))
		s.sendClient(s.dimensionChange(newDim, sg.PlayerPosition, false, false)...)
		t.dimChangeDone(s)
	}
}

func nameOf(l *link) string {
	if l == nil {
		return ""
	}
	return l.server.Name
}

func isClosed(l *link) bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

func (s *switchSession) paletteHas(name string) bool {
	for _, c := range s.palette.custom {
		if c == name {
			return true
		}
	}
	return false
}

// incompatible lists the StartGame settings a client locks at its first join that two servers
// disagree on (WaterdogPE's StartGameSettings).
func incompatible(a, b *packet.StartGame) string {
	if a == nil || b == nil {
		return ""
	}
	var d []string
	if a.PlayerMovementSettings.RewindHistorySize != b.PlayerMovementSettings.RewindHistorySize {
		d = append(d, "rewind history size")
	}
	if a.PlayerMovementSettings.ServerAuthoritativeBlockBreaking != b.PlayerMovementSettings.ServerAuthoritativeBlockBreaking {
		d = append(d, "server authoritative block breaking")
	}
	if a.ServerAuthoritativeInventory != b.ServerAuthoritativeInventory {
		d = append(d, "server authoritative inventory")
	}
	if a.UseBlockNetworkIDHashes != b.UseBlockNetworkIDHashes {
		d = append(d, "block network id hashes")
	}
	return strings.Join(d, ", ")
}

// resetClientLocked undoes the old server's state on the client and applies the new server's
// StartGame settings. s.mu held.
func (s *switchSession) resetClientLocked(sg *packet.StartGame) []packet.Packet {
	var out []packet.Packet
	if s.dead {
		s.dead = false
		s.proxyRespawn = true
		out = append(out, &packet.Respawn{Position: sg.PlayerPosition, State: packet.RespawnStateReadyToSpawn, EntityRuntimeID: s.clientRID})
	}
	for ridden, rider := range s.links {
		out = append(out, &packet.SetActorLink{EntityLink: protocol.EntityLink{RiddenEntityUniqueID: ridden, RiderEntityUniqueID: rider, Type: protocol.EntityLinkRemove}})
	}
	clear(s.links)
	for id := range s.bossBars {
		out = append(out, &packet.BossEvent{BossEntityUniqueID: id, EventType: packet.BossEventHide})
	}
	clear(s.bossBars)
	if len(s.waypoints) > 0 {
		// Locator bar waypoints stay on the client until removed, pointing at players of the old server.
		lb := &packet.LocatorBar{}
		for id := range s.waypoints {
			lb.Waypoints = append(lb.Waypoints, protocol.LocatorBarWaypoint{GroupHandle: id, Action: protocol.WaypointActionRemove})
		}
		out = append(out, lb)
		clear(s.waypoints)
	}
	if len(s.players) > 0 {
		pl := &packet.PlayerList{}
		for id := range s.players {
			pl.Entries = append(pl.Entries, protocol.PlayerListEntry{ActionType: protocol.PlayerListActionRemove, UUID: id})
		}
		out = append(out, pl)
		clear(s.players)
	}
	for id := range s.entities {
		if id != s.clientUID {
			out = append(out, &packet.RemoveActor{EntityUniqueID: id})
		}
	}
	clear(s.entities)
	if len(s.scores) > 0 {
		sc := &packet.SetScore{}
		for _, e := range s.scores {
			sc.Entries = append(sc.Entries, protocol.ScoreboardEntry{EntryID: e.EntryID, ObjectiveName: e.ObjectiveName, IdentityType: protocol.ScoreboardIdentityRemove})
		}
		out = append(out, sc)
		clear(s.scores)
	}
	for name := range s.objectives {
		out = append(out, &packet.RemoveObjective{ObjectiveName: name})
	}
	clear(s.objectives)
	if s.fog {
		out = append(out, &packet.PlayerFog{})
		s.fog = false
	}
	if s.inputLocks != 0 {
		out = append(out, &packet.UpdateClientInputLocks{})
		s.inputLocks = 0
	}
	if len(s.hiddenHud) > 0 {
		h := &packet.SetHud{Visibility: packet.HudVisibilityReset}
		for e := range s.hiddenHud {
			h.Elements = append(h.Elements, e)
		}
		out = append(out, h)
		clear(s.hiddenHud)
	}
	for id, typ := range s.containers {
		out = append(out, &packet.ContainerClose{WindowID: id, ContainerType: typ, ServerSide: true})
	}
	clear(s.containers)
	// ContainerClose can't close the player's own inventory: being put to sleep closes everything.
	out = append(out, s.flags(map[uint32]bool{protocol.EntityDataFlagSleeping: true}))
	for e := int32(0); e <= 37; e++ {
		out = append(out, &packet.MobEffect{EntityRuntimeID: s.clientRID, Operation: packet.MobEffectRemove, EffectType: e})
	}
	out = append(out, &packet.SetActorData{EntityRuntimeID: s.clientRID, EntityMetadata: protocol.EntityMetadata{
		protocol.EntityDataKeyEffectAmbience: byte(0), protocol.EntityDataKeyEffectColor: int32(0), protocol.EntityDataKeyAuxValueData: int16(0),
	}})
	out = append(out,
		&packet.LevelEvent{EventType: packet.LevelEventStopThunderstorm},
		&packet.LevelEvent{EventType: packet.LevelEventStopRaining, EventData: 10000},
	)
	mode := sg.PlayerGameMode
	if mode == packet.GameTypeDefault {
		mode = sg.WorldGameMode
	}
	out = append(out,
		&packet.SetPlayerGameType{GameType: mode},
		&packet.SetDifficulty{Difficulty: uint32(sg.Difficulty)},
		&packet.GameRulesChanged{GameRules: sg.GameRules},
		&packet.SetTime{Time: int32(sg.Time)},
	)
	return out
}

// flags is a SetActorData for the player that sets the given entity flags (all others off).
func (s *switchSession) flags(set map[uint32]bool) packet.Packet {
	var one, two int64
	for f, on := range set {
		if !on {
			continue
		}
		if f < 64 {
			one |= 1 << f
		} else {
			two |= 1 << (f - 64)
		}
	}
	return &packet.SetActorData{EntityRuntimeID: s.clientRID, EntityMetadata: protocol.EntityMetadata{
		protocol.EntityDataKeyFlags: one, protocol.EntityDataKeyFlagsTwo: two,
	}}
}

func (s *switchSession) position(pos mgl32.Vec3, rot mgl32.Vec2) packet.Packet {
	return &packet.MovePlayer{EntityRuntimeID: s.clientRID, Position: pos, Pitch: rot[0], Yaw: rot[1], HeadYaw: rot[1], Mode: packet.MoveModeReset}
}

// dimensionChange is a dimension change, with empty chunks around pos if chunks is set, and the
// server-sent acknowledgement newer clients wait for.
func (s *switchSession) dimensionChange(dim int32, pos mgl32.Vec3, chunks, requestMode bool) []packet.Packet {
	out := []packet.Packet{
		&packet.ChangeDimension{Dimension: dim, Position: pos, Respawn: true},
		&packet.StopSound{StopAll: true},
	}
	if chunks {
		cx, cz := int32(math.Floor(float64(pos[0])))>>4, int32(math.Floor(float64(pos[2])))>>4
		out = append(out, &packet.NetworkChunkPublisherUpdate{Position: protocol.BlockPos{int32(pos[0]), int32(pos[1]), int32(pos[2])}, Radius: 3 << 4})
		for x := int32(-3); x <= 3; x++ {
			for z := int32(-3); z <= 3; z++ {
				out = append(out, emptyChunk(cx+x, cz+z, dim, requestMode))
			}
		}
	}
	out = append(out, &packet.PlayerAction{EntityRuntimeID: s.clientRID, ActionType: protocol.PlayerActionDimensionChangeDone})
	return out
}

// emptyChunk is an empty chunk, in sub-chunk request mode if requestMode (the mode of the server
// the client is about to get chunks from).
func emptyChunk(x, z, dim int32, requestMode bool) packet.Packet {
	pk := &packet.LevelChunk{Position: protocol.ChunkPos{x, z}, Dimension: dim}
	if requestMode {
		limit := int32(23)
		switch dim {
		case dimNether:
			limit = 7
		case dimEnd:
			limit = 15
		}
		pk.SubChunkLimit = protocol.Option(limit)
		return pk
	}
	pk.SubChunkCount = 1
	pk.RawPayload = emptyChunkData(dim)
	return pk
}

// emptyChunkData is one empty sub-chunk, air biomes for every section of the dimension and no border
// blocks: the smallest chunk clients accept (WaterdogPE's fake chunk).
func emptyChunkData(dim int32) []byte {
	sections := 24
	switch dim {
	case dimNether:
		sections = 8
	case dimEnd:
		sections = 16
	}
	b := bytes.NewBuffer(nil)
	b.WriteByte(8) // sub-chunk version
	b.WriteByte(0) // no storages
	b.WriteByte(1<<1 | 1)
	b.Write(make([]byte, 512))
	_ = protocol.WriteVarint32(b, 1)
	_ = protocol.WriteVarint32(b, 0)
	for i := 1; i < sections; i++ {
		b.WriteByte(127<<1 | 1) // same as the previous section
	}
	b.WriteByte(0) // border blocks
	return b.Bytes()
}

func airSubChunks(req *packet.SubChunkRequest) packet.Packet {
	pk := &packet.SubChunk{Dimension: req.Dimension, Position: req.Position}
	offsets := req.Offsets
	if len(offsets) == 0 {
		offsets = []protocol.SubChunkOffset{{0, 0, 0}}
	}
	for _, o := range offsets {
		pk.SubChunkEntries = append(pk.SubChunkEntries, protocol.SubChunkEntry{
			Offset: o, Result: protocol.SubChunkResultSuccessAllAir,
			HeightMapType: protocol.HeightMapDataHasData, HeightMapData: protocol.Option(protocol.HeightMap{}),
			RenderHeightMapType: protocol.HeightMapDataNone,
		})
	}
	return pk
}

// dimChangeDone handles the client finishing a dimension change. It reports whether the
// acknowledgement belonged to the move (and must not reach the server).
func (t *transfer) dimChangeDone(s *switchSession) bool {
	t.mu.Lock()
	switch t.phase {
	case 1:
		t.phase = 2
		t.phase1 = time.Since(t.start)
		t.mu.Unlock()
		t.phase1Done(s)
		return true
	case 2:
		t.phase = 0
		t.phase2 = time.Since(t.start)
		t.mu.Unlock()
		t.phase2Done(s)
		return true
	}
	t.mu.Unlock()
	return false
}

func (t *transfer) inPhase1() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase == 1
}

// phase1Done: the client is out of the old world. Freeze it, take it into the target dimension and
// let the new server's packets through.
func (t *transfer) phase1Done(s *switchSession) {
	s.sendClient(s.flags(map[uint32]bool{protocol.EntityDataFlagNoAI: true, protocol.EntityDataFlagBreathing: true, protocol.EntityDataFlagHasGravity: true}))
	s.sendClient(&packet.UpdateClientInputLocks{Locks: inputLockCamera | inputLockMovement})
	s.mu.Lock()
	cur := s.dimension
	s.mu.Unlock()
	if cur != t.target {
		fake := t.spawn.Add(mgl32.Vec3{-2000, 0, -2000})
		s.sendClient(s.position(fake, t.rot))
		s.mu.Lock()
		s.dimension = otherDimension(cur, t.target)
		dim := s.dimension
		s.mu.Unlock()
		s.sendClient(s.dimensionChange(dim, t.spawn, true, s.n.usesRequestMode(t.l.server.Name))...)
	}
	s.mu.Lock()
	s.placeholders = nil
	if cur != t.target && s.n.usesRequestMode(t.l.server.Name) {
		s.markPlaceholders(t.spawn) // see placeholders.go
	}
	if s.n.usesRequestMode(t.l.server.Name) {
		t.ground = s.awaitSpawnColumnLocked(t.spawn) // see phase2Done
	}
	s.subChunkMode = s.n.usesRequestMode(t.l.server.Name)
	t.mu.Lock()
	q := t.queue
	t.queue, t.queueing, t.wired = nil, false, true
	t.mu.Unlock()
	for _, raw := range q {
		s.track(raw)
		t.rememberChunk(raw)
	}
	s.mu.Unlock()
	if len(q) > 0 {
		_ = s.writeClient(q...)
	}
}

// spawnGroundWait is the longest a player stays frozen at the spawn waiting for the ground there.
const spawnGroundWait = 8 * time.Second

// phase2Done: the client is in the new world. Put it at the spawn and unfreeze it.
//
// On a server that sends sub-chunks on request the ground at the spawn is not there yet: the
// client first gets the relay's own air for those columns (placeholders.go), and the server's
// blocks only after it asked for them. Unfrozen before that, the player fell through the spawn
// and kept falling, and the server followed them down: they were back in the void below the
// world with no health, for good (their position is saved). A server that had just started
// answers slowly enough for that. So the player stays frozen until the server's blocks for the
// spawn column have passed, or spawnGroundWait at most.
func (t *transfer) phase2Done(s *switchSession) {
	s.sendClient(&packet.StopSound{StopAll: true})
	s.sendClient(s.position(t.spawn, t.rot))
	unfreeze := func() {
		s.sendClient(s.flags(map[uint32]bool{protocol.EntityDataFlagBreathing: true, protocol.EntityDataFlagHasGravity: true}))
		s.sendClient(&packet.UpdateClientInputLocks{})
	}
	if t.ground == nil {
		unfreeze()
	} else {
		go func() {
			start := time.Now()
			timedOut := false
			select {
			case <-t.ground:
			case <-time.After(spawnGroundWait):
				timedOut = true
			case <-s.ctx.Done():
				return
			}
			s.mu.Lock()
			still := s.cur == t.l
			s.mu.Unlock()
			if !still {
				return // moved on to another server meanwhile: that move unfreezes
			}
			if waited := time.Since(start); timedOut || waited > time.Second {
				s.log.Info("network: waited for the ground at the spawn", "server", t.l.server.Name, "waited", waited.Round(time.Millisecond), "timedOut", timedOut)
			}
			s.sendClient(s.position(t.spawn, t.rot))
			unfreeze()
		}()
	}
	t.replayChunks(s)
	s.mu.Lock()
	s.armSettle(t.l, t.spawn) // and again once the client settled: see dimreplay.go
	s.mu.Unlock()
	t.tryFinalize(s)
}

func (t *transfer) onPlayStatus(s *switchSession) {
	t.mu.Lock()
	t.playStatus = true
	t.mu.Unlock()
	// Tell the server now, not once the client is out of its loading screen (as WaterdogPE does):
	// Dragonfly only puts the player in the world and sends chunks after this, and the client only
	// leaves the loading screen once it has chunks. Waiting deadlocked until Dragonfly gave up.
	_ = t.l.c.WritePacket(&packet.SetLocalPlayerAsInitialised{EntityRuntimeID: t.l.startGame.EntityRuntimeID})
	t.tryFinalize(s)
}

// tryFinalize ends the move once both the dimension changes are done and the server spawned the
// player: the server is told the player is initialised.
func (t *transfer) tryFinalize(s *switchSession) {
	t.mu.Lock()
	if t.finalized || !t.playStatus || t.phase != 0 {
		t.mu.Unlock()
		return
	}
	t.finalized = true
	if t.timer != nil {
		t.timer.Stop()
	}
	t.mu.Unlock()
	s.mu.Lock()
	if s.transfer == t {
		s.transfer = nil
	}
	s.mu.Unlock()
	time.AfterFunc(5*time.Second, func() {
		s.mu.Lock()
		n, kept := s.subServed, len(s.columns)
		s.subServed = 0
		s.mu.Unlock()
		if n > 0 {
			s.log.Info("network: sub-chunk requests answered from the server's whole columns", "server", t.l.server.Name, "requests", n, "columnsKept", kept)
		}
	})
	s.log.Info("network: move finished", "server", t.l.server.Name, "serverAnswered", t.answered.Round(time.Millisecond),
		"leftOldWorld", t.phase1.Round(time.Millisecond), "inNewWorld", t.phase2.Round(time.Millisecond),
		"total", time.Since(t.start).Round(time.Millisecond))
}

// fail gives up on a move that already took the player off their old server: there is nothing to go
// back to, so the player is disconnected.
func (t *transfer) fail(s *switchSession, reason string) {
	t.mu.Lock()
	if t.finalized {
		t.mu.Unlock()
		return
	}
	t.finalized = true
	t.mu.Unlock()
	s.log.Warn("network: move failed midway", "server", t.l.server.Name, "reason", reason)
	t.l.close()
	s.disconnect(fmt.Sprintf("Could not move you to %s: %s", t.l.server.Name, reason))
}

const (
	inputLockCamera   = 1 << 1
	inputLockMovement = 1 << 2
)

var _ = uuid.Nil

const (
	// actorEventRespawn finishes a respawn on the client (ActorEvent 18).
	actorEventRespawn = 18
)

// rememberChunk keeps raw if it is a LevelChunk for a column the move's placeholders cover.
func (t *transfer) rememberChunk(raw []byte) {
	id, payload, err := splitPacket(raw)
	if err != nil || id != packet.IDLevelChunk {
		return
	}
	var pk packet.LevelChunk
	if decodeInto(&pk, payload) != nil {
		return
	}
	cx, cz := int32(math.Floor(float64(t.spawn[0])))>>4, int32(math.Floor(float64(t.spawn[2])))>>4
	dx, dz := pk.Position[0]-cx, pk.Position[1]-cz
	if dx < -3 || dx > 3 || dz < -3 || dz > 3 {
		return
	}
	t.mu.Lock()
	if t.replay == nil {
		t.replay = map[[2]int32][]byte{}
	}
	t.replay[[2]int32{pk.Position[0], pk.Position[1]}] = append([]byte(nil), raw...)
	t.mu.Unlock()
}

// replayChunks sends the remembered chunks again and forgets them.
func (t *transfer) replayChunks(s *switchSession) {
	t.mu.Lock()
	r := t.replay
	t.replay = nil
	t.mu.Unlock()
	if len(r) == 0 {
		return
	}
	pks := make([][]byte, 0, len(r))
	for _, raw := range r {
		pks = append(pks, raw)
	}
	_ = s.writeClient(pks...)
	s.log.Debug("network: chunks around the spawn sent again after the move", "chunks", len(pks))
}

func (t *transfer) inPhase2() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase == 2
}
