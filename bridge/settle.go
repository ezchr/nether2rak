package bridge

import (
	"encoding/binary"
	"math"
	"os"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// During a move the client still stands where it stood on the server it leaves, and then at the
// relay's own places far away (transfer.go), before it is put at the new server's spawn. Its
// movement packets say so, and the new server took them for real movement: from a lobby at y 99 to
// a spawn at y 0 was a fall of 99 blocks, and the player was dead on arrival, with an empty heart
// bar and no death screen. So after a move the client's movement packets are held back until one
// reports a position at the new spawn, or settleWait at most.
//
// Only for a move to a server that sends sub-chunks on request (Dragonfly), which is where the
// fall happened. Paper behind Geyser puts a returning player at their last location, far from the
// spawn it announces: held back there, their packets were lost for the whole wait and Geyser was
// left out of step (blocks broke without effect, no container or inventory opened).

const (
	settleWait  = 5 * time.Second
	settleRange = 6 // blocks from the spawn, each axis
)

// settleOff (N2R_SETTLE=off) holds nothing back and only logs what the client reports after a move,
// to see whether a client still sends its old position (testing Cinnabar fixes).
// N2R_SETTLE=log keeps holding them back and logs the same, to compare clients.
var settleOff = os.Getenv("N2R_SETTLE") == "off"
var settleLog = os.Getenv("N2R_SETTLE") == "log"

// settleLocked starts holding back movement packets until the client is at spawn. s.mu held.
func (s *switchSession) settleLocked(spawn mgl32.Vec3) {
	s.settlePos, s.settleUntil = spawn, time.Now().Add(settleWait)
	s.settleLogged = 0
	s.settling.Store(true)
}

// unsettled reports whether the PlayerAuthInput in payload is from before the client reached the
// spawn of the server it moved to, and must not reach that server.
func (s *switchSession) unsettled(payload []byte) bool {
	pos, ok := authInputPosition(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if settleOff {
		if time.Now().After(s.settleUntil) || s.settleLogged >= 40 {
			s.settling.Store(false)
			return false
		}
		s.settleLogged++
		s.logAuthInput(payload, pos, ok && nearSpawn(pos, s.settlePos), false)
		return false
	}
	near := ok && nearSpawn(pos, s.settlePos)
	if settleLog && s.settleLogged < 40 && !time.Now().After(s.settleUntil) {
		s.settleLogged++
		s.logAuthInput(payload, pos, near, ok && !near)
	}
	if !ok || time.Now().After(s.settleUntil) || near {
		s.settling.Store(false)
		return false
	}
	return true
}

func nearSpawn(pos, spawn mgl32.Vec3) bool {
	for i := range pos {
		if d := pos[i] - spawn[i]; d > settleRange || d < -settleRange {
			return false
		}
	}
	return true
}

// authInputPosition reads the position from a PlayerAuthInput payload: pitch, yaw, then x, y, z.
func authInputPosition(payload []byte) (mgl32.Vec3, bool) {
	if len(payload) < 20 {
		return mgl32.Vec3{}, false
	}
	var pos mgl32.Vec3
	for i := range pos {
		pos[i] = math.Float32frombits(binary.LittleEndian.Uint32(payload[8+4*i:]))
		if pos[i] != pos[i] {
			return mgl32.Vec3{}, false
		}
	}
	return pos, true
}

// logAuthInput logs one movement report in the test modes. s.mu held.
func (s *switchSession) logAuthInput(payload []byte, pos mgl32.Vec3, near, held bool) {
	var pk packet.PlayerAuthInput
	var flags []int
	var delta mgl32.Vec3
	var tick uint64
	if decodeInto(&pk, payload) == nil {
		for i := 0; i < pk.InputData.Len(); i++ {
			if pk.InputData.Load(i) {
				flags = append(flags, i)
			}
		}
		delta, tick = pk.Delta, pk.Tick
	}
	s.log.Info("settle test: position after move", "n", s.settleLogged, "pos", pos, "spawn", s.settlePos,
		"nearSpawn", near, "heldBack", held, "delta", delta, "tick", tick, "flags", flags)
}

// logSettlingPacket logs a packet other than a movement report that the client sends while
// settling, in the test modes.
func (s *switchSession) logSettlingPacket(id uint32) {
	if !(settleOff || settleLog) || !s.settling.Load() {
		return
	}
	s.log.Info("settle test: other client packet", "id", id)
}
