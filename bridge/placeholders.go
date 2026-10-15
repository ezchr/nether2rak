package bridge

import (
	"math"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// A move into a server that uses sub-chunk requests (Dragonfly) puts the relay's own empty chunks
// in request mode around the spawn, and the client asks for their sub-chunks straight away. The
// server answers "chunk not found" for every column it hasn't loaded for the player yet, and the
// client then waits seconds before asking again: the move took 10 s or more (and timed out) when
// the server had nothing loaded at its spawn. The relay answers requests for its own columns with
// air itself, until the server's real chunk for a column passes (track).

// markPlaceholders records the columns around pos the relay just sent its own request-mode chunks
// for. Call it with s.mu held.
func (s *switchSession) markPlaceholders(pos mgl32.Vec3) {
	cx, cz := int32(math.Floor(float64(pos[0])))>>4, int32(math.Floor(float64(pos[2])))>>4
	s.placeholders = map[[2]int32]bool{}
	for x := int32(-3); x <= 3; x++ {
		for z := int32(-3); z <= 3; z++ {
			s.placeholders[[2]int32{cx + x, cz + z}] = true
		}
	}
}

// placeholderSubChunks answers the part of req that is for the relay's own columns with air, and
// returns the rest for the server, or nil if nothing is left.
func (s *switchSession) placeholderSubChunks(req *packet.SubChunkRequest) *packet.SubChunkRequest {
	s.mu.Lock()
	if len(s.placeholders) == 0 {
		s.mu.Unlock()
		return req
	}
	offsets := req.Offsets
	if len(offsets) == 0 {
		offsets = []protocol.SubChunkOffset{{0, 0, 0}}
	}
	var air, keep []protocol.SubChunkOffset
	for _, o := range offsets {
		if s.placeholders[[2]int32{req.Position[0] + int32(o[0]), req.Position[2] + int32(o[2])}] {
			air = append(air, o)
		} else {
			keep = append(keep, o)
		}
	}
	s.mu.Unlock()
	if len(air) > 0 {
		a := *req
		a.Offsets = air
		s.sendClient(airSubChunks(&a))
	}
	if len(keep) == 0 {
		return nil
	}
	if len(air) == 0 {
		return req
	}
	r := *req
	r.Offsets = keep
	return &r
}
