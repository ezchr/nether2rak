package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/session"
)

// This file implements the dial half of Bedrock's HTTP-based NetherNet signaling, which is how
// a Bedrock Dedicated Server running `transport=nethernet` is actually reached.
//
// Two mechanisms get confused for each other and only one of them applies here:
//
//   - LAN discovery (go-nethernet/discovery, UDP broadcast on port 7551). This is what a client
//     uses to find worlds on the local network. BDS in transport=nethernet mode does NOT use it
//     for dedicated-server joins - confirmed 2026-09-15 by `tcpdump -i lo udp port 7551` during
//     a join attempt, which captured ZERO packets from BDS even though it did hold the socket
//     open. An earlier attempt at a NetherNet backend was built on this and could never have
//     worked.
//
//   - HTTP signaling on BDS's TCP server-port. This is the real one. Confirmed the same day by
//     `curl -v http://127.0.0.1:19134/` against a live BDS returning a genuine HTTP 404, i.e.
//     there is an actual HTTP server there, and it is documented in Mojang's own NetherNet
//     onboarding guide.
//
// Rather than hand-rolling the HTTP exchange, this is a port of the upstream first-party
// implementation, github.com/df-mc/go-nethernet/endpoint.Client, which landed in go-nethernet
// v1.0.20 (2026-08-10) and did not exist when this project pinned its dependency.
//
// It was ported rather than imported because this project was pinned to a go-nethernet snapshot
// older than the endpoint package. The project now builds against upstream main, so this could be
// replaced by importing endpoint.Client directly; it is kept as-is for now because it is the code
// path that was live-tested against BDS and Geyser.
//
// The port keeps upstream's behaviour exactly, including three things the earlier hand-written
// attempt in this codebase got wrong:
//
//  1. NetworkID direction. There are two different network IDs in one exchange and they are not
//     interchangeable. The value passed to Dialer.DialContext (and carried on Signal.NetworkID)
//     is the REMOTE SERVER's base URL, e.g. "http://127.0.0.1:19134". The value in the request
//     PATH, /v1/join/{networkID}, is OUR OWN client-side ID. The previous attempt used one
//     fixed base URL and put a freshly generated ID in the path while also returning that same
//     ID from NetworkID(), which is what produced BDS's
//     `400 Expected /v1/join/{networkId}`.
//  2. Own-ID format. Upstream generates it as `strconv.FormatUint(rand.Uint64(), 10)` - a
//     DECIMAL string. A hex string is rejected by BDS 1.26.50.5 with that same 400; decimal is
//     accepted with a 200. Confirmed live on 2026-09-15 against the real server.
//  3. The libhttpclient User-Agent, which is what the real Bedrock client's HTTP stack sends.
const maxSDPBodySize int64 = 1 << 20

// httpSignaling implements the pinned nethernet.Signaling interface over the HTTP endpoints a
// NetherNet-speaking Bedrock server exposes. Construct it with newHTTPSignaling and hand it to
// nethernet.Dialer.DialContext along with the backend's base URL.
//
// Trickle ICE is impossible over this transport: a single HTTP request/response carries the whole
// exchange, so there is no channel on which to send additional candidates afterwards. Callers
// MUST set nethernet.Dialer.DisableTrickleICE, which makes the dialer gather every local
// candidate up front and embed them in the offer. dialNetherNetBackend does this.
type httpSignaling struct {
	// networkID is this client's OWN identifier, used only as the /v1/join/{networkID} path
	// segment. It is not the server's address - see the NetworkID direction note above.
	networkID string

	client *http.Client
	log    *slog.Logger

	notifiersMu sync.Mutex
	notifiers   map[uint32]nethernet.Notifier
	notifyCount uint32
	closed      bool
}

func newHTTPSignaling(log *slog.Logger, client *http.Client) *httpSignaling {
	if client == nil {
		client = http.DefaultClient
	}
	if log == nil {
		log = slog.Default()
	}
	return &httpSignaling{
		// Decimal, not hex - see point 2 in the file comment above.
		networkID: strconv.FormatUint(mrand.Uint64(), 10),
		client:    client,
		log:       log,
		notifiers: make(map[uint32]nethernet.Notifier),
	}
}

// Signal sends signal to the remote server. Only nethernet.SignalTypeOffer does any work: the
// offer SDP is POSTed to the server and the SDP answer comes back in the same HTTP response,
// which is then handed to the dialer through the channels registered via Notify - the dialer
// expects answers to arrive asynchronously, even though this transport happens to produce one
// synchronously.
func (h *httpSignaling) Signal(ctx context.Context, signal *nethernet.Signal) error {
	u, err := url.Parse(signal.NetworkID)
	if err != nil {
		return fmt.Errorf("parse network ID as URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.Port() == "" {
		return fmt.Errorf("network ID must be a HTTP/HTTPS URL with port: %s", signal.NetworkID)
	}

	switch signal.Type {
	case nethernet.SignalTypeOffer:
		requestURL := u.JoinPath("/v1/join", h.networkID).String()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(signal.Data))
		if err != nil {
			return fmt.Errorf("make request: %w", err)
		}
		req.Header.Set("Content-Type", "application/sdp")
		// What the real Bedrock client's HTTP stack identifies itself as.
		req.Header.Set("User-Agent", "libhttpclient/1.0.0.0")

		resp, err := h.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %s: %s", req.Method, req.URL, resp.Status)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxSDPBodySize+1))
		if err != nil {
			return fmt.Errorf("read response body: %w", err)
		}
		if int64(len(b)) > maxSDPBodySize {
			return fmt.Errorf("SDP answer exceeds %d bytes", maxSDPBodySize)
		}
		if len(b) == 0 {
			return errors.New("missing SDP answer in response body")
		}
		// A body that parses as a bare integer is a NetherNet error code, not an answer - the
		// server rejected the negotiation and said why in-band with a 200.
		if errorCode, err := strconv.ParseUint(string(b), 10, 32); err == nil {
			return fmt.Errorf("negotiation failed with error code: %d", errorCode)
		}

		// ConnectionID and NetworkID must be echoed back exactly: Dialer.notifySignals drops any
		// signal whose pair doesn't match the dial in progress, and a dropped answer shows up as
		// nothing more informative than a negotiation timeout.
		h.notifySignal(&nethernet.Signal{
			Type:         nethernet.SignalTypeAnswer,
			ConnectionID: signal.ConnectionID,
			Data:         string(b),
			NetworkID:    signal.NetworkID,
		})
		return nil
	case nethernet.SignalTypeError:
		return nil
	case nethernet.SignalTypeCandidate:
		// Reachable only if DisableTrickleICE was not set on the dialer.
		return errors.New("trickle ICE is not supported over HTTP signaling")
	default:
		return fmt.Errorf("unknown signal type: %s", signal.Type)
	}
}

// Notify registers n to receive incoming signals.
func (h *httpSignaling) Notify(n nethernet.Notifier) (stop func()) {
	h.notifiersMu.Lock()
	i := h.notifyCount
	h.notifyCount++
	h.notifiers[i] = n
	h.notifiersMu.Unlock()

	return sync.OnceFunc(func() {
		h.notifiersMu.Lock()
		delete(h.notifiers, i)
		h.notifiersMu.Unlock()
	})
}

// Context returns context.Background: unlike the Xbox Live signaling websocket, there is no
// long-lived connection here that can fail out from under a dial, so there is nothing to cancel.
func (h *httpSignaling) Context() context.Context {
	return context.Background()
}

// Credentials returns empty credentials - no STUN/TURN. The backend is on loopback, so ICE only
// ever has to pick the host candidate pair and NAT traversal never enters into it.
func (h *httpSignaling) Credentials(context.Context) (*nethernet.Credentials, error) {
	return &nethernet.Credentials{}, nil
}

// NetworkID returns this client's own ID, the /v1/join/{networkID} path segment.
func (h *httpSignaling) NetworkID() string {
	return h.networkID
}

// PongData is a no-op: this side only dials and never serves a ping.
func (h *httpSignaling) PongData([]byte) {}

// notifySignal delivers signal to every registered notifier. Each dialer ignores signals whose
// ConnectionID/NetworkID pair isn't its own, so broadcasting is safe.
func (h *httpSignaling) notifySignal(signal *nethernet.Signal) {
	h.notifiersMu.Lock()
	if h.closed {
		h.notifiersMu.Unlock()
		return
	}
	notifiers := make([]nethernet.Notifier, 0, len(h.notifiers))
	for _, n := range h.notifiers {
		notifiers = append(notifiers, n)
	}
	h.notifiersMu.Unlock()

	for _, n := range notifiers {
		n.NotifySignal(signal)
	}
}

// DialNetherNetBackend establishes a NetherNet/WebRTC connection to a Bedrock server at address,
// which may be a base URL ("http://127.0.0.1:19134") or a bare host:port. The SDP offer/answer
// exchange runs over the backend's HTTP signaling endpoints via httpSignaling.
//
// The caller controls the timeout through ctx; a NetherNet dial that loses its answer will
// otherwise sit in ICE negotiation until something else gives up.
func DialNetherNetBackend(ctx context.Context, address string, identity *nethernet.Identity, log *slog.Logger) (*nethernet.Conn, error) {
	normalized, err := NormalizeNetherNetAddress(address)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	dialer := nethernet.Dialer{
		// A nil Identity dials anonymously, which a Bedrock Dedicated Server rejects during
		// negotiation with NetherNet error code 37 (identity verification failed) - confirmed
		// 2026-09-15 against BDS 1.26.50.5. See NewClientIdentity.
		Identity: identity,
		Log:      log.With("src", "backend-dialer"),
		// Mandatory for this transport: the whole SDP exchange has to fit in one HTTP
		// request/response, so every local candidate must already be in the offer. See the
		// httpSignaling doc comment.
		DisableTrickleICE: true,
	}

	// normalized is passed as the dialer's networkID because for this transport the remote
	// peer's "network ID" IS its base URL. This client's own, separate ID lives inside
	// httpSignaling and only ever appears in the request path.
	conn, err := dialer.DialContext(ctx, normalized, newHTTPSignaling(log, nil))
	if err != nil {
		return nil, fmt.Errorf("dial nethernet backend %s: %w", normalized, err)
	}
	return conn, nil
}

// NewClientIdentity mints a NetherNet client identity for dialing a Bedrock server.
//
// A NetherNet peer proves who it is with an 'a=identity' SDP attribute holding two things: a JWT
// issued by Minecraft's authorization service whose 'cpk' claim is a public key, and a detached
// JWS over the connection's own DTLS fingerprints signed by the matching private key. Together
// they bind "this Xbox Live account" to "this specific WebRTC connection", so the token cannot be
// lifted and replayed onto someone else's connection.
//
// So the keypair must be generated first and the token requested FOR it - the ordering matters and
// is why this cannot reuse the session's cached MCToken, which is a different token bound to
// nothing. Session.MultiplayerToken POSTs the public key to the authorization service and returns
// the JWT carrying it.
//
// A fresh keypair and token are minted per call rather than cached. That costs one HTTPS round
// trip per backend dial, which is the same thing the vanilla client does (see
// Session.MultiplayerToken's "not reused by the vanilla client" note) and avoids having to track
// the token's expiry against long-lived connections.
func NewClientIdentity(ctx context.Context, s *session.Session) (*nethernet.Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity keypair: %w", err)
	}
	token, err := s.MultiplayerToken(ctx, &key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("request multiplayer token: %w", err)
	}
	domain := s.AuthorizationServiceURI()
	if domain == "" {
		// identityData.Valid() rejects an empty domain outright, so fail here with something
		// that says why rather than letting it surface as a generic negotiation failure.
		return nil, errors.New("session has no authorization service URI to name as the token issuer")
	}
	return &nethernet.Identity{
		PrivateKey: key,
		Token:      token,
		Domain:     domain,
	}, nil
}

// ProbeNetherNetBackend reports whether the backend at address answers the NetherNet capability
// probe. Intended for a one-shot check at startup so a misconfigured backend is caught early.
func ProbeNetherNetBackend(ctx context.Context, address string, log *slog.Logger) error {
	normalized, err := NormalizeNetherNetAddress(address)
	if err != nil {
		return err
	}
	if log == nil {
		log = slog.Default()
	}
	return probeSupported(ctx, http.DefaultClient, normalized, log)
}

// probeSupported sends the GET /v1/join capability ping the real Bedrock client uses to decide
// whether an address speaks NetherNet at all, and reports whether the backend answered it.
//
// The client's own probe walks several candidate URLs (https://host:port, https://host,
// http://host:port, http://host) and falls back to RakNet if none respond. Here the operator has
// already named one exact URL in config, so this just checks that one and surfaces a clear
// startup error instead of letting a misconfigured address turn into an opaque ICE timeout
// several layers down.
//
// Upstream's own Handler answers this with a bare 200 and an empty body, so nothing is parsed out
// of the response - any 2xx counts as "this speaks NetherNet".
func probeSupported(ctx context.Context, client *http.Client, baseURL string, log *slog.Logger) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse backend url %q: %w", baseURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.JoinPath("/v1/join").String(), nil)
	if err != nil {
		return fmt.Errorf("make capability request: %w", err)
	}
	req.Header.Set("User-Agent", "libhttpclient/1.0.0.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSDPBodySize))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend at %s did not accept the NetherNet capability probe (GET /v1/join returned %s) - "+
			"check that it is a Bedrock server running with transport=nethernet", baseURL, resp.Status)
	}
	log.Info("backend speaks nethernet", "url", baseURL, "status", resp.Status)
	return nil
}
