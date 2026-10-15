package bridge

import (
	"bytes"
	"compress/gzip"
	"os"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// TestVanillaPaletteMatchesGeyser checks the embedded vanilla states and the sort against Geyser's
// own palette file, when it is around (N2R_GEYSER_PALETTE).
func TestVanillaPaletteMatchesGeyser(t *testing.T) {
	path := os.Getenv("N2R_GEYSER_PALETTE")
	if path == "" {
		t.Skip("N2R_GEYSER_PALETTE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Blocks []map[string]any `nbt:"blocks"`
	}
	if err := nbt.NewDecoderWithEncoding(zr, nbt.BigEndian).Decode(&m); err != nil {
		t.Fatal(err)
	}
	p, err := newBlockPalette(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blocks) != len(p.states) {
		t.Fatalf("geyser has %d states, we have %d", len(m.Blocks), len(p.states))
	}
	bad := 0
	for i, b := range m.Blocks {
		name, _ := b["name"].(string)
		states, _ := b["states"].(map[string]any)
		want := blockState{Name: name, Properties: encodeProperties(states)}
		if p.states[i] != want {
			if bad < 5 {
				t.Errorf("state %d: geyser %v, we %v", i, want, p.states[i])
			}
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d states differ", bad)
	}
}

func TestCustomBlocksShiftPalette(t *testing.T) {
	vanilla, _ := newBlockPalette(nil)
	withOres, err := newBlockPalette([]protocol.BlockEntry{
		{Name: "oresplus:ruby_ore", Properties: map[string]any{}},
		{Name: "minecraft:red_shrub", Properties: map[string]any{}}, // vanilla data-driven: ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withOres.states) != len(vanilla.states)+1 {
		t.Fatalf("got %d states, want %d", len(withOres.states), len(vanilla.states)+1)
	}
	m := newBlockMap(vanilla, withOres)
	for i, s := range vanilla.states {
		if got := withOres.states[m.forward(uint32(i))]; got != s {
			t.Fatalf("state %d maps to %v, want %v", i, got, s)
		}
		if m.reverse(m.forward(uint32(i))) != uint32(i) {
			t.Fatalf("state %d does not map back", i)
		}
	}
	ruby := withOres.index[blockState{Name: "oresplus:ruby_ore", Properties: encodeProperties(nil)}]
	if vanilla.states[m.reverse(ruby)].Name != "minecraft:air" {
		t.Fatalf("custom block maps back to %v, want air", vanilla.states[m.reverse(ruby)])
	}
}
