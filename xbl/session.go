package xbl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gameparrot/netherconnect/session"
	"github.com/google/uuid"
)

const (
	ServiceConfigID = "4fc10100-5f7a-4470-899b-280835760c07"
	TemplateName    = "MinecraftLobby"

	// TitleId is 0, confirmed against a live, working MCXboxBroadcast Geyser-extension session
	// captured via its own "mcxboxbroadcast dumpsession" command (2026-08-13). A prior version
	// of this file used 896928775, guessing it needed a real Minecraft title ID - that guess
	// was wrong and is a likely reason the session never appeared on the Friends tab.
	TitleId = 0

	sessionURLFmt   = "https://sessiondirectory.xboxlive.com/serviceconfigs/%s/sessionTemplates/%s/sessions/%s"
	createHandleURL = "https://sessiondirectory.xboxlive.com/handles"

	xblContractVersion = "107"
)

// Config describes the world being advertised. Update the exported fields (through the
// Session's methods, see SetWorldInfo) any time the underlying server's state changes and
// call Update to push it to Xbox Live.
type Config struct {
	HostName   string
	WorldName  string
	Players    int
	MaxPlayers int
	Protocol   int32
	Version    string
}

// Session owns one Xbox Live multiplayer session (an MPSD document under the MinecraftLobby
// template) and keeps it alive/updated/joinable. This is the Go equivalent of
// com.rtm516.mcxboxbroadcast.core.SessionManagerCore + SessionManager.
type Session struct {
	auth *session.Session
	xuid string
	log  *slog.Logger

	// connectionIDMu guards connectionID: written once at construction, and now also from
	// SetConnectionID when RTA reconnects mid-session (see SetConnectionID's doc comment) -
	// read concurrently from buildRequest() on the periodic Update ticker.
	connectionIDMu sync.RWMutex
	connectionID   string
	sessionID      string
	handleID       string

	netherNetID uint64
	pmsgID      string
	levelID     string

	cfg   Config
	cfgMu sync.Mutex

	// subscriptionID must stay STABLE across session updates. bedrock-portal reuses one
	// subscription ID for the host's lifetime; regenerating it on every PUT (as an earlier
	// version of this file did) churns the subscription and can break join notifications.
	subscriptionID string

	nonces      map[string]string
	// nonceConnections tracks, per XUID, which member-slot "connection" value the current
	// nonce was issued for. Xbox Live assigns a new connection value each time a XUID
	// (re)joins the session's member list; without tracking it, RefreshNonces could only tell
	// "have we ever seen this XUID" and would keep reusing a stale nonce across every later
	// rejoin - see RefreshNonces' doc comment for the join failure this caused.
	nonceConnections map[string]string
	noncesMu         sync.Mutex

	httpClient *http.Client
}

// New creates a Session. connectionID must be the RTA connection ID (see RTA.ConnectionID())
// obtained from an active RTA subscription - Xbox Live requires this to correlate the
// session's own member entry with a live real-time-activity connection, otherwise joins
// silently fail. pmsgID is the NetherNet messaging ID for this listener (messaging.Conn.NetworkID()).
// netherNetID MUST be the network ID our NetherNet listener is actually bound to (see
// bridge.Listen, which returns it). go-nethernet resolves a listener's ID by parsing the
// signaling service's NetworkID as a uint64, and clients route to exactly that value - so
// advertising anything else (e.g. a freshly generated random ID, as an earlier version of
// this file did) makes the world appear in the Friends tab but silently fail to join.
//
// sessionID identifies the MPSD session document itself and MUST be preserved across a
// signaling-side rebuild (pass the previous Session's SessionID()), not regenerated - pass ""
// only on the very first call of the process. Xbox Live tracks each already-joined player as a
// member of one specific session document; every prior version of this function generated a
// fresh random sessionID on every call, so a rebuild (Xbox Live force-closing the signaling
// socket, or our own proactive token renewal) silently created a brand-new, empty session while
// already-connected players' actual game connections kept working underneath (connCtx
// decoupling - see bridge/listener.go). Their client never got told to re-join the new session,
// so Xbox Live's own bookkeeping showed them gone even though they were still playing.
// Confirmed 2026-08-22: a rebuild at 03:55:02 left the session with only the host as a member
// while NearYeti7308883 (joined 03:39:27, still actively playing) never reappeared in any
// subsequent session put response, and new join attempts from two different accounts/devices
// received no CONNECTREQUEST at all in the 30+ minutes that followed.
func New(authSession *session.Session, xuid, sessionID, connectionID, pmsgID string, netherNetID uint64, log *slog.Logger) *Session {
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// levelID is derived from the (now stable) sessionID rather than freshly randomised, so it
	// does not change when a rebuild constructs a new Session. It still differs between separate
	// broadcaster instances, which is what the uniqueness note below actually requires; what it
	// must NOT do is change identity out from under a client mid-session. MCXboxBroadcast sends
	// the constant "level" here.
	levelSum := sha256.Sum256([]byte(sessionID))
	return &Session{
		auth:           authSession,
		xuid:           xuid,
		log:            log,
		connectionID:   connectionID,
		sessionID:      sessionID,
		netherNetID:    netherNetID,
		pmsgID:         pmsgID,
		levelID:        base64.StdEncoding.EncodeToString(levelSum[:8]),
		subscriptionID:   uuid.NewString(),
		nonces:           make(map[string]string),
		nonceConnections: make(map[string]string),
		httpClient:       &http.Client{},
	}
}

func (s *Session) SessionID() string { return s.sessionID }

// ConnectionID returns the RTA connection ID currently believed to be live, guarded against
// concurrent updates from SetConnectionID.
func (s *Session) ConnectionID() string {
	s.connectionIDMu.RLock()
	defer s.connectionIDMu.RUnlock()
	return s.connectionID
}

// SetConnectionID updates the RTA connection ID this session's own member entry advertises,
// WITHOUT rebuilding the session document, NetherNet listener, or disconnecting anyone. Call
// this whenever the RTA websocket reconnects mid-session (see rta.go's ConnectionId handling)
// and then call Update to push the corrected value.
//
// Before this existed, a Session object's connectionID was set once at construction (New) and
// never touched again - every subsequent Update() PUT kept advertising whichever connection ID
// was current AT SESSION-OBJECT-CREATION TIME, even after RTA silently reconnected and Xbox
// Live issued a completely different one for the new websocket. go-xsapi (df-mc's own MPSD
// client) documents the correct behavior directly: "concurrent RTA reconnect handlers can
// refresh this session with the latest connection ID" (mpsd/join.go, reconcileSessionConnection)
// - a dedicated reconciliation path, not a full session rebuild. This codebase had no equivalent
// at all: an RTA reconnect (main.go's rta.Connect retry loop) got a fresh ConnectionId from Xbox
// Live but nothing ever told the live Session object about it, so every PUT after that point
// advertised a connection ID Xbox Live itself considered dead - a very plausible explanation for
// "session ended, restarting" err="...the owner isn't active in the referenced session" firing
// with no other errors anywhere in the log (confirmed 2026-08-23: RTA reconnected at 01:43:28,
// and the very next handle-related call started failing with exactly that error at 01:44:21,
// with the session's own PUTs still returning 200 the whole time).
func (s *Session) SetConnectionID(connectionID string) {
	s.connectionIDMu.Lock()
	s.connectionID = connectionID
	s.connectionIDMu.Unlock()
}

// SetWorldInfo updates the locally-cached world metadata. Call Update afterwards to push it.
func (s *Session) SetWorldInfo(cfg Config) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
}

func (s *Session) authHeader(ctx context.Context) (string, error) {
	tok, err := s.auth.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return "", fmt.Errorf("request xbl token: %w", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://sessiondirectory.xboxlive.com/", nil)
	tok.SetAuthHeader(req)
	return req.Header.Get("Authorization"), nil
}

func (s *Session) buildRequest() CreateSessionRequest {
	s.cfgMu.Lock()
	cfg := s.cfg
	s.cfgMu.Unlock()

	s.noncesMu.Lock()
	nonceCopy := make(map[string]string, len(s.nonces))
	for k, v := range s.nonces {
		nonceCopy[k] = v
	}
	s.noncesMu.Unlock()

	return CreateSessionRequest{
		Members: map[string]SessionMember{
			"me": {
				Constants: map[string]MemberConstantsSystem{
					"system": {Xuid: s.xuid, Initialize: true},
				},
				Properties: map[string]MemberPropertiesSystem{
					"system": {
						Active:     true,
						Connection: s.ConnectionID(),
						Subscription: MemberSubscription{
							ID:          s.subscriptionID,
							ChangeTypes: []string{"everything"},
						},
					},
				},
			},
		},
		Properties: SessionProperties{
			System: DefaultSessionSystemProperties(),
			Custom: SessionCustomProperties{
				BroadcastSetting:  3,
				CrossPlayDisabled: false,
				Joinability:       "joinable_by_friends",
				// NOTE: MCXboxBroadcast's CreateSessionRequest passes false here, and an old comment
				// on this line claimed a 2026-08-13 live capture also showed false. Left at true
				// deliberately: joins demonstrably succeed with true (players joined all through
				// 2026-08-22), so this is not a known-broken value, and flipping it at the same
				// time as the netherNetID fix would make that fix untestable. Revisit on its own.
				LanGame:                 true,
				MaxMemberCount:          cfg.MaxPlayers,
				MemberCount:             cfg.Players,
				OnlineCrossPlatformGame: true,
				SupportedConnections: []Connection{
					{ConnectionType: 7, HostIpAddress: "", HostPort: 0, NetherNetId: s.netherNetID, PmsgId: s.pmsgID},
				},
				// Confirmed 0 against a live working session capture (2026-08-13) - see the
				// TitleId constant doc comment.
				TitleId:        TitleId,
				TransportLayer: 2,
				// levelId must be UNIQUE per broadcaster instance, not a shared constant.
				// The real Nether2Rak's own changelog documents "Nether2Rak instances
				// colliding with each other on the world menu" as a real bug fixed multiple
				// times (and eventually required a full recode) - consistent with every
				// instance sharing the same literal levelId causing the client to hide/
				// overwrite entries. Using the session's own UUID guarantees uniqueness.
				LevelId:       s.levelID,
				HostName:      cfg.HostName,
				OwnerId:       s.xuid,
				RakNetGUID:    "",
				WorldName:     cfg.WorldName,
				WorldType:     "Survival",
				Protocol:      int(cfg.Protocol),
				Version:       cfg.Version,
				IsEditorWorld: false,
				IsHardcore:    false,
				Nonces:        nonceCopy,
			},
		},
	}
}

func (s *Session) put(ctx context.Context, body any) (*CreateSessionResponse, error) {
	auth, err := s.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal session body: %w", err)
	}
	url := fmt.Sprintf(sessionURLFmt, ServiceConfigID, TemplateName, s.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("put session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	s.log.Debug("session put response", "status", resp.StatusCode, "body", string(respBody))
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("put session: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed CreateSessionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse session response: %w", err)
	}
	return &parsed, nil
}

// Create creates the MPSD session document and binds an activity handle to it so it actually
// shows up as joinable in the Friends tab. Equivalent of SessionManagerCore.createSession().
func (s *Session) Create(ctx context.Context) error {
	sessionReq := s.buildRequest()
	s.log.Debug("creating session",
		"titleId", sessionReq.Properties.Custom.TitleId,
		"lanGame", sessionReq.Properties.Custom.LanGame,
		"broadcastSetting", sessionReq.Properties.Custom.BroadcastSetting,
		"levelId", sessionReq.Properties.Custom.LevelId,
		"netherNetId", sessionReq.Properties.Custom.SupportedConnections[0].NetherNetId,
		"pmsgId", s.pmsgID,
	)
	if _, err := s.put(ctx, sessionReq); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	auth, err := s.authHeader(ctx)
	if err != nil {
		return err
	}
	handleReq := CreateHandleRequest{
		Version: 1,
		Type:    "activity",
		SessionRef: SessionRef{
			Scid:         ServiceConfigID,
			TemplateName: TemplateName,
			Name:         s.sessionID,
		},
	}
	b, _ := json.Marshal(handleReq)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createHandleURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create handle: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("create handle: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsedHandle CreateHandleResponse
	if err := json.Unmarshal(respBody, &parsedHandle); err != nil {
		return fmt.Errorf("parse handle response: %w", err)
	}
	s.handleID = parsedHandle.Id
	s.log.Info("xbox live session created", "sessionID", s.sessionID, "handleID", s.handleID)
	return nil
}

// Update pushes the current world info + nonces to Xbox Live. Call this periodically (e.g.
// every 30s) and any time SetWorldInfo changes something, mirroring StandaloneMain's
// scheduleWithFixedDelay loop.
func (s *Session) Update(ctx context.Context) error {
	_, err := s.put(ctx, s.buildRequest())
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	return nil
}

// RefreshNonces re-reads the live session member list from Xbox Live and (re)generates a
// nonce for every member XUID other than our own, removing nonces for members who left. If
// anything changed it pushes an updated session document. This must run whenever the RTA
// websocket reports a session change (an "ncid" event) - see rta.go - or joining will
// intermittently fail, matching the "queue/resync" fixes in the real Nether2Rak devlog.
//
// A nonce is regenerated whenever a XUID's member-slot "connection" value changes, not merely
// when the XUID is new to s.nonces. Xbox Live assigns a fresh connection value on every real
// (re)join, and the nonce authorizes that SPECIFIC join attempt's WebRTC/ICE handshake - keying
// only on "have we ever seen this XUID" meant a player's first join in a session's lifetime
// worked, but every later rejoin silently failed the handshake while still showing as joined in
// the Xbox Live session itself, since they kept being checked against their original, by-then-
// stale nonce. Confirmed 2026-08-21: a player rejoined the same session 5 times over ~6 minutes,
// XBL accepted the session-membership each time, but only the very first attempt (a different
// process run, hours earlier) ever completed signaling - identical "connection" absence in
// SessionMemberResponse before this fix meant there was nothing to compare against at all.
func (s *Session) RefreshNonces(ctx context.Context) error {
	auth, err := s.authHeader(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf(sessionURLFmt, ServiceConfigID, TemplateName, s.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("get session: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed CreateSessionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("parse session response: %w", err)
	}

	// activeConn maps xuid -> the "connection" value of their current member-slot. A member
	// system entry with no matching properties/connection (shouldn't normally happen, but the
	// API technically allows partial data) falls back to the xuid itself so it's still treated
	// as present, just without connection-change detection for that one entry.
	activeConn := make(map[string]string)
	for _, m := range parsed.Members {
		sys, ok := m.Constants["system"]
		if !ok || sys.Xuid == "" || sys.Xuid == s.xuid {
			continue
		}
		conn := sys.Xuid
		if props, ok := m.Properties["system"]; ok && props.Connection != "" {
			conn = props.Connection
		}
		activeConn[sys.Xuid] = conn
	}

	s.noncesMu.Lock()
	changed := false
	for xuid := range s.nonces {
		if _, ok := activeConn[xuid]; !ok {
			delete(s.nonces, xuid)
			delete(s.nonceConnections, xuid)
			changed = true
		}
	}
	for xuid, conn := range activeConn {
		if s.nonceConnections[xuid] == conn {
			continue // same join we already issued a nonce for - not a new attempt
		}
		var nb [8]byte
		_, _ = rand.Read(nb[:])
		s.nonces[xuid] = fmt.Sprintf("%x", nb)
		s.nonceConnections[xuid] = conn
		changed = true
	}
	s.noncesMu.Unlock()

	if changed {
		return s.Update(ctx)
	}
	return nil
}
