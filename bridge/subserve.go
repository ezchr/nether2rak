package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// A client that was on a server using sub-chunk requests (Dragonfly) keeps asking for the
// sub-chunks of the columns it had there, also after a move to a server that sends its columns
// whole (Geyser): for every such column the new server sends, the client asks again. With two
// lobbies at the same coordinates that is every column around the spawn. The relay used to answer
// all of them with air, which wiped the new server's column: the lobby was invisible. A server
// that sends whole columns can't be asked either (it kicks the client), so the relay answers from
// the columns that server sent, which it keeps for this.

const (
	servedColumnsMax = 4096            // columns kept per player; beyond it the store starts over
	subPendingWait   = 3 * time.Second // how long a request waits for its column before it gets air
)

// servedColumn is a column as the player's server sent it, and what its sub-chunks are once asked.
type servedColumn struct {
	raw    []byte // the LevelChunk packet
	parsed bool
	ok     bool
	subs   map[int32][]byte // by sub-chunk Y, in SubChunk packet form; only ones with blocks
	// height is the Y of the highest block of each x, z of the column (index [z][x]), or noHeight.
	height [16][16]int32
}

const noHeight = math.MinInt32

// pendingSub is a request for a sub-chunk of a column the server has not sent yet.
type pendingSub struct {
	base protocol.SubChunkPos
	off  protocol.SubChunkOffset
	dim  int32
}

// keepColumn keeps a LevelChunk the player's server sent, and answers what waited for it. Call it
// with s.mu held.
func (s *switchSession) keepColumn(pk *packet.LevelChunk, raw []byte) {
	if _, requestMode := pk.SubChunkLimit.Value(); requestMode || pk.CacheEnabled {
		return
	}
	if s.columns == nil || len(s.columns) >= servedColumnsMax {
		s.columns = map[[2]int32]*servedColumn{}
	}
	col := [2]int32{pk.Position[0], pk.Position[1]}
	c := &servedColumn{raw: append([]byte(nil), raw...)}
	s.columns[col] = c
	waiting := s.subPending[col]
	if len(waiting) == 0 {
		return
	}
	delete(s.subPending, col)
	pks := s.answerLocked(c, waiting)
	go s.sendClient(pks...)
}

// forgetColumns drops the kept columns and waiting requests: the player changed server or
// dimension. Call it with s.mu held.
func (s *switchSession) forgetColumns() {
	s.columns, s.subPending = nil, nil
}

// serveSubChunks answers a sub-chunk request from the columns the player's server sent. A request
// for a column not sent yet waits for it, and gets air if it never comes.
func (s *switchSession) serveSubChunks(req *packet.SubChunkRequest) {
	offsets := req.Offsets
	if len(offsets) == 0 {
		offsets = []protocol.SubChunkOffset{{0, 0, 0}}
	}
	s.mu.Lock()
	var out []packet.Packet
	byColumn := map[[2]int32][]pendingSub{}
	for _, o := range offsets {
		col := [2]int32{req.Position[0] + int32(o[0]), req.Position[2] + int32(o[2])}
		byColumn[col] = append(byColumn[col], pendingSub{base: req.Position, off: o, dim: req.Dimension})
	}
	var late [][2]int32
	for col, subs := range byColumn {
		if c, ok := s.columns[col]; ok {
			out = append(out, s.answerLocked(c, subs)...)
			continue
		}
		if s.subPending == nil {
			s.subPending = map[[2]int32][]pendingSub{}
		}
		if len(s.subPending[col]) == 0 {
			late = append(late, col)
		}
		s.subPending[col] = append(s.subPending[col], subs...)
	}
	s.subServed += len(offsets)
	s.mu.Unlock()
	if len(out) > 0 {
		s.sendClient(out...)
	}
	for _, col := range late {
		time.AfterFunc(subPendingWait, func() { s.airPending(col) })
	}
}

// airPending answers what still waits for col with air.
func (s *switchSession) airPending(col [2]int32) {
	s.mu.Lock()
	waiting := s.subPending[col]
	delete(s.subPending, col)
	var pks []packet.Packet
	if len(waiting) > 0 {
		pks = s.answerLocked(&servedColumn{parsed: true}, waiting)
	}
	s.mu.Unlock()
	if len(pks) > 0 {
		s.sendClient(pks...)
	}
}

// answerLocked builds the SubChunk packets answering reqs, all for column c. s.mu held.
func (s *switchSession) answerLocked(c *servedColumn, reqs []pendingSub) []packet.Packet {
	if !c.parsed {
		c.parsed = true
		air := s.palette.index[blockState{Name: "minecraft:air", Properties: encodeProperties(nil)}]
		if err := c.parse(air); err != nil {
			s.log.Warn("network: a server's column could not be split into sub-chunks, the client gets air for it", "err", err)
		}
	}
	type key struct {
		base protocol.SubChunkPos
		dim  int32
	}
	byBase := map[key]*packet.SubChunk{}
	var order []*packet.SubChunk
	for _, r := range reqs {
		k := key{r.base, r.dim}
		pk, ok := byBase[k]
		if !ok {
			pk = &packet.SubChunk{Dimension: r.dim, Position: r.base}
			byBase[k] = pk
			order = append(order, pk)
		}
		pk.SubChunkEntries = append(pk.SubChunkEntries, c.entry(r.off, r.base[1]+int32(r.off[1])))
	}
	out := make([]packet.Packet, len(order))
	for i, pk := range order {
		out[i] = pk
	}
	return out
}

// entry is the answer for the sub-chunk at Y y of the column.
func (c *servedColumn) entry(off protocol.SubChunkOffset, y int32) protocol.SubChunkEntry {
	e := protocol.SubChunkEntry{Offset: off, Result: protocol.SubChunkResultSuccessAllAir}
	if data, ok := c.subs[y]; ok && c.ok {
		e.Result = protocol.SubChunkResultSuccess
		e.RawPayload = protocol.Option(data)
	}
	var hm protocol.HeightMap
	higher, lower := true, true
	for z := 0; z < 16; z++ {
		for x := 0; x < 16; x++ {
			h := c.height[z][x]
			switch {
			case !c.ok || h == noHeight || h>>4 < y:
				hm[z][x], higher = -1, false
			case h>>4 > y:
				hm[z][x], lower = 16, false
			default:
				hm[z][x], higher, lower = int8(h&15), false, false
			}
		}
	}
	switch {
	case higher:
		e.HeightMapType = protocol.HeightMapDataTooHigh
	case lower:
		e.HeightMapType = protocol.HeightMapDataTooLow
	default:
		e.HeightMapType = protocol.HeightMapDataHasData
		e.HeightMapData = protocol.Option(hm)
	}
	// The render height map as Dragonfly sends it: the same again. (With "all copied" as its type
	// the client dropped the connection.)
	e.RenderHeightMapType, e.RenderHeightMapData = e.HeightMapType, e.HeightMapData
	return e
}

// parse splits the column's LevelChunk into its sub-chunks. air is the client's id of air.
func (c *servedColumn) parse(air uint32) error {
	for z := range c.height {
		for x := range c.height[z] {
			c.height[z][x] = noHeight
		}
	}
	_, payload, err := splitPacket(c.raw)
	if err != nil {
		return err
	}
	var pk packet.LevelChunk
	if err := decodeInto(&pk, payload); err != nil {
		return err
	}
	minY := int32(-4)
	if pk.Dimension != dimOverworld {
		minY = 0
	}
	c.subs = map[int32][]byte{}
	buf := bytes.NewBuffer(pk.RawPayload)
	for i := 0; i < int(pk.SubChunkCount); i++ {
		ver, err := buf.ReadByte()
		if err != nil {
			return err
		}
		storages, y := byte(1), minY+int32(i)
		switch ver {
		case 1:
		case 8, 9:
			if storages, err = buf.ReadByte(); err != nil {
				return err
			}
			if ver == 9 {
				b, err := buf.ReadByte()
				if err != nil {
					return err
				}
				y = int32(int8(b))
			}
		default:
			return fmt.Errorf("unknown sub-chunk version %d", ver)
		}
		start := len(pk.RawPayload) - buf.Len()
		blocks := false
		for n := byte(0); n < storages; n++ {
			tops, any, err := readStorage(buf, air)
			if err != nil {
				return err
			}
			if !any {
				continue
			}
			blocks = true
			for z := 0; z < 16; z++ {
				for x := 0; x < 16; x++ {
					if t := tops[z][x]; t >= 0 {
						if h := y<<4 + int32(t); h > c.height[z][x] {
							c.height[z][x] = h
						}
					}
				}
			}
		}
		if !blocks {
			continue
		}
		end := len(pk.RawPayload) - buf.Len()
		data := make([]byte, 0, end-start+3)
		data = append(data, 9, storages, byte(int8(y)))
		c.subs[y] = append(data, pk.RawPayload[start:end]...)
	}
	c.ok = true
	return nil
}

// readStorage reads one block storage of a sub-chunk. tops is the highest block that is not air of
// each x, z (index [z][x], -1 for none) and any whether there is one at all.
func readStorage(buf *bytes.Buffer, air uint32) (tops [16][16]int8, any bool, err error) {
	for z := range tops {
		for x := range tops[z] {
			tops[z][x] = -1
		}
	}
	head, err := buf.ReadByte()
	if err != nil {
		return tops, false, err
	}
	bitsPer := int(head >> 1)
	if bitsPer == 0x7f {
		return tops, false, nil // same as the storage before it: nothing new for the heights
	}
	if bitsPer > 32 {
		return tops, false, fmt.Errorf("palette of %d bits", bitsPer)
	}
	var words []byte
	perWord := 0
	if bitsPer != 0 {
		perWord = 32 / bitsPer
		n := 4096 / perWord
		if bitsPer == 3 || bitsPer == 5 || bitsPer == 6 {
			n++
		}
		words = buf.Next(n * 4)
		if len(words) != n*4 {
			return tops, false, errors.New("short block data")
		}
	}
	count := int32(1)
	if bitsPer != 0 {
		if err := protocol.Varint32(buf, &count); err != nil {
			return tops, false, err
		}
		if count <= 0 || count > 4096 {
			return tops, false, fmt.Errorf("palette of %d entries", count)
		}
	}
	solid := make([]bool, count)
	for i := range solid {
		var v int32
		if err := protocol.Varint32(buf, &v); err != nil {
			return tops, false, err
		}
		solid[i] = uint32(v) != air
		any = any || solid[i]
	}
	if !any {
		return tops, false, nil
	}
	if bitsPer == 0 {
		for z := range tops {
			for x := range tops[z] {
				tops[z][x] = 15
			}
		}
		return tops, true, nil
	}
	mask := uint32(1)<<bitsPer - 1
	any = false
	for i := 0; i < 4096; i++ {
		w := i / perWord * 4
		word := uint32(words[w]) | uint32(words[w+1])<<8 | uint32(words[w+2])<<16 | uint32(words[w+3])<<24
		v := word >> (i % perWord * bitsPer) & mask
		if int(v) >= len(solid) || !solid[v] {
			continue
		}
		x, z, y := i>>8, i>>4&15, i&15
		if int8(y) > tops[z][x] {
			tops[z][x] = int8(y)
		}
		any = true
	}
	return tops, any, nil
}
