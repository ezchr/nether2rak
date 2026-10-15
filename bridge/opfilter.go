package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// What a vanilla server sends every player, operator or not, and a player who is not an admin of
// the relay has no use for: the command and last output of every command block in the chunks
// around them, and the commands behind the buttons of every NPC they can see. A world-saving
// proxy reads both straight off the wire. The relay blanks them for everyone but its admins; the
// blocks and the NPCs work the same, as the server runs its own copy.

// commandBlockID is the id of a command block's block entity, whatever kind of command block.
var commandBlockID = []byte("CommandBlock")

// scrubbedTags are the string tags of a command block that are emptied.
var scrubbedTags = [][]byte{nbtStringTag("Command"), nbtStringTag("LastOutput")}

// nbtStringTag is the start of a string tag with the name passed in network NBT: the tag type, the
// length of the name and the name. The length of the value and the value follow it.
func nbtStringTag(name string) []byte {
	return append([]byte{8, byte(len(name))}, name...)
}

// scrubForPlayer returns the packet with what only operators should read blanked, or raw itself
// if there is nothing of that kind in it.
func scrubForPlayer(raw []byte) []byte {
	id, payload, err := splitPacket(raw)
	if err != nil {
		return raw
	}
	switch id {
	case packet.IDLevelChunk:
		if !bytes.Contains(payload, commandBlockID) {
			return raw
		}
		var pk packet.LevelChunk
		if decodeInto(&pk, payload) != nil {
			return raw
		}
		out, n := scrubCommandTags(pk.RawPayload)
		if n == 0 {
			return raw
		}
		pk.RawPayload = out
		return encodePacket(&pk)
	case packet.IDSubChunk:
		if !bytes.Contains(payload, commandBlockID) {
			return raw
		}
		var pk packet.SubChunk
		if decodeInto(&pk, payload) != nil {
			return raw
		}
		changed := false
		for i, e := range pk.SubChunkEntries {
			data, ok := e.RawPayload.Value()
			if !ok {
				continue
			}
			if out, n := scrubCommandTags(data); n > 0 {
				pk.SubChunkEntries[i].RawPayload = protocol.Option(out)
				changed = true
			}
		}
		if !changed {
			return raw
		}
		return encodePacket(&pk)
	case packet.IDBlockActorData:
		if !bytes.Contains(payload, commandBlockID) {
			return raw
		}
		var pk packet.BlockActorData
		if decodeInto(&pk, payload) != nil {
			return raw
		}
		if id, _ := pk.NBTData["id"].(string); id != string(commandBlockID) {
			return raw
		}
		pk.NBTData["Command"], pk.NBTData["LastOutput"] = "", ""
		if _, ok := pk.NBTData["LastOutputParams"]; ok {
			pk.NBTData["LastOutputParams"] = []any{}
		}
		return encodePacket(&pk)
	case packet.IDAddActor:
		if !bytes.Contains(payload, npcCommandMark) {
			return raw
		}
		var pk packet.AddActor
		if decodeInto(&pk, payload) != nil || !scrubNPCMetadata(pk.EntityMetadata) {
			return raw
		}
		return encodePacket(&pk)
	case packet.IDSetActorData:
		if !bytes.Contains(payload, npcCommandMark) {
			return raw
		}
		var pk packet.SetActorData
		if decodeInto(&pk, payload) != nil || !scrubNPCMetadata(pk.EntityMetadata) {
			return raw
		}
		return encodePacket(&pk)
	case packet.IDNPCDialogue:
		if !bytes.Contains(payload, npcCommandMark) {
			return raw
		}
		var pk packet.NPCDialogue
		if decodeInto(&pk, payload) != nil {
			return raw
		}
		out, changed := scrubNPCActions(pk.ActionJSON)
		if !changed {
			return raw
		}
		pk.ActionJSON = out
		return encodePacket(&pk)
	}
	return raw
}

// scrubCommandTags empties the command and last output of the command blocks in data, which is
// chunk data ending in block entities, and reports how many tags it emptied. Network NBT has no
// lengths but those of its strings, so a value can be cut out where it stands.
func scrubCommandTags(data []byte) ([]byte, int) {
	if !bytes.Contains(data, commandBlockID) {
		return data, 0
	}
	n := 0
	for _, tag := range scrubbedTags {
		from := 0
		for {
			i := bytes.Index(data[from:], tag)
			if i < 0 {
				break
			}
			at := from + i + len(tag) // the value's length, a varuint32
			length, size, ok := readVaruint32(data[at:])
			if !ok || at+size+int(length) > len(data) {
				from = at
				continue
			}
			if length > 0 {
				out := make([]byte, 0, len(data)-int(length))
				out = append(out, data[:at]...)
				out = append(out, 0)
				out = append(out, data[at+size+int(length):]...)
				data = out
				n++
			}
			from = at + 1
		}
	}
	return data, n
}

func readVaruint32(b []byte) (v uint32, size int, ok bool) {
	for i := 0; i < 5 && i < len(b); i++ {
		v |= uint32(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

// npcCommandMark is in the actions of every NPC with a command behind a button.
var npcCommandMark = []byte("cmd_line")

// scrubNPCMetadata blanks the commands in an NPC's actions, and reports whether it changed them.
func scrubNPCMetadata(metadata map[uint32]any) bool {
	actions, ok := metadata[protocol.EntityDataKeyActions].(string)
	if !ok {
		return false
	}
	out, changed := scrubNPCActions(actions)
	if changed {
		metadata[protocol.EntityDataKeyActions] = out
	}
	return changed
}

// scrubNPCActions blanks the command of every command button in an NPC's actions (a JSON list),
// keeping the buttons: the client shows a button by its name and tells the server which one was
// pressed, by its place in the list. Actions that are not a list of buttons become an empty list.
func scrubNPCActions(actions string) (string, bool) {
	if actions == "" {
		return actions, false
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(actions), &list); err != nil {
		return "[]", true
	}
	changed := false
	for _, a := range list {
		// Type 0 is a link, shown to the player; 1 is a command.
		if t, _ := a["type"].(float64); t == 0 {
			continue
		}
		if text, _ := a["text"].(string); text != "" {
			a["text"] = ""
			changed = true
		}
		if d, ok := a["data"].([]any); ok && len(d) > 0 {
			a["data"] = []any{}
			changed = true
		}
	}
	if !changed {
		return actions, false
	}
	out, err := json.Marshal(list)
	if err != nil {
		return "[]", true
	}
	return string(out), true
}

// opFilterDebug (env N2R_OPFILTER_DEBUG) also counts, for admins, the packets that would have been
// blanked, so a test can compare what an admin and a player are sent.
var opFilterDebug = os.Getenv("N2R_OPFILTER_DEBUG") != ""

// noteScrubbed counts a packet that had something to blank and logs the first few of each kind.
// Called from the one goroutine that forwards a link's packets.
func (s *switchSession) noteScrubbed(raw []byte) {
	id, _, err := splitPacket(raw)
	if err != nil {
		return
	}
	name := fmt.Sprintf("packet %d", id)
	switch id {
	case packet.IDLevelChunk:
		name = "LevelChunk"
	case packet.IDSubChunk:
		name = "SubChunk"
	case packet.IDBlockActorData:
		name = "BlockActorData"
	case packet.IDAddActor:
		name = "AddActor"
	case packet.IDSetActorData:
		name = "SetActorData"
	case packet.IDNPCDialogue:
		name = "NPCDialogue"
	}
	if s.scrubbed == nil {
		s.scrubbed = map[string]int{}
	}
	s.scrubbed[name]++
	if n := s.scrubbed[name]; n == 1 || n == 10 || n == 100 || n == 1000 {
		s.log.Info("opfilter: operator-only data in packets", "blanked", s.scrub, "packet", name, "count", n, "totals", fmt.Sprint(s.scrubbed))
	}
}
