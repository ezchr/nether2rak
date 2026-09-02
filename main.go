// Command nether2rak broadcasts a locally-running Geyser instance over Xbox Live as a joinable
// Friends-tab world, and relays real players straight into Geyser over NetherNet - without the
// fake-handshake-then-TransferPacket trick MCXboxBroadcast uses. See the accompanying README
// for the full explanation of why that distinction matters and what's still unverified.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gameparrot/netherconnect/bridge"
	"github.com/gameparrot/netherconnect/proxy"
	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

// deviceAuth uses gophertunnel's AndroidConfig: client ID 0000000048183522, the Minecraft
// Bedrock (Android) title ID. This MUST be a real Minecraft Bedrock title client ID.
//
// Xbox Live's SISU flow issues an XSTS token carrying a *title* claim, and the Minecraft
// session directory service config (SCID 4fc10100-...) is only accessible to tokens bearing
// Minecraft's own title. An earlier version of this file used NetherConnect's preview client
// ID (00000000403fc600) - the sign-in page then reads "Minecraft beta", and every session
// directory call fails with:
//
//	403: The requested service config cannot be accessed.
//
// MCXboxBroadcast uses this same Android title ID (BEDROCK_ANDROID_TITLE_ID) for the same
// reason.
var deviceAuth = auth.AndroidConfig

const tokenCacheFile = "token.json"

// errTokenRenewal marks a runSession exit as our own proactive, scheduled restart ahead of MC
// token expiry (see the ValidUntil comment in runSession) rather than an actual failure, so
// main's retry loop can restart immediately without logging it as an error or growing backoff.
var errTokenRenewal = errors.New("proactive session renewal")

func main() {
	debug := false
	for _, a := range os.Args[1:] {
		if a == "-debug" || a == "--debug" {
			debug = true
		}
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Error("failed to load config.json", "err", err)
		os.Exit(1)
	}

	// Bound to loopback so the profiler is reachable only over an SSH tunnel, never from the
	// public internet. Port is configurable (see PprofPort's doc comment) since a fixed port
	// collides if more than one nether2rak instance runs on the same machine.
	if debug {
		pprofAddr := fmt.Sprintf("127.0.0.1:%d", cfg.PprofPort)
		go func() {
			log.Info("pprof listening (debug builds only)", "addr", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Warn("pprof server stopped", "err", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokSrc, err := tokenSource(log)
	if err != nil {
		log.Error("failed to authenticate", "err", err)
		os.Exit(1)
	}

	authSession, err := session.SessionFromTokenSource(tokSrc, deviceAuth, ctx)
	if err != nil {
		log.Error("failed to start minecraft auth session", "err", err)
		os.Exit(1)
	}

	xstsTok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		log.Error("failed to get xbox live identity", "err", err)
		os.Exit(1)
	}
	xuid := xstsTok.AuthorizationToken.DisplayClaims.UserInfo[0].XUID
	gamertag := xstsTok.AuthorizationToken.DisplayClaims.UserInfo[0].GamerTag
	log.Info("authenticated", "gamertag", gamertag, "xuid", xuid)

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
		log.Error("failed to obtain rta connection id", "err", err)
		os.Exit(1)
	}

	// --- Real-latency ping API for the Folia-side PingDisplay plugin (loopback only). Port is
	// configurable (see PingPort's doc comment) for the same multi-instance reason as pprof's.
	bridge.StartPingServer(fmt.Sprintf("127.0.0.1:%d", cfg.PingPort), log)

	// --- Friend request handling.
	friends := xbl.NewFriendManager(authSession, log)
	go friends.Run(ctx)
	rta.OnFriendRequest = func() {
		if err := friends.CheckPending(ctx); err != nil {
			log.Error("failed to check pending friend requests", "err", err)
		}
	}

	var allowXUID func(string) bool
	if len(cfg.AllowedXUIDs) > 0 {
		allowed := make(map[string]bool, len(cfg.AllowedXUIDs))
		for _, x := range cfg.AllowedXUIDs {
			allowed[x] = true
		}
		allowXUID = func(x string) bool { return allowed[x] }
	}

	switch strings.ToLower(cfg.Compression) {
	case "", "snappy":
		proxy.DefaultCompression = packet.SnappyCompression
	case "flate", "deflate", "zlib":
		proxy.DefaultCompression = packet.FlateCompression
	case "none", "nop", "off":
		proxy.DefaultCompression = packet.NopCompression
	default:
		log.Warn("unknown compression in config, falling back to snappy", "value", cfg.Compression)
		proxy.DefaultCompression = packet.SnappyCompression
	}
	if cfg.CompressionThreshold > 0 {
		proxy.DefaultCompressionThreshold = cfg.CompressionThreshold
	}
	log.Info("packet compression configured",
		"algorithm", cfg.Compression, "threshold", proxy.DefaultCompressionThreshold)
	log.Info("relaying into backend server", "address", cfg.ServerAddress)
	log.Info("running - press Ctrl+C to stop (there is no console prompt; this process just waits for connections)")

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
		sessionID, netherNetID, err = runSession(ctx, authSession, rta, xuid, sessionID, netherNetID, cfg, allowXUID, debug, log)
		if ctx.Err() != nil {
			return
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
			return
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

// runSession builds a fresh NetherNet listener and Xbox Live session, then serves connections
// until the listener dies or ctx is cancelled. Called in a loop by main so a signaling-side
// disconnect (expected to happen periodically - see the comment above the call site) results in
// a clean reconnect instead of the process going idle.
func runSession(ctx context.Context, authSession *session.Session, rta *xbl.RTA, xuid, sessionID string,
	netherNetID uint64, cfg FileConfig, allowXUID func(string) bool, debug bool, log *slog.Logger) (string, uint64, error) {

	// Re-read every call, not passed in once by the caller: the RTA websocket can reconnect
	// mid-process (main's own goroutine handles that independently), which changes this value -
	// see RTA.ConnectionID's doc comment for why a stale one here breaks session creation.
	connectionID, err := rta.ConnectionID(ctx)
	if err != nil {
		return sessionID, netherNetID, fmt.Errorf("obtain rta connection id: %w", err)
	}

	ln, netherNetID, pmsgID, err := bridge.Listen(ctx, authSession, netherNetID, log)
	if err != nil {
		return sessionID, netherNetID, fmt.Errorf("start nethernet listener: %w", err)
	}
	log.Info("nethernet listeners ready", "netherNetID", netherNetID, "pmsgID", pmsgID)

	// MemberCount must reflect the XBL session's own member count, which always includes the
	// host account itself as member 0 - confirmed against a live working session capture, which
	// showed MemberCount:1 with zero real players connected. Sending 0 here contradicts the
	// session's own "members" map (which always has at least one entry) and is a likely reason
	// the world was filtered from the Friends tab. FakePlayerCount, when set above zero,
	// overrides this floor for display purposes only. When FakePlayerCount is 0, the displayed
	// count instead tracks bridge.ConnectedPlayerCount() live (refreshed every session-update
	// tick below), floored at 1 for the same reason.
	displayedPlayers := displayPlayerCount(cfg)

	xblSession := xbl.New(authSession, xuid, sessionID, connectionID, pmsgID, netherNetID, log)
	sessionID = xblSession.SessionID()
	xblSession.SetWorldInfo(xbl.Config{
		HostName:   cfg.HostName,
		WorldName:  cfg.WorldName,
		Players:    displayedPlayers,
		MaxPlayers: cfg.MaxPlayers,
		Protocol:   cfg.Protocol,
		Version:    cfg.Version,
	})

	sessionCtx, cancelSession := context.WithCancelCause(ctx)
	defer cancelSession(nil)

	// bridge.Listen (above) baked a snapshot of the MC multiplayer token into the signaling
	// websocket's Authorization header at dial time (see signaling/dial.go - the token is never
	// re-sent for the life of that connection). Xbox Live has been observed unilaterally closing
	// that websocket ~1h45m-3h into a session with "Signaling server instance is shutting down."
	// (confirmed 2026-08-20 across multiple sessions, run.log), which lines up with this token's
	// own ValidUntil rather than any fixed schedule - the wording is misleading, but the server
	// is just refusing to keep trusting a connection whose credential it has since expired.
	// Instead of waiting for that forced, disruptive close (which was also dropping in-flight
	// player ICE connections - see bridge/listener.go's 30s ConnContext comment), fetch the same
	// token bridge.Listen just obtained and proactively cancel sessionCtx a few minutes before it
	// lapses, so main's retry loop rebuilds the listener/session with a fresh token on our own
	// schedule instead of Microsoft's.
	if mcTok, tokErr := authSession.MCToken(sessionCtx); tokErr == nil {
		untilExpiry := time.Until(mcTok.ValidUntil)
		log.Info("mc token obtained for this session", "validUntil", mcTok.ValidUntil, "validFor", untilExpiry)
		const renewMargin = 5 * time.Minute
		renewIn := untilExpiry - renewMargin
		if renewIn < time.Minute {
			renewIn = time.Minute
		}
		// Debug-only override to force a near-immediate renewal for testing the reconnect
		// behavior live, without waiting ~4h for a real one. Unset in normal operation.
		if s := os.Getenv("NETHER2RAK_DEBUG_RENEW_SECONDS"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil {
				renewIn = time.Duration(secs) * time.Second
				log.Warn("NETHER2RAK_DEBUG_RENEW_SECONDS override active", "renewIn", renewIn)
			}
		}
		go func() {
			select {
			case <-sessionCtx.Done():
			case <-time.After(renewIn):
				log.Info("proactively renewing session ahead of mc token expiry", "after", renewIn)
				cancelSession(errTokenRenewal)
			}
		}()
	} else {
		log.Warn("could not check mc token expiry for proactive renewal", "err", tokErr)
	}

	// Every member's nonce must be (re)generated whenever the session's member list changes -
	// see RefreshNonces' doc comment - or a newly joined player's connection info is never
	// properly authorized and their WebRTC/ICE handshake with us silently never completes, even
	// though the Xbox Live session document itself shows them as joined. This callback is how
	// the RTA websocket's session-change notifications (see rta.go) reach that refresh; it must
	// be re-armed every time runSession builds a new xblSession; a prior refactor moved session
	// setup into this function but dropped this registration, which was the actual regression.
	rta.OnSessionChanged = func() {
		if err := xblSession.RefreshNonces(sessionCtx); err != nil {
			log.Error("failed to refresh session nonces", "err", err)
		}
	}

	// If RTA reconnects mid-session (a drop that recovers on its own - see IsStale's doc
	// comment - not a full runSession rebuild), the websocket gets a brand new ConnectionId
	// from Xbox Live, but xblSession was constructed with the OLD one and never learns about
	// the change on its own. Every Update() PUT after that point would keep advertising a
	// connection ID Xbox Live no longer considers live, which read back as the session's owner
	// "not active" even though nothing else about the session was actually broken - see
	// Session.SetConnectionID's doc comment for the full incident this responds to (confirmed
	// 2026-08-23: RTA reconnected at 01:43:28, activity-handle-related calls started failing
	// with "the owner isn't active in the referenced session" one minute later, session PUTs
	// kept returning 200 throughout). Skip the very first callback firing (which just reports
	// the connectionID xblSession was already constructed with moments ago) by comparing
	// against the value runSession itself just read.
	initialConnectionID := connectionID
	rta.OnConnectionIDChanged = func(newConnectionID string) {
		if newConnectionID == initialConnectionID {
			return
		}
		xblSession.SetConnectionID(newConnectionID)
		if err := xblSession.Update(sessionCtx); err != nil {
			// go-xsapi's own reconcileSessionConnection (mpsd/subscription.go's
			// HandleSubscribe) treats a failed reconciliation PUT as fatal and closes the
			// session outright rather than leaving it in a half-reconciled state - do the
			// same here via cancelSession, which is exactly what the existing retry loop in
			// the caller (the switch on err in the outer for loop) already knows how to
			// rebuild from cleanly, the same path errTokenRenewal uses.
			log.Error("failed to push reconciled connection id after rta reconnect, rebuilding session", "err", err)
			cancelSession(fmt.Errorf("reconcile connection id: %w", err))
		} else {
			log.Info("reconciled session connection id after rta reconnect")
		}
	}

	// Presence must be (re-)established BEFORE requesting the session's activity handle, not
	// after. Presence is a separate Xbox Live API from the session directory (see Presence's doc
	// comment) - creating a session document does not itself mark the account "active", and
	// Create's own handle-creation step requires that the account already reads as active in
	// the session it's being asked to create a handle for. On a fresh process start this
	// ordering was harmless (nothing had presence yet either way), but on any rebuild - e.g. the
	// proactive token-renewal restart below - the PREVIOUS presence.Run's heartbeat loop was
	// tied to the sessionCtx that was just cancelled, so presence had already lapsed by the time
	// this new session tried to create a handle referencing it. Confirmed 2026-08-20: a 3h54m
	// outage (12:15:56-16:09:45, run.log) where every single "create handle" call failed with
	// "403: the owner isn't active in the referenced session" - and there is no successful
	// presence heartbeat logged anywhere in that entire window, only resuming once a manual
	// process restart re-ran presence.Run before the next Create attempt.
	presence := xbl.NewPresence(authSession, xuid, log)
	if err := presence.Run(sessionCtx); err != nil {
		return sessionID, netherNetID, fmt.Errorf("set xbox live presence: %w", err)
	}

	// Presence returning 200 does not mean the session directory can already see this account
	// as active - the two are separate Xbox Live services with their own propagation delay.
	// Confirmed 2026-08-21/22 (run.log): "xbox live presence set to active" logged, then 156ms
	// later "create handle: status 403: owner isn't active in the referenced session" - and
	// because main's retry loop rebuilds everything (new listener, new keypair, new presence
	// POST, new session) on every failure, it was racing against its own fix in a tight loop
	// (every 5-9s) for over 7 minutes straight (09:44:34-09:51:47), never giving presence time
	// to actually propagate, until it eventually hit Xbox Live's own token-refresh rate limit.
	// A short fixed wait here breaks that livelock by giving presence a real chance to land
	// before the next Create attempt, instead of the retry loop being the reason it never does.
	select {
	case <-sessionCtx.Done():
		return sessionID, netherNetID, context.Cause(sessionCtx)
	case <-time.After(3 * time.Second):
	}

	if err := xblSession.Create(sessionCtx); err != nil {
		return sessionID, netherNetID, fmt.Errorf("create xbox live session: %w", err)
	}
	log.Info("session is live - should now be visible on the friends tab", "sessionID", xblSession.SessionID())

	if debug {
		time.Sleep(3 * time.Second)
		checkOwnPresence(sessionCtx, authSession, xuid, log)
	}

	go func() {
		ticker := time.NewTicker(time.Duration(cfg.UpdateIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				// Refresh the advertised player count before pushing the update, so a live
				// FakePlayerCount:0 deployment reflects real joins/leaves rather than staying
				// stuck at whatever the count was when the session was first created. Skipped
				// when FakePlayerCount is set, since that value is a static override and never
				// changes here.
				if cfg.FakePlayerCount == 0 {
					xblSession.SetWorldInfo(xbl.Config{
						HostName:   cfg.HostName,
						WorldName:  cfg.WorldName,
						Players:    displayPlayerCount(cfg),
						MaxPlayers: cfg.MaxPlayers,
						Protocol:   cfg.Protocol,
						Version:    cfg.Version,
					})
				}
				if err := xblSession.Update(sessionCtx); err != nil {
					log.Error("failed to update session", "err", err)
				}
				// Active health check, not just reacting to an explicit disconnect - mirrors
				// the real Xbox client's SessionManagerCore.checkConnection(), which polls its
				// RTA connection's health on every session-update tick rather than only
				// rebuilding when something else already noticed a failure. See IsStale's doc
				// comment for the silent-stall failure mode this catches that nothing else in
				// this codebase can detect on its own.
				if rta.IsStale(2 * time.Duration(cfg.UpdateIntervalSeconds) * time.Second) {
					rta.ForceReconnect("no RTA frames received within health-check window")
				}
			}
		}
	}()

	// ctx (the process-lifetime context from main, not sessionCtx) is passed as connCtx so that
	// already-accepted, already-playing connections survive a signaling-side session cycle -
	// see Serve's doc comment. Only the accept loop and signaling transports themselves are
	// torn down and rebuilt when sessionCtx ends; a healthy in-progress WebRTC transport for a
	// connected player has nothing to do with whether the signaling websocket that originally
	// negotiated it is still open.
	err = ln.Serve(sessionCtx, ctx, bridge.Config{
		ServerAddress: cfg.ServerAddress,
		AllowXUID:     allowXUID,
		Log:           log,
	})
	if errors.Is(context.Cause(sessionCtx), errTokenRenewal) {
		return sessionID, netherNetID, errTokenRenewal
	}
	if errors.Is(err, context.Canceled) {
		return sessionID, netherNetID, nil
	}
	return sessionID, netherNetID, err
}

// displayPlayerCount computes the player count to advertise on the Friends tab: cfg.FakePlayerCount
// when set above zero (an explicit override), otherwise the real live count of players currently
// relayed through this process (bridge.ConnectedPlayerCount), floored at 1 - see the floor's doc
// comment at this function's call site in runSession for why 0 can't be sent to Xbox Live.
func displayPlayerCount(cfg FileConfig) int {
	if cfg.FakePlayerCount > 0 {
		return cfg.FakePlayerCount
	}
	if n := bridge.ConnectedPlayerCount(); n > 0 {
		return n
	}
	return 1
}

// checkOwnPresence queries userpresence.xboxlive.com directly for this account's own presence
// document, as ground truth independent of any client-side Friends tab caching.
//
// Previously POSTed to profile.xboxlive.com/users/batch/profile/settings asking for
// "PresenceState"/"PresenceText" - that endpoint returns PROFILE fields (gamertag, gamerscore,
// avatar etc.), not presence state, and doesn't accept those setting names at all; it always
// returned 400 "the body of the request was invalid." Presence has its own separate service and
// read-back shape, matching what presence.go already POSTs to (userPresenceURLFmt) - GETting
// the same resource returns the real presence document Xbox Live has for this account.
func checkOwnPresence(ctx context.Context, authSession *session.Session, xuid string, log *slog.Logger) {
	tok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		log.Warn("presence self-check: failed to get token", "err", err)
		return
	}
	url := fmt.Sprintf("https://userpresence.xboxlive.com/users/xuid(%s)", xuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Warn("presence self-check: build request failed", "err", err)
		return
	}
	tok.SetAuthHeader(req)
	req.Header.Set("x-xbl-contract-version", "3")
	req.Header.Set("Accept-Language", "en-US")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("presence self-check: request failed", "err", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	log.Info("presence self-check (ground truth from userpresence.xboxlive.com)", "status", resp.StatusCode, "body", string(body))
}

// tokenSource performs (or reloads a cached) Microsoft device-code login, printing the
// "go to microsoft.com/link and enter this code" instructions to stdout - headless equivalent
// of what auth_utils.go does with a Fyne popup, and what MCXboxBroadcast's Java logs do too.
func tokenSource(log *slog.Logger) (oauth2.TokenSource, error) {
	cachePath := filepath.Join(".", tokenCacheFile)

	if b, err := os.ReadFile(cachePath); err == nil {
		tok := new(oauth2.Token)
		if err := json.Unmarshal(b, tok); err == nil {
			src := deviceAuth.RefreshTokenSource(tok)
			if _, err := src.Token(); err == nil {
				return &cachingTokenSource{src: src, path: cachePath}, nil
			}
			log.Warn("cached token expired, need to sign in again")
		}
	}

	fmt.Println("Signing in with a Microsoft account - this should be the account you want the world to appear to be hosted by.")
	tok, err := deviceAuth.RequestLiveTokenWriter(os.Stdout)
	if err != nil {
		return nil, fmt.Errorf("request live token: %w", err)
	}
	src := deviceAuth.RefreshTokenSource(tok)
	return &cachingTokenSource{src: src, path: cachePath}, nil
}

// cachingTokenSource writes the refreshed token back to disk on every successful refresh so
// re-runs don't require signing in again, matching NetherConnect's own token.json behaviour.
type cachingTokenSource struct {
	src  oauth2.TokenSource
	path string
}

func (c *cachingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := c.src.Token()
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(c.path, b, 0600)
	}
	return tok, nil
}
