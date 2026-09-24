package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gameparrot/netherconnect/bridge"
	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
)

// This file is the "invite my real Xbox friends" feature - a separate source of XUIDs to invite
// from invite_queue.txt/cmd/scraper (invitewatcher.go), sharing the same underlying
// xblSession.SendInvite mechanism. Where the scraper discovers XUIDs by joining an arbitrary
// third-party server, this reads the host account's own real Xbox Live friends list directly
// (xbl.ListFriends) - no second account, no scraping, just this account's own existing friends.
//
// OFF BY DEFAULT, same shape as the queue-based inviter: starts stopped, controlled by its own
// loopback-only HTTP endpoint:
//
//	curl http://127.0.0.1:<InvitePort>/invites/friends/start
//	curl http://127.0.0.1:<InvitePort>/invites/friends/stop
//	curl http://127.0.0.1:<InvitePort>/invites/friends/status
//
// Shares the InvitePort control server with the queue-based inviter (different path prefix,
// same HTTP server) rather than opening a second port - see startInviteControlServer.
//
// Runs the same repeating-lap shape: re-fetch the friends list, invite everyone on it, loop
// straight back and do it again, until stopped. Re-fetching every lap (rather than once at
// start) means someone added as a friend mid-run gets included on the very next lap without
// needing a restart.

// friendsInviteRateLimit mirrors inviteRateLimit's reasoning exactly (see invitewatcher.go) -
// same endpoint, same real-world rate-limit uncertainty, so no reason to pick a different pace.
const friendsInviteRateLimit = 10 * time.Second

// friendsInviteController is the friends-list twin of inviteController. Kept as a separate type
// rather than generalizing the two together, since their only shared behavior is "loop and call
// SendInvite" - everything about where the XUID list comes from differs.
type friendsInviteController struct {
	authSession *session.Session
	xblSession  *xbl.Session
	log         *slog.Logger

	mu      sync.Mutex
	running bool
	stop    context.CancelFunc
}

func newFriendsInviteController(authSession *session.Session, xblSession *xbl.Session, log *slog.Logger) *friendsInviteController {
	return &friendsInviteController{
		authSession: authSession,
		xblSession:  xblSession,
		log:         log.With("src", "friends-inviter"),
	}
}

func (c *friendsInviteController) Start(parentCtx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	loopCtx, cancel := context.WithCancel(parentCtx)
	c.stop = cancel
	c.running = true
	c.log.Info("friends invite loop started")
	go c.run(loopCtx)
}

func (c *friendsInviteController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	c.stop()
	c.stop = nil
	c.running = false
	c.log.Info("friends invite loop stopped")
}

func (c *friendsInviteController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *friendsInviteController) run(ctx context.Context) {
	for {
		friends, err := xbl.ListFriends(ctx, c.authSession)
		if err != nil {
			c.log.Error("failed to list friends", "err", err)
		} else if len(friends) == 0 {
			c.log.Debug("friends list empty, waiting")
		} else {
			c.log.Info("starting friends invite lap", "count", len(friends))
			for _, f := range friends {
				select {
				case <-ctx.Done():
					return
				case <-time.After(friendsInviteRateLimit):
				}
				if bridge.ConnectedXUIDs()[f.XUID] {
					c.log.Debug("skipping invite - already connected", "xuid", f.XUID)
					continue
				}
				if err := c.xblSession.SendInvite(ctx, f.XUID); err != nil {
					c.log.Error("failed to send invite", "xuid", f.XUID, "err", err)
				}
			}
			c.log.Info("friends invite lap complete", "count", len(friends))
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// registerFriendsInviteRoutes adds the /invites/friends/* routes to an existing mux - called
// from startInviteControlServer so both inviters share one HTTP server/port rather than each
// opening their own.
func registerFriendsInviteRoutes(ctx context.Context, mux *http.ServeMux, c *friendsInviteController) {
	mux.HandleFunc("/invites/friends/start", func(w http.ResponseWriter, r *http.Request) {
		// ctx, not context.Background() - see invitewatcher.go's /invites/start handler for the
		// full story (same orphaned-goroutine bug, same fix, same live-confirmed symptom).
		c.Start(ctx)
		fmt.Fprintln(w, "started")
	})
	mux.HandleFunc("/invites/friends/stop", func(w http.ResponseWriter, r *http.Request) {
		c.Stop()
		fmt.Fprintln(w, "stopped")
	})
	mux.HandleFunc("/invites/friends/status", func(w http.ResponseWriter, r *http.Request) {
		if c.Running() {
			fmt.Fprintln(w, "running")
		} else {
			fmt.Fprintln(w, "stopped")
		}
	})
}
