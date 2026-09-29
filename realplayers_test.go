package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRealPlayerCount: with no fake count, the Friends tab follows the backend's own count, with
// the floor of 1, a change reported only when there is one, and a failed read keeping the last value.
func TestRealPlayerCount(t *testing.T) {
	var players atomic.Int64
	var down atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"name":"x","protocol":1,"version":"1","level":"x","players":%d,"maxPlayers":30,"gameType":0}`, players.Load())
	}))
	defer backend.Close()

	cfg := FileConfig{BackendTransport: "nethernet-norelay", NetherNetBackendAddress: backend.URL}
	if !advertisesRealPlayers(cfg) {
		t.Fatal("no fake count and no drift should advertise the real count")
	}
	realPlayers.Store(-1)
	if got := advertisedPlayers(cfg); got != 1 {
		t.Errorf("before any read: %d, want the floor of 1", got)
	}

	ctx := context.Background()
	step := func(n int64, wantChanged bool, want int) {
		t.Helper()
		players.Store(n)
		changed, err := refreshRealPlayers(ctx, cfg)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if changed != wantChanged {
			t.Errorf("players %d: changed=%v, want %v", n, changed, wantChanged)
		}
		if got := advertisedPlayers(cfg); got != want {
			t.Errorf("players %d: advertised %d, want %d", n, got, want)
		}
	}
	step(5, true, 5)
	step(5, false, 5) // no change, nothing to push
	step(0, true, 1)  // empty server still shows the floor
	step(12, true, 12)

	down.Store(true)
	if _, err := refreshRealPlayers(ctx, cfg); err == nil {
		t.Error("a backend that is down read as a success")
	}
	if got := advertisedPlayers(cfg); got != 12 {
		t.Errorf("after a failed read: advertised %d, want the last good value 12", got)
	}

	if advertisesRealPlayers(FileConfig{FakePlayerCount: 21}) {
		t.Error("a fake count must win over the real one")
	}
}
