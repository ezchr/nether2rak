package bridge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func networkNBT(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := nbt.MarshalEncoding(v, nbt.NetworkLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeAll decodes the compounds that follow one another in data.
func decodeAll(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := nbt.NewDecoderWithEncoding(bytes.NewBuffer(data), nbt.NetworkLittleEndian)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		out = append(out, m)
	}
	return out
}

func commandBlock(command, output string) map[string]any {
	return map[string]any{
		"id": "CommandBlock", "x": int32(4), "y": int32(70), "z": int32(-9),
		"Command": command, "LastOutput": output, "CustomName": "Gate", "TrackOutput": byte(1), "TickDelay": int32(20),
	}
}

func TestScrubCommandTags(t *testing.T) {
	long := "/execute as @a[tag=vip] run give @s diamond 64 " + strings.Repeat("x", 300) // a length of two bytes
	chest := map[string]any{"id": "Chest", "x": int32(1), "y": int32(2), "z": int32(3), "CustomName": "Command"}
	prefix := []byte{9, 1, 0xfc, 1, 0x80, 0x90} // stands in for the sub-chunks and biomes before the block entities
	data := append([]byte(nil), prefix...)
	data = append(data, networkNBT(t, chest)...)
	data = append(data, networkNBT(t, commandBlock("/say secret", "done"))...)
	data = append(data, networkNBT(t, commandBlock(long, ""))...)

	out, n := scrubCommandTags(data)
	if n != 3 {
		t.Fatalf("%d tags emptied, want 3 (two commands, one output)", n)
	}
	if !bytes.HasPrefix(out, prefix) {
		t.Fatal("the data before the block entities changed")
	}
	got := decodeAll(t, out[len(prefix):])
	if len(got) != 3 {
		t.Fatalf("%d block entities decode after the scrub, want 3", len(got))
	}
	if got[0]["CustomName"] != "Command" || got[0]["id"] != "Chest" {
		t.Errorf("the chest changed: %v", got[0])
	}
	for _, cb := range got[1:] {
		if cb["Command"] != "" || cb["LastOutput"] != "" {
			t.Errorf("command block not emptied: %v", cb)
		}
		if cb["CustomName"] != "Gate" || cb["TickDelay"] != int32(20) || cb["x"] != int32(4) || cb["TrackOutput"] != byte(1) {
			t.Errorf("command block lost other data: %v", cb)
		}
	}
	if bytes.Contains(out, []byte("secret")) || bytes.Contains(out, []byte("diamond")) {
		t.Error("command text is still in the data")
	}

	// Nothing to do: the very same data comes back.
	plain := append(append([]byte(nil), prefix...), networkNBT(t, chest)...)
	if same, n := scrubCommandTags(plain); n != 0 || &same[0] != &plain[0] {
		t.Errorf("data without a command block was touched (%d tags)", n)
	}
	// Scrubbing twice changes nothing more.
	if again, n := scrubCommandTags(out); n != 0 || !bytes.Equal(again, out) {
		t.Errorf("a second scrub emptied %d tags", n)
	}
	// A tag cut short does not panic or loop.
	cut := append(append([]byte(nil), commandBlockID...), nbtStringTag("Command")...)
	cut = append(cut, 0xff)
	if _, n := scrubCommandTags(cut); n != 0 {
		t.Errorf("a cut tag counted as emptied")
	}
}

func TestScrubForPlayerPackets(t *testing.T) {
	chunkData := append([]byte{0xaa, 0xbb}, networkNBT(t, commandBlock("/op @p", "ok"))...)
	chunk := encodePacket(&packet.LevelChunk{Position: protocol.ChunkPos{3, 4}, SubChunkCount: 1, RawPayload: chunkData})
	out := scrubForPlayer(chunk)
	if bytes.Contains(out, []byte("/op @p")) {
		t.Error("LevelChunk still carries the command")
	}
	_, payload, _ := splitPacket(out)
	var lc packet.LevelChunk
	if err := decodeInto(&lc, payload); err != nil || lc.Position != (protocol.ChunkPos{3, 4}) || lc.SubChunkCount != 1 {
		t.Errorf("LevelChunk after the scrub: %+v, err %v", lc.Position, err)
	}

	bad := encodePacket(&packet.BlockActorData{Position: protocol.BlockPos{1, 2, 3}, NBTData: commandBlock("/kill @a", "x")})
	out = scrubForPlayer(bad)
	_, payload, _ = splitPacket(out)
	var ba packet.BlockActorData
	if err := decodeInto(&ba, payload); err != nil {
		t.Fatal(err)
	}
	if ba.NBTData["Command"] != "" || ba.NBTData["LastOutput"] != "" || ba.NBTData["CustomName"] != "Gate" || ba.Position != (protocol.BlockPos{1, 2, 3}) {
		t.Errorf("BlockActorData after the scrub: %v", ba.NBTData)
	}

	sign := encodePacket(&packet.BlockActorData{NBTData: map[string]any{"id": "Sign", "Text": "CommandBlock"}})
	if got := scrubForPlayer(sign); !bytes.Equal(got, sign) {
		t.Error("a sign was changed")
	}
	text := encodePacket(&packet.Text{TextType: packet.TextTypeRaw, Message: "CommandBlock cmd_line"})
	if got := scrubForPlayer(text); &got[0] != &text[0] {
		t.Error("another packet was touched")
	}
}

func TestScrubNPCActions(t *testing.T) {
	actions := `[{"button_name":"Shop","data":[{"cmd_line":"give @s diamond","cmd_ver":44}],"mode":0,"text":"give @s diamond","type":1},` +
		`{"button_name":"Site","data":null,"mode":0,"text":"https://example.com","type":0},` +
		`{"button_name":"On open","data":[{"cmd_line":"tag @s add met","cmd_ver":44}],"mode":2,"text":"tag @s add met","type":1}]`
	out, changed := scrubNPCActions(actions)
	if !changed {
		t.Fatal("nothing changed")
	}
	if strings.Contains(out, "diamond") || strings.Contains(out, "tag @s") || strings.Contains(out, "cmd_line") {
		t.Errorf("commands left: %s", out)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 3 {
		t.Fatalf("%d buttons after the scrub (%v): %s", len(list), err, out)
	}
	for i, name := range []string{"Shop", "Site", "On open"} {
		if list[i]["button_name"] != name {
			t.Errorf("button %d is %v, want %s", i, list[i]["button_name"], name)
		}
	}
	if list[1]["text"] != "https://example.com" {
		t.Errorf("the link of a link button was blanked: %v", list[1])
	}
	if list[2]["mode"] != float64(2) || list[0]["type"] != float64(1) {
		t.Errorf("mode or type changed: %v", list)
	}
	if again, changed := scrubNPCActions(out); changed || again != out {
		t.Error("a second scrub changed the actions")
	}
	if got, changed := scrubNPCActions(""); changed || got != "" {
		t.Error("empty actions changed")
	}
	if got, changed := scrubNPCActions("not json cmd_line"); !changed || got != "[]" {
		t.Errorf("unreadable actions = %q, want an empty list", got)
	}

	meta := map[uint32]any{protocol.EntityDataKeyActions: actions, protocol.EntityDataKeyName: "Trader"}
	if !scrubNPCMetadata(meta) || strings.Contains(meta[protocol.EntityDataKeyActions].(string), "diamond") || meta[protocol.EntityDataKeyName] != "Trader" {
		t.Errorf("metadata after the scrub: %v", meta)
	}
	if scrubNPCMetadata(map[uint32]any{protocol.EntityDataKeyName: "Cow"}) {
		t.Error("an entity without actions changed")
	}
}
