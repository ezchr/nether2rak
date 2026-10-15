package bridge

import (
	"testing"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestSettleHoldsBackOldPositions(t *testing.T) {
	s := &switchSession{}
	spawn := mgl32.Vec3{0.5, 1.62, 0.5}
	s.settleLocked(spawn)
	input := func(pos mgl32.Vec3) []byte {
		_, payload, err := splitPacket(encodePacket(&packet.PlayerAuthInput{Pitch: 3, Yaw: 90, Position: pos}))
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	for _, pos := range []mgl32.Vec3{{-6.5, 100.62, 2.9}, {2000.5, 1.62, 2000.5}, {-1999.5, 1.62, -1999.5}} {
		if !s.unsettled(input(pos)) {
			t.Fatalf("position %v from before the spawn was let through", pos)
		}
	}
	if s.unsettled(input(mgl32.Vec3{0.5, 1.62, 0.5})) {
		t.Fatal("position at the spawn was held back")
	}
	if s.settling.Load() {
		t.Fatal("still settling after the client reached the spawn")
	}

	s.settleLocked(spawn)
	s.settleUntil = time.Now().Add(-time.Second)
	if s.unsettled(input(mgl32.Vec3{500, 70, 500})) {
		t.Fatal("held back after the wait ran out")
	}
}
