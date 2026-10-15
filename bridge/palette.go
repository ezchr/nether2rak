package bridge

import (
	"bytes"
	_ "embed"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// The block palette: what a client numbers block states by. With block network id hashes off (both
// Dragonfly and Geyser leave them off), a block's runtime id is its index in the list of every
// vanilla block state plus the states of the custom blocks the server announced in StartGame, sorted
// (stably) by the FNV-1 64-bit hash of the block name. Two servers with different custom blocks so
// number most blocks differently, and the relay renumbers between them.

// vanillaBlockStates is every vanilla block state of the supported version, in order, as network
// NBT (Dragonfly's block_states.nbt for 1.26.50). It must be updated with the protocol version.
//
//go:embed block_states.nbt
var vanillaBlockStates []byte

// blockState is one entry of a palette: a block name and its encoded properties.
type blockState struct {
	Name       string
	Properties string // canonical encoding, see encodeProperties
}

// vanillaStates is the decoded vanillaBlockStates, and vanillaNames the set of names in it.
var vanillaStates, vanillaNames = func() ([]blockState, map[string]bool) {
	dec := nbt.NewDecoder(bytes.NewBuffer(vanillaBlockStates))
	var states []blockState
	names := map[string]bool{}
	for {
		var s struct {
			Name       string         `nbt:"name"`
			Properties map[string]any `nbt:"states"`
			Version    int32          `nbt:"version"`
		}
		if err := dec.Decode(&s); err != nil {
			break
		}
		states = append(states, blockState{Name: s.Name, Properties: encodeProperties(s.Properties)})
		names[s.Name] = true
	}
	return states, names
}()

// encodeProperties encodes block properties canonically, so equal states encode equally.
func encodeProperties(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v;", k, m[k])
	}
	return b.String()
}

// blockPalette is the full numbering a client uses for one set of custom blocks.
type blockPalette struct {
	states []blockState
	index  map[blockState]uint32
	// custom is the sorted names of the non-vanilla custom blocks, which identify the palette.
	custom []string
}

// newBlockPalette builds the palette a client builds from StartGame's custom block list.
func newBlockPalette(blocks []protocol.BlockEntry) (*blockPalette, error) {
	states := slices.Clone(vanillaStates)
	var custom []string
	for _, b := range blocks {
		if vanillaNames[b.Name] {
			// A vanilla data-driven block: StartGame only carries its components, the states are
			// already in the vanilla list.
			continue
		}
		perms, err := customBlockStates(b)
		if err != nil {
			return nil, fmt.Errorf("custom block %s: %w", b.Name, err)
		}
		states = append(states, perms...)
		custom = append(custom, b.Name)
	}
	sort.SliceStable(states, func(i, j int) bool {
		return fnv164(states[i].Name) < fnv164(states[j].Name)
	})
	p := &blockPalette{states: states, index: make(map[blockState]uint32, len(states)), custom: custom}
	sort.Strings(p.custom)
	for i, s := range states {
		p.index[s] = uint32(i)
	}
	return p, nil
}

// customBlockStates lists a custom block's states in the order the client registers them: every
// combination of its properties' values, the FIRST property changing fastest (as Geyser generates
// them, matching the client). Property values come as whatever NBT list type the server used
// (strings, ints, bytes for booleans).
func customBlockStates(b protocol.BlockEntry) ([]blockState, error) {
	props, _ := b.Properties["properties"].([]any)
	type prop struct {
		name   string
		values []any
	}
	var ps []prop
	for _, p := range props {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property is %T", p)
		}
		name, _ := m["name"].(string)
		values := anySlice(m["enum"])
		if name == "" || len(values) == 0 {
			return nil, fmt.Errorf("property %v has no name or values", m)
		}
		ps = append(ps, prop{name, values})
	}
	if len(ps) == 0 {
		return []blockState{{Name: b.Name, Properties: encodeProperties(nil)}}, nil
	}
	total := 1
	for _, p := range ps {
		total *= len(p.values)
	}
	out := make([]blockState, 0, total)
	for i := 0; i < total; i++ {
		cur := make(map[string]any, len(ps))
		idx := i
		for _, p := range ps {
			cur[p.name] = p.values[idx%len(p.values)]
			idx /= len(p.values)
		}
		out = append(out, blockState{Name: b.Name, Properties: encodeProperties(cur)})
	}
	return out, nil
}

// anySlice returns the elements of any slice (an NBT list decodes to []any, []int32, []string...).
func anySlice(v any) []any {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// fnv164 is the FNV-1 64-bit hash (multiply, then xor) the palette is sorted by.
func fnv164(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(s); i++ {
		h *= 1099511628211
		h ^= uint64(s[i])
	}
	return h
}

// sameCustomBlocks reports whether two palettes number every block the same.
func (p *blockPalette) sameAs(o *blockPalette) bool {
	return slices.Equal(p.custom, o.custom) && len(p.states) == len(o.states)
}

// blockMap renumbers runtime ids of one palette to another. A state missing from the target maps to
// its air id.
type blockMap struct {
	to      []uint32 // from id -> to id
	back    []uint32 // to id -> from id
	fromAir uint32   // air in the from palette
	toAir   uint32   // air in the to palette
}

// isAir reports whether id is air: in the target palette if to, else in the source one.
func (m *blockMap) isAir(id uint32, to bool) bool {
	if to {
		return id == m.fromAir
	}
	return id == m.toAir
}

func newBlockMap(from, to *blockPalette) *blockMap {
	air := to.index[blockState{Name: "minecraft:air", Properties: encodeProperties(nil)}]
	fromAir := from.index[blockState{Name: "minecraft:air", Properties: encodeProperties(nil)}]
	m := &blockMap{to: make([]uint32, len(from.states)), back: make([]uint32, len(to.states)), fromAir: fromAir, toAir: air}
	for i := range m.back {
		m.back[i] = fromAir
	}
	// A custom block with one state on both sides matches by name: Dragonfly announces the Ores
	// Plus blocks without properties, Geyser with its fixed note block ones (instrument=harp;
	// note=N;powered=true), and an exact match only would turn them into air (invisible items).
	single := map[string]uint32{}
	count := map[string]int{}
	for i, s := range to.states {
		if count[s.Name]++; count[s.Name] == 1 {
			single[s.Name] = uint32(i)
		}
	}
	fromCount := map[string]int{}
	for _, s := range from.states {
		fromCount[s.Name]++
	}
	for i, s := range from.states {
		id, ok := to.index[s]
		if !ok && count[s.Name] == 1 && fromCount[s.Name] == 1 {
			id, ok = single[s.Name], true
		}
		if !ok {
			id = air
		}
		m.to[i] = id
		if ok {
			m.back[id] = uint32(i)
		}
	}
	return m
}

// forward renumbers a backend id for the client; reverse a client id for the backend.
func (m *blockMap) forward(id uint32) uint32 {
	if int(id) < len(m.to) {
		return m.to[id]
	}
	return id
}

func (m *blockMap) reverse(id uint32) uint32 {
	if int(id) < len(m.back) {
		return m.back[id]
	}
	return id
}

// PaletteNames lists the block names a client numbers by, given StartGame's custom blocks (for
// tests and tools).
func PaletteNames(blocks []protocol.BlockEntry) ([]string, error) {
	p, err := newBlockPalette(blocks)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(p.states))
	for i, s := range p.states {
		names[i] = s.Name
	}
	return names, nil
}

// ChunkPaletteIDs lists the block runtime ids in the palettes of n network-encoded sub-chunks (for
// tests and tools).
func ChunkPaletteIDs(data []byte, n int) ([]uint32, error) {
	var ids []uint32
	_, err := rewriteSubChunks(data, n, func(id uint32) uint32 {
		ids = append(ids, id)
		return id
	})
	return ids, err
}
