package bridge

import (
	"reflect"
	"time"
	"unsafe"

	"github.com/df-mc/go-nethernet"
	"github.com/pion/webrtc/v4"
)

// clientRoundTrip is a player's network round trip to the relay. For a NetherNet player it is
// the round trip WebRTC's own connectivity checks (STUN binding requests, every few seconds)
// measure: the device's network stack answers them at once, so this is the real network ping,
// the figure Dragonfly shows for a player who joins it directly. go-nethernet's Latency() is
// half the SCTP round trip, which the client's delayed acknowledgements inflate, and how much
// depends on how much the server sends: it read 25-50 ms on Dragonfly after a move for a player
// whose network ping is 10 ms. That is still the reading for RakNet players, and until the first
// check is answered.
func clientRoundTrip(r latencyReporter) time.Duration {
	if n, ok := r.(*nethernet.Conn); ok {
		if d, ok := iceRoundTrip(n); ok {
			return d
		}
	}
	return r.Latency() * 2
}

// iceRoundTrip reads the selected candidate pair's round trip from n's ICE transport, which
// go-nethernet keeps in an unexported field (read with reflection: no library is patched).
func iceRoundTrip(n *nethernet.Conn) (time.Duration, bool) {
	f := reflect.ValueOf(n).Elem().FieldByName("ice")
	if !f.IsValid() {
		return 0, false
	}
	ice, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*webrtc.ICETransport)
	if !ok || ice == nil {
		return 0, false
	}
	stats, ok := ice.GetSelectedCandidatePairStats()
	if !ok || stats.CurrentRoundTripTime <= 0 {
		return 0, false
	}
	return time.Duration(stats.CurrentRoundTripTime * float64(time.Second)), true
}
