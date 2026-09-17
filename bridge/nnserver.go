package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
)

// This file is the serve-side counterpart of nnendpoint.go: where that one DIALS a NetherNet
// server over HTTP signaling, this one IS a NetherNet server over HTTP signaling, so a player can
// type this machine's address into the Bedrock server list and connect.
//
// It is a port of upstream's github.com/df-mc/go-nethernet/endpoint.Handler, adapted to the
// pinned nethernet.Signaling interface for the reasons set out at the top of nnendpoint.go
// (upstream's Notify takes a Notifier; the pin takes a channel).
//
// How a direct-IP join reaches this, per upstream's own handler documentation: on an address
// join, the Bedrock client sends GET /v1/join to each candidate URL in turn - https://host:port,
// https://host, http://host:port, http://host - and connects over NetherNet to the first that
// answers, falling back to RakNet only if none do. So serving this on the address players type is
// what makes an IP join arrive here rather than at a RakNet listener.
//
// Two endpoints:
//
//   - GET  /v1/join              - server-list entry (name, player count, version...). Real BDS
//     answers this with JSON; upstream's own Handler answers a bare 200, which is enough to
//     connect but leaves the server list looking dead. See motdFunc.
//   - POST /v1/join/{networkID}  - the SDP offer/answer exchange. One HTTP round trip carries the
//     whole negotiation, which is why trickle ICE must be disabled on the listener.

// negotiationTimeout bounds how long a POST /v1/join waits for the listener to produce an SDP
// answer before giving up on that client. Matches upstream's default.
const negotiationTimeout = 15 * time.Second

// motdFunc returns the JSON body served for GET /v1/join, i.e. what the player sees for this
// entry in their server list.
type motdFunc func(ctx context.Context) []byte

// httpSignalingServer implements the pinned nethernet.Signaling interface on top of the HTTP
// endpoints a Bedrock client probes, and is also the http.Handler serving them.
//
// The data flow for one join is worth stating plainly, because the Signaling interface reads
// backwards for a server:
//
//  1. Client POSTs an SDP offer. handleOffer parks a reply channel and pushes the offer into the
//     channel the nethernet.Listener registered through Notify.
//  2. The Listener negotiates and calls Signal with the SDP answer - Signal is how the answer
//     comes back IN, not how anything is sent out.
//  3. Signal routes that answer to the parked channel, and handleOffer writes it as the HTTP
//     response body.
type httpSignalingServer struct {
	// networkID identifies this listener locally. It is never sent to clients, but the pinned
	// Listener parses it as a uint64, so it must be decimal.
	networkID string

	log  *slog.Logger
	motd motdFunc

	mux *http.ServeMux

	// pending maps an in-flight negotiation to the channel handleOffer is blocked on.
	pending   map[connectionKey]chan *nethernet.Signal
	pendingMu sync.RWMutex

	// notifier is the channel the single registered Listener reads offers from.
	notifier   chan<- *nethernet.Signal
	notifierMu sync.RWMutex
}

// connectionKey uniquely identifies one in-progress negotiation.
type connectionKey struct {
	networkID    string
	connectionID uint64
}

func (k connectionKey) String() string {
	return k.networkID + "/" + strconv.FormatUint(k.connectionID, 10)
}

func newHTTPSignalingServer(log *slog.Logger, motd motdFunc) *httpSignalingServer {
	if log == nil {
		log = slog.Default()
	}
	s := &httpSignalingServer{
		// Masked to 63 bits, matching the convention used for the Friends-tab listener's
		// NetherNetId in listener.go - rand.Uint64 alone can exceed int64 max, which no
		// reference implementation emits.
		networkID: strconv.FormatUint(mrand.Uint64()&^(1<<63), 10),
		log:       log,
		motd:      motd,
		mux:       http.NewServeMux(),
		pending:   make(map[connectionKey]chan *nethernet.Signal),
	}
	s.mux.HandleFunc("GET /v1/join", s.handleServerList)
	s.mux.HandleFunc("POST /v1/join/{networkID}", s.handleOffer)
	return s
}

func (s *httpSignalingServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mux.ServeHTTP(w, req)
}

// handleServerList answers the client's server-list probe.
func (s *httpSignalingServer) handleServerList(w http.ResponseWriter, req *http.Request) {
	body := s.motd(req.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleOffer handles POST /v1/join/{networkID}, carrying one full SDP exchange.
func (s *httpSignalingServer) handleOffer(w http.ResponseWriter, req *http.Request) {
	req.Close = true // one negotiation per TCP connection; do not keep-alive

	networkID := req.PathValue("networkID")
	if networkID == "" {
		writeText(w, http.StatusBadRequest, "Expected /v1/join/{networkID}")
		return
	}
	// The client's own ID, which Bedrock sends as a decimal uint64. Rejecting anything else
	// mirrors what real BDS does - and is the same check that rejected our own hex IDs when we
	// were on the dialing side of this exchange.
	if _, err := strconv.ParseUint(networkID, 10, 64); err != nil {
		writeText(w, http.StatusBadRequest, "Network ID must be uint64")
		return
	}
	log := s.log.With("networkID", networkID, "remote", req.RemoteAddr)

	req.Body = http.MaxBytesReader(w, req.Body, maxSDPBodySize)
	offer, err := io.ReadAll(req.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			log.Error("sdp offer too large", "limit", maxBytesError.Limit)
			writeText(w, http.StatusRequestEntityTooLarge, "SDP offer is too large")
			return
		}
		log.Error("reading offer body", "err", err)
		writeText(w, http.StatusBadRequest, "Missing SDP offer in request body")
		return
	}
	if len(offer) == 0 {
		writeText(w, http.StatusBadRequest, "Missing SDP offer in request body")
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), negotiationTimeout)
	defer cancel()

	log.Debug("direct-ip join: negotiating", "offerLen", len(offer))
	signal, err := s.negotiate(ctx, networkID, string(offer))
	if err != nil {
		log.Error("direct-ip join: negotiation failed", "err", err)
		switch {
		case errors.Is(err, errNoListener):
			writeText(w, http.StatusServiceUnavailable, "Service unavailable")
		case errors.Is(err, context.DeadlineExceeded):
			writeText(w, http.StatusBadGateway, "Timed out waiting for answer")
		default:
			writeText(w, http.StatusInternalServerError, "An error has occurred while handling this request")
		}
		return
	}

	switch signal.Type {
	case nethernet.SignalTypeAnswer:
		log.Debug("direct-ip join: answered")
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(signal.Data))
	case nethernet.SignalTypeError:
		// The error code goes back as a plain body, which is what the dialing side parses out
		// of a 200 as an in-band failure (see httpSignaling.Signal).
		log.Error("direct-ip join: listener rejected", "code", signal.Data)
		writeText(w, http.StatusBadRequest, "Negotiation failed with error code: "+signal.Data)
	default:
		log.Error("direct-ip join: unexpected signal type", "type", signal.Type)
		writeText(w, http.StatusInternalServerError, "An error has occurred while handling this request")
	}
}

// errNoListener reports that no nethernet.Listener is currently registered to negotiate offers.
var errNoListener = errors.New("no listener registered")

// negotiate hands one offer to the registered Listener and waits for its answer.
func (s *httpSignalingServer) negotiate(ctx context.Context, networkID, offer string) (*nethernet.Signal, error) {
	s.notifierMu.RLock()
	notifier := s.notifier
	s.notifierMu.RUnlock()
	if notifier == nil {
		return nil, errNoListener
	}

	signal := &nethernet.Signal{
		Type:         nethernet.SignalTypeOffer,
		ConnectionID: mrand.Uint64(),
		Data:         offer,
		NetworkID:    networkID,
	}
	key := connectionKey{networkID: signal.NetworkID, connectionID: signal.ConnectionID}

	ch := make(chan *nethernet.Signal, 1)
	s.pendingMu.Lock()
	s.pending[key] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	}()

	select {
	case notifier <- signal:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		// Tell the Listener to abandon the half-negotiated connection rather than leaving it
		// holding transports for a client that is no longer waiting on the other end.
		select {
		case notifier <- &nethernet.Signal{
			Type:         nethernet.SignalTypeError,
			ConnectionID: signal.ConnectionID,
			Data:         strconv.FormatUint(nethernet.ErrorCodeNegotiationTimeoutWaitingForResponse, 10),
			NetworkID:    signal.NetworkID,
		}:
		default:
		}
		return nil, ctx.Err()
	}
}

// Signal receives a signal FROM the registered Listener - normally the SDP answer for a client
// currently blocked in handleOffer - and routes it to that request.
func (s *httpSignalingServer) Signal(ctx context.Context, signal *nethernet.Signal) error {
	if signal.Type == nethernet.SignalTypeCandidate {
		// Reachable only if the listener was built without DisableTrickleICE; there is no second
		// round trip on which to deliver additional candidates.
		return errors.New("trickle ICE is not supported over HTTP signaling")
	}

	key := connectionKey{networkID: signal.NetworkID, connectionID: signal.ConnectionID}
	s.pendingMu.RLock()
	ch, ok := s.pending[key]
	s.pendingMu.RUnlock()
	if !ok {
		return fmt.Errorf("unexpected connection ID: %s", key)
	}

	select {
	case ch <- signal:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify registers the Listener's channel. Only one Listener may be registered: each offer
// produces exactly one answer, and the HTTP response has nowhere to put a second one.
func (s *httpSignalingServer) Notify(signals chan<- *nethernet.Signal) (stop func()) {
	s.notifierMu.Lock()
	if s.notifier != nil {
		s.notifierMu.Unlock()
		panic("bridge: httpSignalingServer.Notify: listener already registered")
	}
	s.notifier = signals
	s.notifierMu.Unlock()

	return sync.OnceFunc(func() {
		s.notifierMu.Lock()
		s.notifier = nil
		s.notifierMu.Unlock()
	})
}

// Context returns context.Background: there is no long-lived signaling connection here whose
// failure should invalidate the listener, unlike the Xbox Live websocket.
func (s *httpSignalingServer) Context() context.Context {
	return context.Background()
}

// Credentials returns empty credentials - no STUN/TURN. Clients reaching a public address
// directly do not need a relay allocation to find us.
func (s *httpSignalingServer) Credentials(context.Context) (*nethernet.Credentials, error) {
	return &nethernet.Credentials{}, nil
}

// NetworkID returns this listener's local identifier.
func (s *httpSignalingServer) NetworkID() string {
	return s.networkID
}

// PongData is a no-op: the server-list data is served over GET /v1/join instead, not through the
// RakNet-style pong payload this interface was shaped around.
func (s *httpSignalingServer) PongData([]byte) {}

func writeText(w http.ResponseWriter, statusCode int, text string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(text))
}
