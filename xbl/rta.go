package xbl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gameparrot/netherconnect/session"
)

const rtaWebsocketURL = "wss://rta.xboxlive.com/connect"

const (
	msgTypeSubscribe   = 1
	msgTypeUnsubscribe = 2
	msgTypeEvent       = 3
	msgTypeResync      = 4
)

// RTA is a thin client for Xbox Live's Real-Time Activity websocket. Xbox Live requires an
// active RTA subscription to a session directory "connection" before a session member entry
// referencing that connection is considered valid - without this, joins silently fail. It's
// also how we learn about session-membership changes (to refresh nonces, see session.go) and
// incoming friend requests, in real time rather than by polling.
type RTA struct {
	auth *session.Session
	xuid string
	log  *slog.Logger

	OnSessionChanged func()
	OnFriendRequest  func()
	// OnConnectionIDChanged fires with the new ConnectionId every time this RTA websocket
	// (re)connects, including the first connect. Wire this to Session.SetConnectionID + Update
	// so an already-running session's advertised connection stays in sync with which RTA
	// websocket is actually alive - see Session.SetConnectionID's doc comment for the failure
	// this fixes when nothing is wired here.
	OnConnectionIDChanged func(connectionID string)

	conn *websocket.Conn

	// connectionIDMu guards connectionID: written from handle() (called on Connect's read-loop
	// goroutine) and read from ConnectionID() (called from main's goroutine on every runSession
	// call now that ConnectionID is re-read per rebuild instead of cached once) - previously
	// unsynchronized, harmless only because connectionID used to be write-once.
	connectionIDMu sync.RWMutex
	connectionID   string

	// lastFrameMu guards lastFrame: the time any frame was last received on the current RTA
	// websocket. Used by IsStale to detect a connection that never produced a read error but
	// also stopped actually delivering anything - see IsStale's doc comment.
	lastFrameMu sync.RWMutex
	lastFrame   time.Time

	ready     chan struct{}
	readyOnce sync.Once
}

func NewRTA(authSession *session.Session, xuid string, log *slog.Logger) *RTA {
	return &RTA{auth: authSession, xuid: xuid, log: log, ready: make(chan struct{})}
}

// Connect dials the RTA websocket, subscribes to session-directory connection notifications
// and to the user's friends feed, and processes events until ctx is cancelled or the
// connection drops (call Connect again to reconnect - SessionManagerCore.checkConnection()
// does this reactively in the Java version; a simple retry loop works fine here too).
func (r *RTA) Connect(ctx context.Context) error {
	tok, err := r.auth.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return fmt.Errorf("request xbl token for rta: %w", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://xboxlive.com/", nil)
	tok.SetAuthHeader(req)
	header := http.Header{}
	header.Set("Authorization", req.Header.Get("Authorization"))

	conn, _, err := websocket.Dial(ctx, rtaWebsocketURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return fmt.Errorf("dial rta websocket: %w", err)
	}
	r.conn = conn
	r.touchLastFrame()

	if err := wsjson.Write(ctx, conn, []any{msgTypeSubscribe, 1, "https://sessiondirectory.xboxlive.com/connections/"}); err != nil {
		return fmt.Errorf("subscribe to session directory connections: %w", err)
	}

	// coder/websocket does not auto-ping (unlike org.java_websocket's WebSocketClient, which the
	// real Xbox client this was ported from relies on for its default connection-lost timeout -
	// see RtaWebsocketClient's own doc comment). Without an active probe, IsStale can only judge
	// "time since Xbox Live last happened to push us something", which is unreliable during
	// genuinely quiet stretches (no session changes, no friend requests) and would false-positive
	// on a perfectly healthy connection. Sending our own WS-level ping and waiting for the pong
	// gives a real round-trip health signal instead, and doubles as keeping the connection warm
	// through any idle-timing NAT/proxy in the path.
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					// A failed/timed-out ping is a direct, active signal the connection is dead
					// - stronger than IsStale's passive "nothing arrived in a while" inference.
					// Force the reconnect now rather than waiting for the next health-check tick.
					r.ForceReconnect("ping failed: " + err.Error())
					return
				}
				r.touchLastFrame()
			}
		}
	}()

	for {
		var parts []json.RawMessage
		if err := wsjson.Read(ctx, conn, &parts); err != nil {
			return fmt.Errorf("rta read: %w", err)
		}
		r.touchLastFrame()
		r.handle(ctx, parts)
	}
}

func (r *RTA) touchLastFrame() {
	r.lastFrameMu.Lock()
	r.lastFrame = time.Now()
	r.lastFrameMu.Unlock()
}

// IsStale reports whether the RTA websocket has gone silent for longer than maxAge without
// producing a read error - i.e. genuinely dead, but in a way Connect's loop can't detect on its
// own, since it only returns when wsjson.Read actually fails.
//
// A websocket can stop delivering anything (silently dropped by a NAT timeout, a router losing
// connection state, or the remote closing without sending a clean close frame our TCP stack
// happens to notice) while still looking "open" from Go's side - the read call just blocks
// forever waiting for bytes that will never arrive. Nothing about that state produces an error,
// so main's existing reconnect-on-error loop never fires, and RTA.ConnectionID keeps returning a
// value that LOOKS set but belongs to a connection Xbox Live itself has long since stopped
// treating as active - session/activity-handle creation then fails with "the owner isn't active
// in the referenced session" for as long as this goes unnoticed, exactly matching a ~2h dead
// window found in run.log with zero errors logged anywhere (2026-08-21).
//
// This mirrors the real MCXboxBroadcastExtension client's SessionManagerCore.checkConnection(),
// which actively polls both its RTA websocket and NetherNet channel's open/closed state on every
// periodic session-update tick, rather than only reacting to an explicit disconnect event -
// confirmed by decompiling its actual class files (com.rtm516.mcxboxbroadcast.core, CFR
// decompile, 2026-08-21) after "people can't join after a few hours" kept recurring despite the
// earlier connectionID and nonce fixes. Call this on the same periodic cadence as our own
// session-update ticker; if stale, call ForceReconnect to actually recover (checking alone does
// nothing - see ForceReconnect's doc comment).
func (r *RTA) IsStale(maxAge time.Duration) bool {
	r.lastFrameMu.RLock()
	last := r.lastFrame
	r.lastFrameMu.RUnlock()
	if last.IsZero() {
		return false // never connected yet - not our job to report that as "stale"
	}
	return time.Since(last) > maxAge
}

// ForceReconnect closes the current RTA websocket, causing Connect's blocked read call to
// unblock with an error - which is what actually triggers main's existing reconnect goroutine to
// redial and obtain a fresh ConnectionId. Detecting staleness (IsStale) without this does
// nothing on its own; this is the repair step, not just the check.
func (r *RTA) ForceReconnect(reason string) {
	if r.conn != nil {
		r.log.Warn("forcing rta reconnect", "reason", reason)
		_ = r.conn.Close(websocket.StatusNormalClosure, "forced reconnect: "+reason)
	}
}

func (r *RTA) handle(ctx context.Context, parts []json.RawMessage) {
	if len(parts) < 1 {
		return
	}
	var msgType int
	_ = json.Unmarshal(parts[0], &msgType)

	if r.log.Enabled(context.Background(), -4) { // slog.LevelDebug
		raw := make([]string, 0, len(parts))
		for _, p := range parts {
			raw = append(raw, string(p))
		}
		r.log.Debug("rta frame", "type", msgType, "parts", raw)
	}

	switch msgType {
	case msgTypeSubscribe:
		// [1, seq, statusCode, subscriptionId, data]
		if len(parts) < 5 {
			return
		}
		var data map[string]any
		if err := json.Unmarshal(parts[4], &data); err == nil {
			if cid, ok := data["ConnectionId"].(string); ok && cid != "" {
				// Update on every (re)connect, not just the first - the guard used to be
				// `r.connectionID == ""`, which set this once and then never again. A
				// ConnectionId belongs to one specific RTA websocket connection; once that
				// socket drops and Connect's caller reconnects (see main.go's retry loop
				// around rta.Connect), Xbox Live no longer considers the OLD connection ID
				// active. Any xblSession.Create call still using it fails with "the owner
				// isn't active in the referenced session" - a genuinely correct rejection,
				// since that connection really is dead, just not the error anyone reading it
				// would guess the cause of. Confirmed 2026-08-21: RTA reconnected at 08:12:33
				// (session.go's own reconnect loop handled the drop fine), but connectionID
				// was never refreshed, so every session rebuild after that point - including
				// the one forced by a real StatusGoingAway over an hour later - referenced a
				// connection ID that had been stale the whole time.
				r.connectionIDMu.Lock()
				r.connectionID = cid
				r.connectionIDMu.Unlock()
				r.readyOnce.Do(func() { close(r.ready) })
				if r.OnConnectionIDChanged != nil {
					r.OnConnectionIDChanged(cid)
				}
				// Now subscribe to the friends feed too, matching the Java client. Re-sent on
				// every (re)connect, not just the first: this subscription lives on the
				// websocket connection itself, so a fresh connection after a drop has none of
				// the old socket's subscriptions and needs this again or friend-request
				// notifications silently stop after the first RTA reconnect.
				_ = wsjson.Write(ctx, r.conn, []any{msgTypeSubscribe, 2, fmt.Sprintf("https://social.xboxlive.com/users/xuid(%s)/friends", r.xuid)})
			}
		}
	case msgTypeEvent:
		// [3, subscriptionId, data]
		if len(parts) < 3 {
			return
		}
		var data map[string]any
		if err := json.Unmarshal(parts[2], &data); err != nil {
			return
		}
		if nt, _ := data["NotificationType"].(string); nt == "IncomingFriendRequestCountChanged" {
			if r.OnFriendRequest != nil {
				r.OnFriendRequest()
			}
		}
		if _, ok := data["ncid"]; ok {
			if r.OnSessionChanged != nil {
				r.OnSessionChanged()
			}
		}
	case msgTypeResync:
		r.log.Debug("rta resync requested")
		if r.OnSessionChanged != nil {
			r.OnSessionChanged()
		}
	}
}

// ConnectionID blocks until the RTA connection ID has FIRST been received, then always returns
// the current value - which may have changed since the first call if the RTA websocket dropped
// and reconnected in between (see handle's ConnectionId case). Call this again on every session
// rebuild rather than caching its result, or a stale connection ID will make Xbox Live reject
// session/activity-handle creation with "the owner isn't active in the referenced session".
func (r *RTA) ConnectionID(ctx context.Context) (string, error) {
	select {
	case <-r.ready:
		r.connectionIDMu.RLock()
		defer r.connectionIDMu.RUnlock()
		return r.connectionID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(10 * time.Second):
		return "", fmt.Errorf("timed out waiting for rta connection id")
	}
}
