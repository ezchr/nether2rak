package main

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func driftCfg(min, max, step, fake, maxPlayers int) FileConfig {
	return FileConfig{
		MaxPlayers:            maxPlayers,
		FakePlayerCount:       fake,
		UpdateIntervalSeconds: 30,
		FakePlayerDrift:       &PlayerDriftConfig{Min: min, Max: max, MaxStep: step},
	}
}

func TestDriftDisabled(t *testing.T) {
	if newPlayerCountDrift(FileConfig{FakePlayerCount: 21}, time.Now, nil) != nil {
		t.Fatal("no fake_player_drift must mean no drift")
	}
	if newPlayerCountDrift(driftCfg(0, 0, 2, 21, 30), time.Now, nil) != nil {
		t.Fatal("max 0 must mean no drift")
	}
}

func TestDriftStartsAtFakeCountClamped(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	cases := []struct{ fake, want int }{{21, 21}, {5, 18}, {99, 26}, {0, 22}} // 0 -> middle of 18..26
	for _, c := range cases {
		d := newPlayerCountDrift(driftCfg(18, 26, 2, c.fake, 30), clock.now, func(int) int { return 0 })
		if got := d.Current(); got != c.want {
			t.Errorf("fake %d: start %d, want %d", c.fake, got, c.want)
		}
	}
}

func TestDriftMovesOncePerInterval(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	d := newPlayerCountDrift(driftCfg(10, 30, 2, 20, 40), clock.now, func(int) int { calls++; return 1 })

	for range 5 { // several broadcasts reading within one interval must not move it
		if got := d.Current(); got != 20 {
			t.Fatalf("moved before an interval elapsed: %d", got)
		}
	}
	clock.advance(29 * time.Second)
	if d.Current() != 20 {
		t.Fatal("moved before a whole interval")
	}
	clock.advance(1 * time.Second)
	if got := d.Current(); got != 21 {
		t.Fatalf("after one interval got %d, want 21", got)
	}
	clock.advance(90 * time.Second) // three intervals
	if got := d.Current(); got != 24 {
		t.Fatalf("after three more intervals got %d, want 24", got)
	}
	if calls != 4 {
		t.Fatalf("random move drawn %d times, want 4", calls)
	}
}

func TestDriftStaysInRangeAndBelowMax(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	// Range reaches max_players: the ceiling must drop to max_players-1 so the world never reads full.
	up := newPlayerCountDrift(driftCfg(20, 30, 3, 25, 30), clock.now, func(n int) int { return n })
	down := newPlayerCountDrift(driftCfg(20, 30, 3, 25, 30), clock.now, func(n int) int { return -n })
	for range 50 {
		clock.advance(30 * time.Second)
		if v := up.Current(); v > 29 {
			t.Fatalf("drifted to %d, must stay below max_players 30", v)
		}
		if v := down.Current(); v < 20 {
			t.Fatalf("drifted to %d, below min 20", v)
		}
	}
	if up.Current() != 29 || down.Current() != 20 {
		t.Fatalf("expected to settle at the bounds, got %d / %d", up.Current(), down.Current())
	}
}

func TestDriftBadRangeAndStepDefaults(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var gotStep int
	d := newPlayerCountDrift(driftCfg(40, 35, 0, 0, 30), clock.now, func(n int) int { gotStep = n; return 0 })
	clock.advance(30 * time.Second)
	if v := d.Current(); v != 29 {
		t.Fatalf("min above max, both above max_players: got %d, want 29", v)
	}
	if gotStep != 1 {
		t.Fatalf("max_step 0 should default to 1, got %d", gotStep)
	}
}
