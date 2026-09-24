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

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/bridge"
	"github.com/gameparrot/netherconnect/proxy"
	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
	"github.com/gameparrot/netherconnect/xbl/friendactivity"
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

	// `-login [FILE]`: sign in (or reuse FILE's cached token) and broadcast as an EXTRA
	// broadcaster under that account, in THIS process, until Ctrl+C - a standalone
	// broadcast-only run, not a one-shot sign-in step. It does not touch config.json and does
	// not require restarting whatever main relay process is already running: this is meant to
	// be started and left running as its own process, the same way the main relay is, just
	// scoped to one extra account. See runLoginBroadcast in broadcasts.go.
	//
	// FILE is optional. Given with no FILE, it picks the next unused "tokenN.json" itself
	// (token2.json, token3.json, ... - token.json is always the primary) so running
	// `-login` repeatedly, once per extra account, needs no filename bookkeeping.
	//
	// It still shares config.json's BACKEND settings (transport/address/compression/allowed
	// XUIDs) - those describe the server being broadcast, not the account - but starts neither
	// a ping server nor a direct-IP door: both are already running in the main relay process
	// and a second one here would either fight over the same port or duplicate the front door.
	args := os.Args[1:]
	for i, a := range args {
		if a == "-login" || a == "--login" {
			file := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				file = args[i+1]
			}
			if file == "" {
				file = nextTokenFile(".")
				fmt.Printf("No filename given - using %s (the next unused tokenN.json).\n", file)
			}
			runLoginBroadcast(file, debug, log)
			return
		}
	}

	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Error("failed to load config.json", "err", err)
		os.Exit(1)
	}
	if d := sharedPlayerDrift(cfg); d != nil {
		log.Info("advertised player count drifts", "min", d.min, "max", d.max, "maxStep", d.maxStep,
			"every", d.interval, "start", d.Current())
	}

	// Records the last time each connecting friend actually reached a real backend, so
	// cmd/friends/pruneinactive can later tell apart a friend who genuinely never plays from one
	// who simply has not connected in a while. See friendactivity's own package doc for why this
	// is a plain file the relay writes rather than something pruneinactive computes itself - it
	// has no other way to know who has actually joined.
	friendActivity, err := friendactivity.Open("friend_activity.txt", log)
	if err != nil {
		log.Error("failed to open friend activity file", "err", err)
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

	// The primary broadcast's account (token.json). Signed in here, before the per-broadcast
	// code, because the direct-IP front door below also relays with this account's identity.
	// Its identity/RTA/friend-request/session setup now lives in runBroadcast (broadcasts.go),
	// which also runs each of cfg.ExtraBroadcasts under its own account.
	tokSrc, err := tokenSource(log, tokenCacheFile, "primary")
	if err != nil {
		log.Error("failed to authenticate", "err", err)
		os.Exit(1)
	}

	authSession, err := session.SessionFromTokenSource(tokSrc, deviceAuth, ctx)
	if err != nil {
		log.Error("failed to start minecraft auth session", "err", err)
		os.Exit(1)
	}

	// --- Real-latency ping API for the Folia-side PingDisplay plugin (loopback only). Port is
	// configurable (see PingPort's doc comment) for the same multi-instance reason as pprof's.
	// One per process: it answers for every broadcast (entries are keyed by player XUID).
	bridge.StartPingServer(fmt.Sprintf("127.0.0.1:%d", cfg.PingPort), log)

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
	// Resolve and sanity-check the backend transport before announcing the world. A bad address
	// or a backend that isn't actually speaking NetherNet is worth failing/warning about here,
	// rather than letting it surface as an unexplained stall on the first player's join.
	switch strings.ToLower(strings.TrimSpace(cfg.BackendTransport)) {
	case "", bridge.TransportRakNet:
		log.Info("relaying into backend over raknet", "address", cfg.GeyserAddress)
	case bridge.TransportNetherNet:
		normalized, err := bridge.NormalizeNetherNetAddress(cfg.NetherNetBackendAddress)
		if err != nil {
			log.Error("invalid nethernet_backend_address in config.json", "err", err)
			os.Exit(1)
		}
		log.Info("relaying into backend over nethernet", "address", normalized)
		// Warn rather than exit: the backend may simply not be up yet, and the relay is useful
		// the moment it is. Each join re-dials anyway, so this recovers on its own.
		probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
		if err := bridge.ProbeNetherNetBackend(probeCtx, normalized, log); err != nil {
			log.Warn("backend did not answer the nethernet capability probe - joins will fail until it does", "err", err)
		}
		cancelProbe()
	default:
		log.Error("unknown backend_transport in config.json - use \"raknet\" or \"nethernet\"",
			"value", cfg.BackendTransport)
		os.Exit(1)
	}
	// Second front door: direct-IP joins. Started once for the process (not per Xbox Live
	// session) because it has no dependency on Xbox Live signaling - it only needs the backend.
	// It is handed the same bridge.Config the Friends-tab listener gets below, which is what
	// makes both paths resolve a player to one backend record.
	if cfg.RelayDirectIP {
		if err := bridge.StartDirectIP(ctx, bridge.DirectIPConfig{
			ListenAddress: cfg.RelayDirectIPListenAddress,
			WorldName:     cfg.WorldName,
			MaxPlayers:    cfg.MaxPlayers,
			Protocol:      cfg.Protocol,
			Version:       cfg.Version,
			Relay:         relayConfig(cfg, authSession, allowXUID, friendActivity, log),
			Log:           log,
		}); err != nil {
			log.Error("failed to start direct-ip front door", "err", err)
			os.Exit(1)
		}
	} else {
		log.Info("direct-ip front door disabled (set direct_ip_enabled to turn it on)")
	}

	log.Info("running - press Ctrl+C to stop (there is no console prompt; this process just waits for connections)")

	// Extra broadcasts (config extra_broadcasts, see broadcasts.go): each signs in its own
	// account and runs in the background; a problem with one never stops the others.
	extras, problems := resolveBroadcasts(cfg)
	for _, p := range problems {
		log.Error(p)
	}
	for _, b := range extras {
		log.Info("starting extra broadcast", "broadcast", b.Name, "tokenFile", b.TokenFile, "worldName", b.WorldName)
		go runExtraBroadcast(ctx, b, cfg, allowXUID, friendActivity, debug, log)
	}

	// The primary broadcast runs on this goroutine until shutdown, as before. If its account
	// can't get going at all, the process exits - exactly the old behaviour.
	primaryCfg := cfg
	primaryCfg.ExtraBroadcasts = nil
	if err := runBroadcast(ctx, "primary", authSession, primaryCfg, allowXUID, friendActivity, debug, log); err != nil && ctx.Err() == nil {
		log.Error("primary broadcast failed to start", "err", err)
		os.Exit(1)
	}
}

// runSession builds a fresh NetherNet listener and Xbox Live session, then serves connections
// until the listener dies or ctx is cancelled. Called in a loop by main so a signaling-side
// disconnect (expected to happen periodically - see the comment above the call site) results in
// a clean reconnect instead of the process going idle.
func runSession(ctx context.Context, authSession *session.Session, rta *xbl.RTA, xuid, sessionID string,
	netherNetID uint64, cfg FileConfig, allowXUID func(string) bool, friendActivity *friendactivity.Store, debug bool, log *slog.Logger) (string, uint64, error) {

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

	// Player count shown on the Friends tab: fake_player_count, drifting inside
	// fake_player_drift's range when that is set, never below 1 - see advertisedPlayers.
	worldInfo := func() xbl.Config {
		return xbl.Config{
			HostName:   cfg.HostName,
			WorldName:  cfg.WorldName,
			Players:    advertisedPlayers(cfg),
			MaxPlayers: cfg.MaxPlayers,
			Protocol:   cfg.Protocol,
			Version:    cfg.Version,
		}
	}

	xblSession := xbl.New(authSession, xuid, sessionID, connectionID, pmsgID, netherNetID, log)
	sessionID = xblSession.SessionID()
	xblSession.SetWorldInfo(worldInfo())

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

	// Invite-everyone-from-another-server feature (see invitewatcher.go for the full design and
	// cmd/scraper for the other half). OFF BY DEFAULT - the controller starts stopped, and only
	// runs after an explicit "start" over the loopback HTTP control endpoint below. sessionCtx,
	// not ctx, bounds the loop's max lifetime - a session rebuild must stop it along with
	// everything else that references the old xblSession, since this XUID list belongs to the
	// specific session being recreated.
	//
	// Only when this broadcast has an invite_port of its own - extra broadcasts default to 0
	// (off), since two broadcasts can't share one control port.
	if cfg.InvitePort > 0 {
		inviteCtl := newInviteController(xblSession, log)
		friendsInviteCtl := newFriendsInviteController(authSession, xblSession, log)
		startInviteControlServer(sessionCtx, fmt.Sprintf("127.0.0.1:%d", cfg.InvitePort), inviteCtl, friendsInviteCtl, log)
	}

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
				xblSession.SetWorldInfo(worldInfo()) // picks up fake_player_drift's next value
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
	err = ln.Serve(sessionCtx, ctx, relayConfig(cfg, authSession, allowXUID, friendActivity, log))
	if errors.Is(context.Cause(sessionCtx), errTokenRenewal) {
		return sessionID, netherNetID, errTokenRenewal
	}
	if errors.Is(err, context.Canceled) {
		return sessionID, netherNetID, nil
	}
	return sessionID, netherNetID, err
}

// relayConfig builds the bridge configuration shared by both front doors. Both the Friends-tab
// listener and the direct-IP listener are served with the value this returns, deliberately: any
// difference between them would be a difference in how a player's identity reaches the backend,
// which is the exact problem this build exists to eliminate.
func relayConfig(cfg FileConfig, authSession *session.Session, allowXUID func(string) bool, friendActivity *friendactivity.Store, log *slog.Logger) bridge.Config {
	return bridge.Config{
		NetherNetIdentity: func(ctx context.Context) (*nethernet.Identity, error) {
			return bridge.NewClientIdentity(ctx, authSession)
		},
		BackendTransport:        cfg.BackendTransport,
		GeyserAddress:           cfg.GeyserAddress,
		NetherNetBackendAddress: cfg.NetherNetBackendAddress,
		FixNativeBDSPersistence: cfg.FixNativeBDSPersistence,
		DisableClientEncryption: cfg.DisableClientEncryption,
		AllowXUID:               allowXUID,
		OnJoin:                  friendActivity.RecordJoin,
		Log:                     log,
	}
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
//
// file is the token cache (token.json for the primary broadcast, each extra broadcast's own
// token_file otherwise); label names the broadcast in the sign-in prompt.
func tokenSource(log *slog.Logger, file, label string) (oauth2.TokenSource, error) {
	cachePath := filepath.Join(".", file)

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

	fmt.Printf("[%s] Signing in with a Microsoft account for %s - this should be the account you want the world to appear to be hosted by.\n", label, file)
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
