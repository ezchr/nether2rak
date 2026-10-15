package bridge

import (
	"bytes"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// A column of two version 8 sub-chunks, the second with one block, splits into that sub-chunk in
// SubChunk form, with the block's height.
func TestServedColumnParse(t *testing.T) {
	const air, stone = 7, 42
	b := bytes.NewBuffer(nil)
	// sub-chunk 0 (Y -4): one storage, all air
	b.Write([]byte{8, 1, 0<<1 | 1})
	_ = protocol.WriteVarint32(b, air)
	// sub-chunk 1 (Y -3): one storage of 1 bit per block, a stone at x=3 y=5 z=9
	b.Write([]byte{8, 1, 1<<1 | 1})
	words := make([]byte, 128*4)
	i := 3<<8 | 9<<4 | 5
	words[i/32*4+i%32/8] |= 1 << (i % 8)
	b.Write(words)
	_ = protocol.WriteVarint32(b, 2)
	_ = protocol.WriteVarint32(b, air)
	_ = protocol.WriteVarint32(b, stone)
	sub1 := append([]byte{9, 1, 0xfd}, b.Bytes()[2+1+1:][2:]...)
	tail := []byte{0xaa, 0xbb} // biomes and the rest
	b.Write(tail)

	raw := encodePacket(&packet.LevelChunk{Position: protocol.ChunkPos{2, -1}, SubChunkCount: 2, RawPayload: b.Bytes()})
	c := &servedColumn{raw: raw}
	if err := c.parse(air); err != nil {
		t.Fatal(err)
	}
	if !c.ok || len(c.subs) != 1 {
		t.Fatalf("ok=%v, %d sub-chunks kept, want 1", c.ok, len(c.subs))
	}
	if !bytes.Equal(c.subs[-3], sub1[:len(sub1)-0]) {
		t.Errorf("sub-chunk -3 = %d bytes %v..., want %d bytes %v...", len(c.subs[-3]), c.subs[-3][:6], len(sub1), sub1[:6])
	}
	if h := c.height[9][3]; h != -3*16+5 {
		t.Errorf("height at x=3 z=9 = %d, want %d", h, -3*16+5)
	}
	if h := c.height[0][0]; h != noHeight {
		t.Errorf("height of an empty x, z = %d, want none", h)
	}

	e := c.entry(protocol.SubChunkOffset{}, -3)
	if e.Result != protocol.SubChunkResultSuccess || e.HeightMapType != protocol.HeightMapDataHasData {
		t.Errorf("entry of the sub-chunk with the block: result %d, height map type %d", e.Result, e.HeightMapType)
	}
	if hm, _ := e.HeightMapData.Value(); hm[9][3] != 5 || hm[0][0] != -1 {
		t.Errorf("height map: %d at the block, %d elsewhere; want 5 and -1", hm[9][3], hm[0][0])
	}
	if e := c.entry(protocol.SubChunkOffset{}, -4); e.Result != protocol.SubChunkResultSuccessAllAir || e.HeightMapType != protocol.HeightMapDataHasData {
		t.Errorf("entry below the block: result %d, height map type %d", e.Result, e.HeightMapType)
	}
	if e := c.entry(protocol.SubChunkOffset{}, 0); e.Result != protocol.SubChunkResultSuccessAllAir || e.HeightMapType != protocol.HeightMapDataTooLow {
		t.Errorf("entry above everything: result %d, height map type %d", e.Result, e.HeightMapType)
	}
}
