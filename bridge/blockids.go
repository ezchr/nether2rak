package bridge

import (
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// uniqueBlockIDs numbers the custom blocks of the union again, in order: the ones numbered from
// 10000 (vanilla_block_data block_id) get 10000, 10001, 10002... in the order the union lists them.
// Each server numbers its own custom blocks that way, from 10000, so in the union Dragonfly's Ores
// Plus blocks and Geyser's player skull blocks shared numbers, and the client drew one block of each
// such pair's item as nothing. Giving only the duplicates new numbers past the end (out of order)
// broke more: Bedrock chest menus on PvP stopped opening. In order, like each server's own list, it is
// what the client expects. Entries numbered below 10000 (vanilla-style ids) keep theirs, as do entries
// without the field (removing it re-sorted the client's palette: every block came out wrong). The ids
// are only the client's: the relay maps blocks by name and the servers never see the union's numbers.
func uniqueBlockIDs(blocks []protocol.BlockEntry) []protocol.BlockEntry {
	next := int64(10000)
	for i, b := range blocks {
		id, ok := blockID(b)
		if !ok || id < 10000 {
			continue
		}
		if id != next {
			blocks[i].Properties = withBlockID(b.Properties, next)
		}
		next++
	}
	return blocks
}

func blockID(b protocol.BlockEntry) (int64, bool) {
	v, ok := b.Properties["vanilla_block_data"].(map[string]any)
	if !ok {
		return 0, false
	}
	switch n := v["block_id"].(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case int16:
		return int64(n), true
	case uint8:
		return int64(n), true
	}
	return 0, false
}

// withBlockID copies props with vanilla_block_data's block_id set to id, keeping its NBT type.
func withBlockID(props map[string]any, id int64) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = v
	}
	old := props["vanilla_block_data"].(map[string]any)
	vbd := make(map[string]any, len(old))
	for k, v := range old {
		vbd[k] = v
	}
	switch old["block_id"].(type) {
	case int64:
		vbd["block_id"] = id
	case int16:
		vbd["block_id"] = int16(id)
	default:
		vbd["block_id"] = int32(id)
	}
	out["vanilla_block_data"] = vbd
	return out
}
