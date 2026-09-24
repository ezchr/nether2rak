package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gameparrot/netherconnect/bridge"
	"github.com/gameparrot/netherconnect/invitequeue"
	"github.com/gameparrot/netherconnect/xbl"
)

// This file is the host-account half of the "invite everyone from another server into ours"
// feature. The other half (finding out who to invite) is a completely separate, standalone
// binary - cmd/scraper - run under its OWN dedicated Xbox account, never this one. See
// cmd/scraper's package doc for why that split exists and how the two halves communicate.
//
// The split exists because only the account that owns a session can send real invites into it
// (Xbox Live checks the caller's own membership/rights against the session referenced - see
// xbl.Session.SendInvite's doc comment), while discovering who is playing on some arbitrary
// third-party server means actually joining that server as a live Bedrock client, repeatedly,
// which is a fundamentally different and riskier thing to do from the same account that's also
// hosting a Friends-tab world. Two accounts, two processes, one shared file.
//
// OFF BY DEFAULT. This does not run just because invite_queue.txt exists - it starts stopped
// every time the process starts, and only sends anything after an explicit "start" command,
// issued over a loopback-only HTTP control endpoint (same pattern as bridge/pingserver.go's
// existing ping API):
//
//	curl http://127.0.0.1:<InvitePort>/invites/start
//	curl http://127.0.0.1:<InvitePort>/invites/stop
//	curl http://127.0.0.1:<InvitePort>/invites/status
//
// Once started, it runs a REPEATING cycle, not a one-shot pass: every XUID currently in the
// queue file gets invited, then it immediately loops back to the start of the list and invites
// everyone again, continuing until "stop" is issued (or the process/session ends) - no pause
// between laps, only the per-invite rate limit within each lap. Someone who missed or dismissed
// an earlier invite gets another shot on the next lap.

// inviteQueueFile is the shared handoff point: cmd/scraper appends one "xuid,username" line per
// player as it discovers them on whatever server it's pointed at. A plain file rather than a
// network call (matches how this project already hands config/tokens around - see config.json,
// token.json) so nothing needs to be running on both ends at the same instant.
const inviteQueueFile = "invite_queue.txt"

// inviteRateLimit bounds how often a single invite is actually sent to Xbox Live, i.e. the
// minimum gap between any two invites within one lap (and, since a lap loops straight back to
// the top with no pause - see run's own comment - the minimum gap between two invites to the
// SAME person on consecutive laps too). Xbox Live's own real limits for this endpoint are not
// published. Raised from 3s to 10s 2026-09-16 after confirming delivery actually works: a fixed
// working invite really was arriving, but at 3s/invite the platform folds every repeat into one
// notification's count rather than showing anything new - and sending that fast, indefinitely,
// risks reading as an abuse pattern to Xbox Live over a long-running loop, independent of
// whether the recipient ever notices the difference.
const inviteRateLimit = 10 * time.Second

// inviteController owns the running/stopped state of the invite loop and the HTTP endpoint used
// to flip it. There is exactly one of these per process (one xblSession, one world to invite
// people into), constructed once in runSession and torn down with it.
type inviteController struct {
	xblSession *xbl.Session
	log        *slog.Logger

	mu      sync.Mutex
	running bool
	// stop, non-nil only while running, cancels the current invite loop without touching
	// sessionCtx - "stop" must not tear down the whole session, only this one feature.
	stop context.CancelFunc
}

// newInviteController constructs a controller in the stopped state - see this file's package doc
// for why starting stopped is load-bearing, not just a default.
func newInviteController(xblSession *xbl.Session, log *slog.Logger) *inviteController {
	l := log.With("src", "invite-watcher")
	// Prune once here, at construction, rather than only as part of the invite loop's own read
	// path - the loop only runs between "start" and "stop" (off by default, see this file's
	// package doc), so tying expiry to it would mean stale entries never get removed at all
	// while invites happen to be off. This runs unconditionally, once per process start,
	// independent of whether the loop is ever started - see cmd/scraper's own startup prune
	// for the other half of "independent of either process's uptime".
	invitequeue.Prune(inviteQueueFile, l)
	return &inviteController{xblSession: xblSession, log: l}
}

// Start begins the repeating invite-lap loop if it is not already running. parentCtx bounds the
// loop's maximum lifetime (tie it to sessionCtx so a session rebuild stops this too); Stop (or a
// second Start) can end it earlier. Safe to call when already running - it is then a no-op.
func (c *inviteController) Start(parentCtx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	loopCtx, cancel := context.WithCancel(parentCtx)
	c.stop = cancel
	c.running = true
	c.log.Info("invite loop started")
	go c.run(loopCtx)
}

// Stop ends the invite loop if running. Safe to call when already stopped.
func (c *inviteController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	c.stop()
	c.stop = nil
	c.running = false
	c.log.Info("invite loop stopped")
}

// Running reports whether the loop is currently active.
func (c *inviteController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// run is the actual repeating lap loop, started by Start in its own goroutine. Exits as soon as
// ctx is cancelled, whether by Stop or by the parent session ending.
func (c *inviteController) run(ctx context.Context) {
	for {
		xuids := readXUIDList(inviteQueueFile, c.log)
		if len(xuids) == 0 {
			c.log.Debug("invite queue empty, waiting", "path", inviteQueueFile)
		} else {
			c.log.Info("starting invite lap", "count", len(xuids))
			for _, xuid := range xuids {
				select {
				case <-ctx.Done():
					return
				case <-time.After(inviteRateLimit):
				}
				// Checked fresh right before each send, not once per lap - a rate-limited lap
				// over a long list can take a while, and someone can join partway through it.
				// Anyone already in our world gets a pointless invite otherwise.
				if bridge.ConnectedXUIDs()[xuid] {
					c.log.Debug("skipping invite - already connected", "xuid", xuid)
					continue
				}
				if err := c.xblSession.SendInvite(ctx, xuid); err != nil {
					c.log.Error("failed to send invite", "xuid", xuid, "err", err)
					// Keep going with the rest of the lap rather than aborting the whole cycle
					// over one failed invite (a stale/invalid XUID, a momentary API hiccup) -
					// this same XUID gets tried again on the next lap regardless.
				}
			}
			c.log.Info("invite lap complete", "count", len(xuids))
			continue
		}

		// Only reached when the queue is empty - no point spinning a tight loop re-reading an
		// empty file. A non-empty lap loops straight back to the top instead (see the "continue"
		// above), per explicit instruction to remove the pause between laps.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// startInviteControlServer serves the loopback-only start/stop/status endpoint for c. Mirrors
// bridge.StartPingServer's shape exactly (loopback bind, plain HTTP, no auth - relies on the
// port never being reachable off the VPS, same trust model as the ping API and pprof).
func startInviteControlServer(ctx context.Context, addr string, c *inviteController, friendsCtl *friendsInviteController, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/invites/start", func(w http.ResponseWriter, r *http.Request) {
		// ctx (this server's own sessionCtx param), not context.Background(): a loop rooted in
		// Background() outlives the session generation that started it. When the session rebuilds
		// (a new xblSession/inviteController/HTTP server, ~8x/day - see this function's own doc
		// comment on the HTTP server's shutdown wiring), the OLD controller's goroutine kept
		// running forever, invisible to and unstoppable by the NEW controller's /invites/stop -
		// confirmed live 2026-09-22: /invites/stop and /invites/status both correctly reported
		// "stopped" on the current generation while an orphaned goroutine from a prior generation
		// kept sending invites every 10s regardless, until the whole process was killed. Tying the
		// loop to ctx means a session rebuild cancels any in-flight loop along with the HTTP
		// server, so there is never more than one live goroutine and start/stop/status on the
		// currently-listening server always reflect the actual running state.
		c.Start(ctx)
		fmt.Fprintln(w, "started")
	})
	mux.HandleFunc("/invites/stop", func(w http.ResponseWriter, r *http.Request) {
		c.Stop()
		fmt.Fprintln(w, "stopped")
	})
	mux.HandleFunc("/invites/status", func(w http.ResponseWriter, r *http.Request) {
		if c.Running() {
			fmt.Fprintln(w, "running")
		} else {
			fmt.Fprintln(w, "stopped")
		}
	})
	// /invites/friends/* - see friendsinviter.go. Registered on this same mux/port rather than
	// opening a second listener, since it's the same control-plane trust model (loopback only).
	registerFriendsInviteRoutes(ctx, mux, friendsCtl)

	server := &http.Server{Addr: addr, Handler: mux}
	// ctx is sessionCtx from runSession's caller, not the process-lifetime ctx - this server is
	// tied to one specific xblSession/inviteCtl/friendsCtl generation (see the call site's own
	// comment on why), so it must go down with that generation rather than outliving it. Without
	// this, every session rebuild (roughly 8x/day in practice - token renewals, RTA drops, etc.)
	// spawned a brand new http.ListenAndServe on the same fixed port while the previous
	// generation's server kept running forever with nothing to ever stop it: only the first
	// server in the process's life ever actually held the port, and every later rebuild's
	// ListenAndServe failed immediately with "address already in use", permanently orphaning
	// that generation's invite controller from the loopback API (confirmed 2026-09-16 - paired
	// "listening"/"stopped: bind: address already in use" log lines on every rebuild after the
	// first, since the misleading "listening" line below logs before the bind is even attempted).
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		log.Info("invite control server listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Warn("invite control server stopped", "err", err)
		}
	}()
}

// inviteExpiry is how long a discovered player stays in the invite rotation before being
// dropped. Prevents invite_queue.txt from growing forever with people who were seen once on the
// target server months ago and have no realistic chance of still caring about an invite - a
// long-running scraper otherwise accumulates entries indefinitely with nothing ever removing
// them.
// readXUIDList reads every non-empty, de-duplicated line from path, preserving first-seen order.
// Each line is "xuid,username" (see cmd/scraper's output format - the username is written purely
// so a human can read the file; a bare-XUID line with no comma, from before that format existed,
// is still accepted). Missing file or read error is treated as an empty list rather than an
// error, since the queue file's absence just means the scraper hasn't been run yet or hasn't
// found anyone - not a fault in this process.
func readXUIDList(path string, log *slog.Logger) []string {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("could not read invite queue file", "path", path, "err", err)
		}
		return nil
	}
	defer f.Close()

	seen := make(map[string]bool)
	var xuids []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		xuid, _, _ := strings.Cut(line, ",")
		if xuid == "" || seen[xuid] {
			continue
		}
		seen[xuid] = true
		xuids = append(xuids, xuid)
	}
	return xuids
}
