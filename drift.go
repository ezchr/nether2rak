package main

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gameparrot/netherconnect/bridge"
)

// PlayerDriftConfig makes the advertised Friends-tab player count wander inside a range instead
// of sitting on one fixed number. See FileConfig.FakePlayerDrift.
type PlayerDriftConfig struct {
	// Min and Max bound the advertised count (inclusive).
	Min int `json:"min"`
	Max int `json:"max"`
	// MaxStep is the most the count moves per session update (update_interval_seconds). Each
	// update picks a random move in [-MaxStep, +MaxStep], so it sometimes holds still. Default 1.
	MaxStep int `json:"max_step"`
}

// playerCountDrift is the advertised player count when fake_player_drift is on. There is one per
// process: the primary and every extra broadcast show the same number, and it survives session
// rebuilds (token renewals, signaling drops), so a rebuild does not snap it back to its start.
//
// It advances lazily by elapsed time rather than per call, so any number of broadcasts can read
// it on their own tickers without moving it faster.
type playerCountDrift struct {
	mu       sync.Mutex
	min, max int
	maxStep  int
	interval time.Duration
	value    int
	last     time.Time
	now      func() time.Time
	randMove func(maxStep int) int // uniform in [-maxStep, +maxStep]
}

// newPlayerCountDrift returns nil when cfg does not enable drift.
func newPlayerCountDrift(cfg FileConfig, now func() time.Time, randMove func(int) int) *playerCountDrift {
	d := cfg.FakePlayerDrift
	if d == nil || d.Max <= 0 {
		return nil
	}
	lo, hi := d.Min, d.Max
	if lo < 1 {
		lo = 1 // the host account itself is always a member
	}
	// A count at or above MaxMemberCount shows the world as full, and a full world can't be
	// joined - MCXboxBroadcast guards the same way (maxPlayers is forced above players).
	if cfg.MaxPlayers > 1 && hi > cfg.MaxPlayers-1 {
		hi = cfg.MaxPlayers - 1
	}
	if lo > hi {
		lo = hi
	}
	step := d.MaxStep
	if step < 1 {
		step = 1
	}
	interval := time.Duration(cfg.UpdateIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	start := (lo + hi) / 2
	if cfg.FakePlayerCount > 0 {
		start = min(max(cfg.FakePlayerCount, lo), hi)
	}
	return &playerCountDrift{
		min: lo, max: hi, maxStep: step, interval: interval,
		value: start, last: now(), now: now, randMove: randMove,
	}
}

// Current returns the count to advertise now, first applying one random move per whole update
// interval elapsed since the last move.
func (d *playerCountDrift) Current() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	elapsed := d.now().Sub(d.last)
	moves := int(elapsed / d.interval)
	if moves <= 0 {
		return d.value
	}
	d.last = d.last.Add(time.Duration(moves) * d.interval)
	// After a long idle stretch nothing reads the intermediate values; a handful of moves is
	// indistinguishable from thousands.
	moves = min(moves, 8)
	for range moves {
		d.value = min(max(d.value+d.randMove(d.maxStep), d.min), d.max)
	}
	return d.value
}

var (
	sharedDriftOnce sync.Once
	sharedDrift     *playerCountDrift
)

// sharedPlayerDrift returns the process-wide drift, built from the first config it is given (every
// broadcast in a process shares one config.json), or nil when drift is off.
func sharedPlayerDrift(cfg FileConfig) *playerCountDrift {
	sharedDriftOnce.Do(func() {
		sharedDrift = newPlayerCountDrift(cfg, time.Now, func(n int) int { return rand.IntN(2*n+1) - n })
	})
	return sharedDrift
}

// advertisedPlayers is the player count shown on the Friends tab right now.
//
// It must never be 0: MemberCount has to agree with the session's own member list, which always
// contains the host account - a live working session showed MemberCount:1 with nobody connected,
// and 0 plausibly got the world filtered from the Friends tab.
func advertisedPlayers(cfg FileConfig) int {
	if d := sharedPlayerDrift(cfg); d != nil {
		return d.Current()
	}
	if cfg.FakePlayerCount > 0 {
		return cfg.FakePlayerCount
	}
	// The real count, as last read by refreshRealPlayers. Unknown (never read yet) and 0 both
	// show as 1, for the reason above.
	return max(int(realPlayers.Load()), 1)
}

// realPlayerPollInterval is how often the backend's real player count is read when it is the one
// being advertised. Faster than update_interval_seconds on purpose: a change is pushed to Xbox
// Live as soon as it is seen (see runSession), while the regular session update keeps its own
// slower pace, so the Friends tab follows joins and leaves closely without every tick writing.
const realPlayerPollInterval = 15 * time.Second

// realPlayers is the backend's player count as last read, shared by every broadcast in the
// process. -1 until the first successful read; a failed read keeps the previous value.
var realPlayers atomic.Int64

func init() { realPlayers.Store(-1) }

// advertisesRealPlayers reports whether the Friends tab shows the backend's real count: neither
// fake_player_count nor fake_player_drift is set.
func advertisesRealPlayers(cfg FileConfig) bool {
	return sharedPlayerDrift(cfg) == nil && cfg.FakePlayerCount <= 0
}

// refreshRealPlayers reads the backend's player count and reports whether it changed.
func refreshRealPlayers(ctx context.Context, cfg FileConfig) (changed bool, err error) {
	n, err := bridge.BackendPlayers(ctx, bridge.Config{
		BackendTransport:        cfg.BackendTransport,
		NetherNetBackendAddress: cfg.NetherNetBackendAddress,
		GeyserAddress:           cfg.GeyserAddress,
	})
	if err != nil {
		return false, err
	}
	return realPlayers.Swap(int64(n)) != int64(n), nil
}
