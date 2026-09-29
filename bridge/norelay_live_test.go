package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/session"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

// TestNoRelayLiveBackend runs a whole no-relay join against a real backend: a go-nethernet Dialer
// plays the player's client and trickles its candidates exactly as one does over Xbox Live, the
// real signalBroker forwards the offer, and the backend answers. It passes only if WebRTC comes up
// DIRECTLY between the dialer and the backend with nothing relaying the data.
//
// This is the test the unit tests cannot stand in for. They prove the forwarded offer matches the
// layout go-nethernet's description.encode produces, which go-nethernet (and so Dragonfly) parses.
// A native BDS parses SDP with Mojang's own C++ code instead, and a trickling client's offer keeps
// its m=/c= placeholders where a non-trickle one would carry a real candidate - only the real
// server can say whether that is accepted.
//
// It mints a real client identity, since BDS refuses anonymous peers, so it needs a signed-in
// token. It never logs in to Minecraft: the connection is closed once WebRTC is up, so no player
// joins and nothing is written to the world. Skipped unless both are set:
//
//	N2R_LIVE_BACKEND=http://127.0.0.1:19134
//	N2R_TOKEN=/path/to/token.json   (refreshed tokens are written back, as the relay does)
func TestNoRelayLiveBackend(t *testing.T) {
	backend, tokenPath := os.Getenv("N2R_LIVE_BACKEND"), os.Getenv("N2R_TOKEN")
	if backend == "" || tokenPath == "" {
		t.Skip("set N2R_LIVE_BACKEND and N2R_TOKEN to run against a real backend")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	s, err := liveSession(ctx, tokenPath)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	identity, err := NewClientIdentity(ctx, s)
	if err != nil {
		t.Fatalf("mint client identity: %v", err)
	}

	// Who the token claims to be, read without verifying - only to know what the broker, which
	// does verify it against the real authorization service, should come up with.
	claimed := unverifiedTokenClaims(t, identity.Token)
	verify := SessionTokenVerifier(s)

	// Trickle ICE left ON, as a real client over Xbox Live signaling has it. That is the whole
	// point: the relay's own backend dial disables it, and so has never exercised this path.
	dial := func(ctx context.Context, pipe *signalPipe) (*nethernet.Conn, error) {
		dialer := nethernet.Dialer{Identity: identity, Log: log.With("src", "fake-client")}
		return dialer.DialContext(ctx, pipe.hostID, pipe.client())
	}
	// startBroker wires a broker to a fresh pipe. The host's ID is a UUID on purpose: that is what
	// the messaging transport - the one newer clients prefer - reports, and it must never reach
	// the backend's join path.
	startBroker := func(allow func(string) bool, onJoin func(string, string)) *signalPipe {
		pipe := newSignalPipe(ctx, "live-client-1", "3f1c9e2a-7b4d-4e8f-9a01-2c3d4e5f6a7b")
		broker, err := newSignalBroker("live", pipe.host(), Config{
			NetherNetBackendAddress: backend,
			VerifyPlayerToken:       verify,
			AllowXUID:               allow,
			OnJoin:                  onJoin,
		}, log)
		if err != nil {
			t.Fatalf("broker: %v", err)
		}
		go func() { _ = broker.Run(ctx) }()
		pipe.waitForHost(t)
		return pipe
	}

	// A player on allowed_xuids joins, and the join is recorded under their verified XUID.
	var joins joinRecorder
	pipe := startBroker(func(x string) bool { return x == claimed.XUID }, joins.record)
	start := time.Now()
	conn, err := dial(ctx, pipe)
	if err != nil {
		t.Fatalf("no-relay join failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
	}
	defer conn.Close()

	t.Logf("connected straight to the backend in %v", time.Since(start).Round(time.Millisecond))
	t.Logf("  remote:  %s", conn.RemoteAddr())
	t.Logf("  client candidates trickled through the broker: %d", pipe.candidatesSent())
	if pipe.candidatesSent() == 0 {
		t.Error("the client trickled no candidates, so the trickle path was not actually exercised")
	}
	if got, want := joins.got(), []string{claimed.XUID + "/" + claimed.Name}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("friend activity recorded %q, want %q", got, want)
	} else {
		t.Logf("  verified against the authorization service and recorded: %s", got[0])
	}

	// The same player, not on the list, is turned away before the backend is ever asked.
	refusedCtx, cancelRefused := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRefused()
	var refusedJoins joinRecorder
	refused := startBroker(func(string) bool { return false }, refusedJoins.record)
	if conn, err := dial(refusedCtx, refused); err == nil {
		conn.Close()
		t.Error("a player not in allowed_xuids connected")
	} else {
		t.Logf("player not in allowed_xuids refused: %v", err)
	}
	if got := refusedJoins.got(); len(got) != 0 {
		t.Errorf("a refused join was recorded as activity: %q", got)
	}
}

// unverifiedTokenClaims decodes a JWT's xid and xname without checking its signature.
func unverifiedTokenClaims(t *testing.T, token string) PlayerToken {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode token payload: %v", err)
	}
	var claims struct {
		XUID string `json:"xid"`
		Name string `json:"xname"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.XUID == "" {
		t.Fatalf("token has no xid claim: %v", err)
	}
	return PlayerToken{XUID: claims.XUID, Name: claims.Name}
}

// signalPipe is an in-memory stand-in for Xbox Live signaling between one client and one host.
//
// Xbox Live rewrites NetworkID in transit: a client addresses the host's ID, and the host sees the
// client's ID as the sender. The pipe does the same, or the dialer and the broker would each drop
// the other's signals as meant for someone else.
type signalPipe struct {
	ctx              context.Context
	clientID, hostID string

	mu         sync.Mutex
	hostNotify nethernet.Notifier
	hostReady  chan struct{}
	clientSubs []nethernet.Notifier
	candidates int
}

func newSignalPipe(ctx context.Context, clientID, hostID string) *signalPipe {
	return &signalPipe{ctx: ctx, clientID: clientID, hostID: hostID, hostReady: make(chan struct{})}
}

func (p *signalPipe) client() nethernet.Signaling { return pipeEnd{p, false} }
func (p *signalPipe) host() nethernet.Signaling   { return pipeEnd{p, true} }

func (p *signalPipe) waitForHost(t *testing.T) {
	t.Helper()
	select {
	case <-p.hostReady:
	case <-time.After(5 * time.Second):
		t.Fatal("broker never subscribed to the signaling pipe")
	}
}

func (p *signalPipe) candidatesSent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.candidates
}

// pipeEnd is one side of a signalPipe.
type pipeEnd struct {
	p      *signalPipe
	isHost bool
}

func (e pipeEnd) Signal(_ context.Context, s *nethernet.Signal) error {
	out := *s
	e.p.mu.Lock()
	var targets []nethernet.Notifier
	if e.isHost {
		out.NetworkID = e.p.hostID // the client sees the host as the sender
		targets = append(targets, e.p.clientSubs...)
	} else {
		out.NetworkID = e.p.clientID // the host sees the client as the sender
		if e.p.hostNotify != nil {
			targets = append(targets, e.p.hostNotify)
		}
		if s.Type == nethernet.SignalTypeCandidate {
			e.p.candidates++
		}
	}
	e.p.mu.Unlock()
	for _, n := range targets {
		n.NotifySignal(&out)
	}
	return nil
}

func (e pipeEnd) Notify(n nethernet.Notifier) func() {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	if e.isHost {
		e.p.hostNotify = n
		close(e.p.hostReady)
	} else {
		e.p.clientSubs = append(e.p.clientSubs, n)
	}
	return func() {}
}

func (e pipeEnd) Context() context.Context { return e.p.ctx }
func (e pipeEnd) PongData([]byte)          {}

// Credentials returns no STUN/TURN servers: client and backend share a host, so ICE only ever
// needs the host candidates, as with the relay's own backend dial.
func (e pipeEnd) Credentials(context.Context) (*nethernet.Credentials, error) {
	return &nethernet.Credentials{}, nil
}

func (e pipeEnd) NetworkID() string {
	if e.isHost {
		return e.p.hostID
	}
	return e.p.clientID
}

// liveSession signs in from a cached token the way the relay does, writing refreshed tokens back
// so the cache stays valid for the relay afterwards.
func liveSession(ctx context.Context, path string) (*session.Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tok := new(oauth2.Token)
	if err := json.Unmarshal(b, tok); err != nil {
		return nil, err
	}
	src := &liveTokenWriteBack{src: auth.AndroidConfig.RefreshTokenSource(tok), path: path}
	return session.SessionFromTokenSource(src, auth.AndroidConfig, ctx)
}

type liveTokenWriteBack struct {
	src  oauth2.TokenSource
	path string
}

func (w *liveTokenWriteBack) Token() (*oauth2.Token, error) {
	tok, err := w.src.Token()
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(w.path, b, 0o600)
	}
	return tok, nil
}
