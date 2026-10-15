package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Rewriting packets between a backend and a client that first joined a different backend. Two
// things can differ:
//
//   - The player's own entity ids. The client keeps the ids of the first server's StartGame, so on
//     any other server the player's ids and the ids the client knows itself by are swapped in every
//     packet, both ways (as WaterdogPE does): the backend's own id becomes the client's, and an
//     entity the backend numbers like the client's own id gets the backend's own id instead.
//   - Block runtime ids, when the backend's custom blocks differ from the client's palette (see
//     palette.go): chunk palettes, block updates, block particles and sounds, and the block of every
//     item stack.
//
// A packet that holds none of these passes through as raw bytes; only the packets that might are
// decoded, changed and encoded again.

// rewriter holds what one backend link translates. It is used by both pumps, read-only after the link
// is set up, except falling which has its own lock.
type rewriter struct {
	swapEntities bool
	rid          [2]uint64 // the backend's own runtime id, the client's
	uid          [2]int64  // the same for unique ids

	blocks *blockMap // nil when the backend numbers blocks like the client
	// items renumbers item network ids (nil: the backend numbers items like the client). Set
	// when the backend's ItemRegistry arrives, read by both pumps.
	items atomic.Pointer[itemMap]
	// blockItems is the client's custom block items (client item id -> the block they place, a
	// client runtime id). A server that numbers that item as a plain item (Geyser's Ores Plus
	// ores) sends its stacks without a block, which crashes the client drawing them.
	blockItems map[int32]uint32

	fallingMu sync.Mutex
	falling   map[uint64]bool // runtime ids (client side) of falling blocks, whose variant is a block
}

func newRewriter(backendRID uint64, backendUID int64, clientRID uint64, clientUID int64, blocks *blockMap) *rewriter {
	return &rewriter{
		swapEntities: backendRID != clientRID || backendUID != clientUID,
		rid:          [2]uint64{backendRID, clientRID},
		uid:          [2]int64{backendUID, clientUID},
		blocks:       blocks,
		falling:      map[uint64]bool{},
	}
}

// identity reports whether the rewriter changes nothing.
func (r *rewriter) identity() bool {
	return !r.swapEntities && r.blocks == nil && r.items.Load() == nil
}

// mode is what a packet may need rewritten for: modeBlocks and/or modeItems on top of entity ids.
func (r *rewriter) mode() int {
	m := 0
	if r.blocks != nil {
		m |= modeBlocks
	}
	if r.items.Load() != nil {
		m |= modeItems
	}
	return m
}

const (
	modeBlocks = 1 << iota
	modeItems
)

// itemMap renumbers item network ids between a backend and the client.
type itemMap struct {
	to, back map[int32]int32
}

// newItemMap maps a backend's items onto the client's by name; nil when they are numbered alike.
// An item the client doesn't know becomes air.
func newItemMap(backend []protocol.ItemEntry, client map[string]int32) *itemMap {
	m := &itemMap{to: map[int32]int32{}, back: map[int32]int32{}}
	same := true
	for _, it := range backend {
		cid, ok := client[it.Name]
		if !ok {
			cid = 0
		}
		if cid != int32(it.RuntimeID) {
			same = false
		}
		m.to[int32(it.RuntimeID)] = cid
		if ok {
			m.back[cid] = int32(it.RuntimeID)
		}
	}
	if same {
		return nil
	}
	return m
}

func (r *rewriter) item(id int32, toClient bool) int32 {
	m := r.items.Load()
	if m == nil || id == 0 {
		return id
	}
	tbl := m.to
	if !toClient {
		tbl = m.back
	}
	if v, ok := tbl[id]; ok {
		return v
	}
	return id
}

func (r *rewriter) swapRID(v uint64) uint64 {
	switch v {
	case r.rid[0]:
		return r.rid[1]
	case r.rid[1]:
		return r.rid[0]
	}
	return v
}

func (r *rewriter) swapUID(v int64) int64 {
	switch v {
	case r.uid[0]:
		return r.uid[1]
	case r.uid[1]:
		return r.uid[0]
	}
	return v
}

func (r *rewriter) block(id uint32, toClient bool) uint32 {
	if r.blocks == nil {
		return id
	}
	if toClient {
		return r.blocks.forward(id)
	}
	return r.blocks.reverse(id)
}

// leadingRIDPackets start with the varuint64 runtime id of the entity they are about and hold no
// other id: when that id is neither of the player's, they pass untouched without decoding. They are
// the busiest packets (entity movement).
var leadingRIDPackets = map[uint32]bool{
	packet.IDMoveActorDelta:    true,
	packet.IDMoveActorAbsolute: true,
	packet.IDSetActorMotion:    true,
	packet.IDUpdateAttributes:  true,
	packet.IDActorEvent:        true,
	packet.IDMobEffect:         true,
}

// rewrite returns the packet as the other side must see it. toClient is the direction. pool decodes.
func (r *rewriter) rewrite(raw []byte, toClient bool, pool packet.Pool) ([]byte, error) {
	if r.identity() {
		return raw, nil
	}
	id, payload, err := splitPacket(raw)
	if err != nil {
		return nil, err
	}
	if r.blocks != nil && toClient {
		switch id {
		case packet.IDLevelChunk:
			return r.rewriteLevelChunk(payload, raw)
		case packet.IDSubChunk:
			return r.rewriteSubChunk(payload, raw)
		}
	}
	if !packetMayNeedRewrite(id, pool, r.mode()) {
		return raw, nil
	}
	if leadingRIDPackets[id] && (r.blocks == nil || id != packet.IDActorEvent) {
		v, ok := peekVaruint64(payload)
		if ok && v != r.rid[0] && v != r.rid[1] {
			return raw, nil
		}
	}
	f, ok := pool[id]
	if !ok {
		return raw, nil
	}
	pk := f()
	if err := decodeInto(pk, payload); err != nil {
		return nil, err
	}
	changed := r.special(pk, toClient)
	if r.walk(reflect.ValueOf(pk), toClient) {
		changed = true
	}
	if !changed {
		return raw, nil
	}
	return encodePacket(pk), nil
}

// special rewrites what the generic walk can't see from field names: block ids in event data and
// in the variant of falling blocks, and entity references in entity metadata.
func (r *rewriter) special(pk packet.Packet, toClient bool) bool {
	switch pk := pk.(type) {
	case *packet.LevelEvent:
		if r.blocks == nil {
			return false
		}
		switch pk.EventType {
		case packet.LevelEventParticlesDestroyBlock, packet.LevelEventParticlesDestroyBlockNoSound,
			packet.LevelEventParticlesCrackBlock, packet.LevelEventParticlesCrackBlockDown,
			packet.LevelEventParticlesCrackBlockUp, packet.LevelEventParticlesCrackBlockNorth,
			packet.LevelEventParticlesCrackBlockSouth, packet.LevelEventParticlesCrackBlockWest,
			packet.LevelEventParticlesCrackBlockEast:
			low := uint32(pk.EventData) & 0xffffff
			pk.EventData = int32(uint32(pk.EventData)&^0xffffff | r.block(low, toClient)&0xffffff)
			return true
		}
	case *packet.LevelSoundEvent:
		if r.blocks == nil {
			return false
		}
		switch pk.SoundType {
		case packet.SoundEventItemUseOn, packet.SoundEventHit, packet.SoundEventStep, packet.SoundEventBreak,
			packet.SoundEventPlace, packet.SoundEventHeavyStep, packet.SoundEventFall, packet.SoundEventLand,
			packet.SoundEventBreakBlock:
			if pk.ExtraData >= 0 {
				pk.ExtraData = int32(r.block(uint32(pk.ExtraData), toClient))
				return true
			}
		}
	case *packet.AddActor:
		changed := r.metadata(pk.EntityMetadata, toClient)
		if pk.EntityType == "minecraft:falling_block" {
			rid := pk.EntityRuntimeID
			if r.swapEntities {
				rid = r.swapRID(rid)
			}
			r.fallingMu.Lock()
			r.falling[rid] = true
			r.fallingMu.Unlock()
			if r.variant(pk.EntityMetadata, toClient) {
				changed = true
			}
		}
		return changed
	case *packet.SetActorData:
		changed := r.metadata(pk.EntityMetadata, toClient)
		rid := pk.EntityRuntimeID
		if r.swapEntities && toClient {
			rid = r.swapRID(rid)
		}
		r.fallingMu.Lock()
		falling := r.falling[rid]
		r.fallingMu.Unlock()
		if falling && r.variant(pk.EntityMetadata, toClient) {
			changed = true
		}
		return changed
	case *packet.AddPlayer:
		return r.metadata(pk.EntityMetadata, toClient)
	case *packet.AddItemActor:
		return r.metadata(pk.EntityMetadata, toClient)
	case *packet.RemoveActor:
		if r.blocks != nil {
			r.fallingMu.Lock()
			delete(r.falling, uint64(pk.EntityUniqueID))
			r.fallingMu.Unlock()
		}
	}
	return false
}

// entityRefKeys are the entity metadata keys that hold the unique id of another entity.
var entityRefKeys = []uint32{
	protocol.EntityDataKeyOwner, protocol.EntityDataKeyTarget, protocol.EntityDataKeyLeashHolder,
	protocol.EntityDataKeyTradeTarget, protocol.EntityDataKeyBalloonAnchor, protocol.EntityDataKeyAgent,
}

func (r *rewriter) metadata(m protocol.EntityMetadata, toClient bool) bool {
	changed := false
	if r.swapEntities {
		for _, k := range entityRefKeys {
			if v, ok := m[k].(int64); ok {
				if nv := r.swapUID(v); nv != v {
					m[k] = nv
					changed = true
				}
			}
		}
	}
	if r.blocks != nil {
		if v, ok := m[protocol.EntityDataKeyCarryBlockRuntimeID].(int32); ok && v >= 0 {
			m[protocol.EntityDataKeyCarryBlockRuntimeID] = int32(r.block(uint32(v), toClient))
			changed = true
		}
	}
	return changed
}

func (r *rewriter) variant(m protocol.EntityMetadata, toClient bool) bool {
	if r.blocks == nil {
		return false
	}
	if v, ok := m[protocol.EntityDataKeyVariant].(int32); ok && v >= 0 {
		m[protocol.EntityDataKeyVariant] = int32(r.block(uint32(v), toClient))
		return true
	}
	return false
}

var (
	itemStackType = reflect.TypeOf(protocol.ItemStack{})
	metadataType  = reflect.TypeOf(protocol.EntityMetadata{})
)

// walk rewrites, through reflection, every field named like an entity id or a block runtime id. It
// reports whether anything changed.
func (r *rewriter) walk(v reflect.Value, toClient bool) bool {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		return r.walk(v.Elem(), toClient)
	case reflect.Interface:
		if v.IsNil() {
			return false
		}
		e := v.Elem()
		if e.Kind() == reflect.Pointer {
			return r.walk(e, toClient)
		}
		if !typeMayNeedRewrite(e.Type(), r.mode()) || !v.CanSet() {
			return false
		}
		cp := reflect.New(e.Type()).Elem()
		cp.Set(e)
		if r.walk(cp, toClient) {
			v.Set(cp)
			return true
		}
		return false
	case reflect.Struct:
		if !typeMayNeedRewrite(v.Type(), r.mode()) {
			return false
		}
		if v.Type() == itemStackType {
			changed := false
			if r.items.Load() != nil {
				f := v.FieldByName("NetworkID")
				if n := r.item(int32(f.Int()), toClient); int64(n) != f.Int() {
					f.SetInt(int64(n))
					changed = true
				}
			}
			clientItem := int32(v.FieldByName("NetworkID").Int())
			if !toClient {
				// The stack still has the client's numbering here only before the item swap
				// above; look the item up by what it was.
				clientItem = r.item(clientItem, true)
			}
			blockRID, isBlockItem := r.blockItems[clientItem]
			f := v.FieldByName("BlockRuntimeID")
			switch {
			case toClient && isBlockItem && r.blocks != nil && (f.Int() == 0 || r.blocks.isAir(uint32(f.Int()), true)):
				f.SetInt(int64(blockRID))
				changed = true
			case !toClient && isBlockItem && r.blocks != nil && uint32(f.Int()) == blockRID && r.blocks.reverse(blockRID) == r.blocks.fromAir:
				f.SetInt(0) // the server has no such block: its item is a plain item there
				changed = true
			case r.blocks != nil && f.Int() != 0:
				f.SetInt(int64(r.block(uint32(f.Int()), toClient)))
				changed = true
			}
			return changed
		}
		changed := false
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			f := v.Field(i)
			switch fieldKind(sf.Name) {
			case fieldRuntimeID:
				if r.swapEntities && setUint(f, r.swapRID) {
					changed = true
				}
				continue
			case fieldUniqueID:
				if r.swapEntities && setInt(f, r.swapUID) {
					changed = true
				}
				continue
			case fieldBlockID:
				if r.blocks != nil && setUint(f, func(x uint64) uint64 { return uint64(r.block(uint32(x), toClient)) }) {
					changed = true
				}
				continue
			}
			if r.walk(f, toClient) {
				changed = true
			}
		}
		return changed
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return false
		}
		if !typeMayNeedRewrite(v.Type().Elem(), r.mode()) {
			return false
		}
		changed := false
		for i := 0; i < v.Len(); i++ {
			if r.walk(v.Index(i), toClient) {
				changed = true
			}
		}
		return changed
	case reflect.Map:
		if v.Type() == metadataType && !v.IsNil() {
			return r.metadata(v.Interface().(protocol.EntityMetadata), toClient)
		}
	}
	return false
}

type fieldClass int

const (
	fieldOther fieldClass = iota
	fieldRuntimeID
	fieldUniqueID
	fieldBlockID
)

func fieldKind(name string) fieldClass {
	switch {
	case name == "BlockRuntimeID" || name == "NewBlockRuntimeID":
		return fieldBlockID
	case strings.HasSuffix(name, "EntityRuntimeID") || name == "PlayerRuntimeID" || name == "EntityRuntimeIDs":
		return fieldRuntimeID
	case strings.HasSuffix(name, "UniqueID"):
		return fieldUniqueID
	}
	return fieldOther
}

// setUint applies f to an unsigned or signed integer field, or to each element of a slice of them.
func setUint(v reflect.Value, f func(uint64) uint64) bool {
	switch v.Kind() {
	case reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8:
		if n := f(v.Uint()); n != v.Uint() {
			v.SetUint(n)
			return true
		}
	case reflect.Int64, reflect.Int32:
		if v.Int() < 0 {
			return false
		}
		if n := f(uint64(v.Int())); n != uint64(v.Int()) {
			v.SetInt(int64(n))
			return true
		}
	case reflect.Slice:
		changed := false
		for i := 0; i < v.Len(); i++ {
			if setUint(v.Index(i), f) {
				changed = true
			}
		}
		return changed
	}
	return false
}

func setInt(v reflect.Value, f func(int64) int64) bool {
	switch v.Kind() {
	case reflect.Int64, reflect.Int32:
		if n := f(v.Int()); n != v.Int() {
			v.SetInt(n)
			return true
		}
	case reflect.Uint64:
		if v.Uint() > math.MaxInt64 {
			return false
		}
		if n := f(int64(v.Uint())); n != int64(v.Uint()) {
			v.SetUint(uint64(n))
			return true
		}
	}
	return false
}

// typeMayNeedRewrite reports whether a value of type t can hold anything rewrite changes, so big
// structures without (skins, NBT, recipes without items) are skipped.
func typeMayNeedRewrite(t reflect.Type, mode int) bool {
	key := typeKey{t, mode}
	if v, ok := mayNeed.Load(key); ok {
		return v.(bool)
	}
	v := typeMayNeed(t, mode, map[reflect.Type]bool{})
	mayNeed.Store(key, v)
	return v
}

type typeKey struct {
	t    reflect.Type
	mode int
}

var mayNeed sync.Map

func typeMayNeed(t reflect.Type, mode int, seen map[reflect.Type]bool) bool {
	blocks := mode&modeBlocks != 0
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeMayNeed(t.Elem(), mode, seen)
	case reflect.Interface:
		return true
	case reflect.Map:
		return t == metadataType
	case reflect.Struct:
		if t == itemStackType {
			return mode != 0
		}
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			switch fieldKind(sf.Name) {
			case fieldRuntimeID, fieldUniqueID:
				return true
			case fieldBlockID:
				if blocks {
					return true
				}
				continue
			}
			if typeMayNeed(sf.Type, mode, seen) {
				return true
			}
		}
	}
	return false
}

// packetMayNeedRewrite reports whether a packet id can hold anything rewrite changes.
func packetMayNeedRewrite(id uint32, pool packet.Pool, mode int) bool {
	switch id {
	case packet.IDLevelEvent, packet.IDLevelSoundEvent:
		return mode&modeBlocks != 0
	}
	key := packetKey{id, mode}
	if v, ok := packetNeed.Load(key); ok {
		return v.(bool)
	}
	v := false
	if f, ok := pool[id]; ok {
		v = typeMayNeedRewrite(reflect.TypeOf(f()), mode)
	}
	packetNeed.Store(key, v)
	return v
}

type packetKey struct {
	id   uint32
	mode int
}

var packetNeed sync.Map

func peekVaruint64(b []byte) (uint64, bool) {
	var v uint64
	for i := 0; i < 10 && i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, true
		}
	}
	return 0, false
}

// rewriteLevelChunk renumbers the block palettes of a chunk's sub-chunks. Chunks sent in sub-chunk
// request mode carry no blocks here (SubChunk packets do).
func (r *rewriter) rewriteLevelChunk(payload, raw []byte) ([]byte, error) {
	var pk packet.LevelChunk
	if err := decodeInto(&pk, payload); err != nil {
		return nil, err
	}
	if _, requestMode := pk.SubChunkLimit.Value(); requestMode || pk.CacheEnabled {
		return raw, nil
	}
	out, err := rewriteSubChunks(pk.RawPayload, int(pk.SubChunkCount), r.toClientBlock)
	if err != nil {
		return nil, fmt.Errorf("level chunk %v: %w", pk.Position, err)
	}
	pk.RawPayload = out
	return encodePacket(&pk), nil
}

func (r *rewriter) rewriteSubChunk(payload, raw []byte) ([]byte, error) {
	var pk packet.SubChunk
	if err := decodeInto(&pk, payload); err != nil {
		return nil, err
	}
	if pk.CacheEnabled {
		return raw, nil
	}
	for i, e := range pk.SubChunkEntries {
		data, ok := e.RawPayload.Value()
		if !ok || len(data) == 0 || e.Result != protocol.SubChunkResultSuccess {
			continue
		}
		out, err := rewriteSubChunks(data, 1, r.toClientBlock)
		if err != nil {
			return nil, fmt.Errorf("sub chunk %v: %w", pk.Position, err)
		}
		pk.SubChunkEntries[i].RawPayload = protocol.Option(out)
	}
	return encodePacket(&pk), nil
}

// rewriteSubChunks renumbers the palettes of n network-encoded sub-chunks at the start of data and
// copies the rest (biomes, border blocks, block entities) unchanged.
func (r *rewriter) toClientBlock(id uint32) uint32 { return r.block(id, true) }

func rewriteSubChunks(data []byte, n int, mapID func(uint32) uint32) ([]byte, error) {
	buf := bytes.NewBuffer(data)
	out := bytes.NewBuffer(make([]byte, 0, len(data)+64))
	for s := 0; s < n; s++ {
		ver, err := buf.ReadByte()
		if err != nil {
			return nil, err
		}
		out.WriteByte(ver)
		var storages byte = 1
		switch ver {
		case 1:
		case 8, 9:
			if storages, err = buf.ReadByte(); err != nil {
				return nil, err
			}
			out.WriteByte(storages)
			if ver == 9 {
				y, err := buf.ReadByte()
				if err != nil {
					return nil, err
				}
				out.WriteByte(y)
			}
		default:
			return nil, fmt.Errorf("unknown sub-chunk version %d", ver)
		}
		for i := byte(0); i < storages; i++ {
			if err := rewriteStorage(buf, out, mapID); err != nil {
				return nil, err
			}
		}
	}
	out.Write(buf.Bytes())
	return out.Bytes(), nil
}

func rewriteStorage(buf, out *bytes.Buffer, mapID func(uint32) uint32) error {
	head, err := buf.ReadByte()
	if err != nil {
		return err
	}
	out.WriteByte(head)
	bitsPer := head >> 1
	if bitsPer == 0x7f {
		return nil
	}
	if bitsPer > 32 {
		return fmt.Errorf("palette of %d bits", bitsPer)
	}
	words := 0
	if bitsPer != 0 {
		words = 4096 / (32 / int(bitsPer))
		if bitsPer == 3 || bitsPer == 5 || bitsPer == 6 {
			words++
		}
	}
	block := buf.Next(words * 4)
	if len(block) != words*4 {
		return errors.New("short block data")
	}
	out.Write(block)
	count := int32(1)
	if bitsPer != 0 {
		if err := protocol.Varint32(buf, &count); err != nil {
			return err
		}
		if count <= 0 || count > 4096 {
			return fmt.Errorf("palette of %d entries", count)
		}
		_ = protocol.WriteVarint32(out, count)
	}
	for i := int32(0); i < count; i++ {
		var v int32
		if err := protocol.Varint32(buf, &v); err != nil {
			return err
		}
		_ = protocol.WriteVarint32(out, int32(mapID(uint32(v))))
	}
	return nil
}
