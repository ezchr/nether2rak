package main

// Multiple broadcasts: the same backend published as several Friends-tab worlds, each hosted by
// its own Xbox account.
//
// Why: every Xbox Live multiplayer session is capped at 30 members - maxMembersCount 30 in
// Minecraft's MinecraftLobby session template, read straight from a live session document on
// 2026-09-23. One account can therefore never admit more than 30 people through the Friends
// tab at once. Each extra account hosts its own session, so each adds another 30.
//
// What one broadcast is: everything that depends on the Xbox account - its own sign-in
// (token file), RTA connection, friend-request handling, NetherNet signaling listener and
// network ID, MPSD session, presence, proactive token renewal and rebuild loop. runBroadcast is
// that whole chain, moved out of main unchanged, and runs once per account.
//
// What is shared: the process - config, backend transport, compression, the ping API, the
// friend-activity log and the direct-IP front door. Every broadcast relays into the same
// backend with the same bridge.Config (relayConfig), so a player is the same player whichever
// world they joined through.
//
// Checked before building this (2026-09-23): no package-level state that two broadcasts would
// share incorrectly. bridge.Listen creates its own signaling connections from the account's
// own MC token; the ping registry is keyed by the joining player's XUID; the one
// single-registration slot in bridge (nnserver's Notify) belongs to the direct-IP door, of
// which there is still only one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
	"github.com/gameparrot/netherconnect/xbl/friendactivity"
)

// resolveBroadcasts fills in defaults for cfg.ExtraBroadcasts and drops invalid entries,
// returning the usable ones plus one message per dropped entry. Pure - no I/O - so it is
// unit tested directly.
func resolveBroadcasts(cfg FileConfig) (out []BroadcastConfig, problems []string) {
	seenFile := map[string]string{tokenCacheFile: "primary"}
	seenName := map[string]bool{"primary": true}
	seenPort := map[int]string{}
	if cfg.InvitePort > 0 {
		seenPort[cfg.InvitePort] = "primary"
	}
	for i, b := range cfg.ExtraBroadcasts {
		if strings.TrimSpace(b.Name) == "" {
			b.Name = fmt.Sprintf("extra-%d", i+1)
		}
		if seenName[b.Name] {
			problems = append(problems, fmt.Sprintf("extra broadcast #%d: name %q is already used - skipped", i+1, b.Name))
			continue
		}
		if strings.TrimSpace(b.TokenFile) == "" {
			problems = append(problems, fmt.Sprintf("extra broadcast %q: token_file is empty - skipped", b.Name))
			continue
		}
		if other, ok := seenFile[b.TokenFile]; ok {
			problems = append(problems, fmt.Sprintf("extra broadcast %q: token_file %q is already used by %s - skipped (each broadcast needs its own Xbox account)", b.Name, b.TokenFile, other))
			continue
		}
		if b.InvitePort > 0 {
			if other, ok := seenPort[b.InvitePort]; ok {
				problems = append(problems, fmt.Sprintf("extra broadcast %q: invite_port %d is already used by %s - invite control turned off for it", b.Name, b.InvitePort, other))
				b.InvitePort = 0
			} else {
				seenPort[b.InvitePort] = b.Name
			}
		}
		if b.HostName == "" {
			b.HostName = cfg.HostName
		}
		if b.WorldName == "" {
			b.WorldName = cfg.WorldName
		}
		seenName[b.Name] = true
		seenFile[b.TokenFile] = b.Name
		out = append(out, b)
	}
	return out, problems
}

// forBroadcast is cfg as one broadcast sees it: the shared settings, with this broadcast's
// own Friends-tab names and invite port.
func (b BroadcastConfig) forBroadcast(cfg FileConfig) FileConfig {
	cfg.HostName, cfg.WorldName, cfg.InvitePort = b.HostName, b.WorldName, b.InvitePort
	cfg.ExtraBroadcasts = nil
	return cfg
}

// Two broadcasts signed in to the same Xbox account would create two sessions and two
// presences for one XUID and fight each other; the token files differing doesn't rule that
// out (someone can sign the same account in twice). claimXUID is the backstop.
var (
	xuidOwnersMu sync.Mutex
	xuidOwners   = map[string]string{}
)

func claimXUID(xuid, name string) (owner string, ok bool) {
	xuidOwnersMu.Lock()
	defer xuidOwnersMu.Unlock()
	if o, taken := xuidOwners[xuid]; taken {
		return o, false
	}
	xuidOwners[xuid] = name
	return name, true
}

// runExtraBroadcast signs in one extra account and runs its broadcast until ctx ends. A
// failure here stops only this broadcast - never the process or the others.
func runExtraBroadcast(ctx context.Context, b BroadcastConfig, cfg FileConfig, allowXUID func(string) bool,
	friendActivity *friendactivity.Store, debug bool, log *slog.Logger) {
	blog := log.With("broadcast", b.Name)
	tokSrc, err := tokenSource(blog, b.TokenFile, b.Name)
	if err != nil {
		blog.Error("extra broadcast not started: sign-in failed", "tokenFile", b.TokenFile, "err", err)
		return
	}
	authSession, err := session.SessionFromTokenSource(tokSrc, deviceAuth, ctx)
	if err != nil {
		blog.Error("extra broadcast not started: auth session failed", "err", err)
		return
	}
	if err := runBroadcast(ctx, b.Name, authSession, b.forBroadcast(cfg), allowXUID, friendActivity, debug, log); err != nil && ctx.Err() == nil {
		blog.Error("extra broadcast stopped", "err", err)
	}
}

// runBroadcast runs one account's Friends-tab broadcast until ctx ends: identity, RTA, friend
// requests, then the session rebuild loop. This is the per-account body of what used to be
// main() - the logic and every comment in it are unchanged, only moved here so it can run once
// per account. Returns early only if the account can't get going at all.
func runBroadcast(ctx context.Context, name string, authSession *session.Session, cfg FileConfig,
	allowXUID func(string) bool, friendActivity *friendactivity.Store, debug bool, log *slog.Logger) error {
	log = log.With("broadcast", name)

	xstsTok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return fmt.Errorf("get xbox live identity: %w", err)
	}
	xuid := xstsTok.AuthorizationToken.DisplayClaims.UserInfo[0].XUID
	gamertag := xstsTok.AuthorizationToken.DisplayClaims.UserInfo[0].GamerTag
	log.Info("authenticated", "gamertag", gamertag, "xuid", xuid)
	if owner, ok := claimXUID(xuid, name); !ok {
		return fmt.Errorf("xbox account %s (%s) is already broadcasting as %q - each broadcast needs its own account", gamertag, xuid, owner)
	}

	// --- RTA: needed for a valid connectionId before we can create a session, and to react
	// to session-membership changes (nonce refresh) and incoming friend requests in real time.
	rta := xbl.NewRTA(authSession, xuid, log)
	go func() {
		for {
			if err := rta.Connect(ctx); err != nil && ctx.Err() == nil {
				log.Warn("rta connection dropped, reconnecting", "err", err)
				time.Sleep(3 * time.Second)
				continue
			}
			return
		}
	}()
	// Blocks until RTA's first connection ID arrives - just to fail fast at startup if RTA can
	// never connect at all. The actual value isn't kept: runSession re-reads rta.ConnectionID
	// itself on every call, since a mid-process RTA reconnect changes it (confirmed 2026-08-21:
	// passing this as a fixed value here meant every session rebuild after an RTA reconnect used
	// a dead connection ID, and Xbox Live correctly rejected session creation for it with "the
	// owner isn't active in the referenced session" - see RTA.ConnectionID's doc comment).
	if _, err := rta.ConnectionID(ctx); err != nil {
		return fmt.Errorf("obtain rta connection id: %w", err)
	}

	// --- Friend request handling.
	//
	// FriendManager.Run drives this: an immediate scan at startup (catches up on requests
	// received entirely while this relay was offline), then RTA's push notification as the fast
	// path, falling back to a periodic scan since that push was found, in real operation, to go
	// silently quiet indefinitely after some reconnects - see FriendManager.Run's own doc comment
	// for the real incident that motivated this (three pending requests found sitting unprocessed
	// with zero related log output).
	friends := xbl.NewFriendManager(authSession, log)
	go friends.Run(ctx)
	rta.OnFriendRequest = func() {
		select {
		case friends.Trigger <- struct{}{}:
		default:
			// Already a trigger pending; Run will pick it up on its next iteration regardless.
		}
	}

	// The signaling websocket's Authorization header is a snapshot of our MC multiplayer token
	// taken once at dial time and never refreshed (see runSession's ValidUntil comment) - Xbox
	// Live was observed unilaterally closing it with "Signaling server instance is shutting
	// down." once that token lapsed (confirmed 2026-08-20, run.log), which runSession now
	// preempts on its own schedule via errTokenRenewal. Serve can still die for other reasons
	// (real network drops, Xbox Live hiccups), so this loop remains the fallback either way:
	// rebuild the listener and the Xbox Live session (both are tied to the specific
	// netherNetID/pmsgID the old listener obtained) and keep going, the same way the RTA
	// connection above already reconnects on drop, instead of leaving the process idle and
	// requiring a manual restart every time this happens.
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	consecutiveFailures := 0
	// sessionID persists across every runSession call (including token-renewal and error
	// retries) rather than being regenerated per call - see xbl.New's doc comment for why:
	// a fresh sessionID on every rebuild silently orphans already-connected players from
	// Xbox Live's own session bookkeeping, even though their game connection keeps working.
	var sessionID string
	// netherNetID likewise persists for the whole process - see bridge.Listen's doc comment:
	// regenerating it per rebuild strands any client that cached the old ID in its Friends tab.
	var netherNetID uint64
	for {
		var err error
		sessionID, netherNetID, err = runSession(ctx, authSession, rta, xuid, sessionID, netherNetID, cfg, allowXUID, friendActivity, debug, log)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, errTokenRenewal):
			log.Info("restarting session for scheduled token renewal")
			// MCToken() is a lazy cache: it only re-fetches once the cached token's own
			// ValidUntil has passed. The proactive-renewal timer above intentionally fires a
			// few minutes BEFORE that (see the timer's own comment), specifically so the
			// rebuild finishes before Xbox Live force-closes the old signaling socket - but
			// that means the cached token is still "valid" by MCToken()'s check when runSession
			// calls it again here, so bridge.Listen just gets handed the same soon-to-expire
			// token instead of a fresh one. The new listener then schedules ANOTHER proactive
			// renewal about a minute later against that same stale token, and the cycle repeats
			// every ~60s (floor-clamped by renewIn's `< time.Minute` guard) until the token
			// finally expires for real and a genuine fetch happens - thrashing the whole
			// NetherNet/session stack for several minutes on every scheduled renewal. Confirmed
			// 2026-08-22: five consecutive rebuilds one minute apart, each logging the exact
			// same validUntil with a shrinking validFor, before a real 4h token finally landed.
			// Forcing a real refresh here breaks the loop: the rebuild actually gets what it
			// asked for on the first attempt.
			if _, refreshErr := authSession.ForceRefreshMCToken(ctx); refreshErr != nil {
				log.Error("failed to force-refresh mc token for scheduled renewal", "err", refreshErr)
			}
			backoff = time.Second
			consecutiveFailures = 0
			continue
		case err != nil:
			log.Error("session ended, restarting", "err", err)
			consecutiveFailures++
		default:
			log.Warn("listener stopped with no error, restarting")
			consecutiveFailures++
		}
		// A cached MC token can look valid by its own ValidUntil while a session-creation call
		// still fails for unrelated reasons (e.g. presence lapsing on rebuild, fixed separately
		// in runSession - see the ordering comment there). This is a defensive fallback for the
		// case where the token itself genuinely has gone stale server-side despite ValidUntil
		// not yet having passed; MCToken's normal lazy refresh can't detect that on its own, so
		// after a few consecutive failures force a real refresh rather than retrying with the
		// same cached token indefinitely.
		const forceRefreshAfter = 3
		if consecutiveFailures >= forceRefreshAfter {
			log.Warn("repeated session failures, forcing mc token refresh",
				"consecutiveFailures", consecutiveFailures)
			if _, refreshErr := authSession.ForceRefreshMCToken(ctx); refreshErr != nil {
				log.Error("failed to force-refresh mc token", "err", refreshErr)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// nextTokenFile returns the first "tokenN.json" (N = 2, 3, ...) in dir that does not already
// exist. token.json (N=1) is always the primary and is never returned. Used by `-login` with no
// filename, so running it repeatedly - once per extra account - needs no manual bookkeeping.
func nextTokenFile(dir string) string {
	for n := 2; ; n++ {
		name := fmt.Sprintf("token%d.json", n)
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
	}
}

// runLoginBroadcast is `-login [FILE]`: sign in to FILE's account (reusing its cached token if
// still valid, exactly like every other broadcast's own startup - so running `-login FILE`
// again later just resumes broadcasting rather than forcing a fresh sign-in) and broadcast as
// an extra broadcaster under that account, IN THIS PROCESS, until Ctrl+C. This is a standalone
// run meant to be started and left running on its own, same as the main relay - it does not
// write to or read extra_broadcasts in config.json, only its shared backend settings.
//
// One account can only run one broadcast: claimXUID (shared with the main relay's own
// extra_broadcasts loop) rejects a second broadcast signed in as the same Xbox account WITHIN
// THIS PROCESS. It cannot see or reject an account already broadcasting from a *different*
// nether2rak-unified process (this one, or another -login run) - Xbox Live itself is the
// backstop for that: creating a second session for an account already hosting one is expected
// to fail or evict the first, not run both cleanly. Use a different account per broadcast.
func runLoginBroadcast(file string, debug bool, log *slog.Logger) {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Error("failed to load config.json", "err", err)
		os.Exit(1)
	}
	friendActivity, err := friendactivity.Open("friend_activity.txt", log)
	if err != nil {
		log.Error("failed to open friend activity file", "err", err)
		os.Exit(1)
	}
	var allowXUID func(string) bool
	if len(cfg.AllowedXUIDs) > 0 {
		allowed := make(map[string]bool, len(cfg.AllowedXUIDs))
		for _, x := range cfg.AllowedXUIDs {
			allowed[x] = true
		}
		allowXUID = func(x string) bool { return allowed[x] }
	}

	name := strings.TrimSuffix(strings.TrimSuffix(file, ".json"), "-token")
	b := BroadcastConfig{Name: name, TokenFile: file}.forBroadcast(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokSrc, err := tokenSource(log, file, name)
	if err != nil {
		log.Error("failed to authenticate", "err", err)
		os.Exit(1)
	}
	authSession, err := session.SessionFromTokenSource(tokSrc, deviceAuth, ctx)
	if err != nil {
		log.Error("failed to start minecraft auth session", "err", err)
		os.Exit(1)
	}

	log.Info("broadcasting - press Ctrl+C to stop", "tokenFile", file, "worldName", b.WorldName)
	if err := runBroadcast(ctx, name, authSession, b, allowXUID, friendActivity, debug, log); err != nil && ctx.Err() == nil {
		log.Error("broadcast failed to start", "err", err)
		os.Exit(1)
	}
}
