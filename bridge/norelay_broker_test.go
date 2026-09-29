package bridge

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
)

// fakeSignaling stands in for the Xbox Live signaling transport: it records what the broker
// signals back to the client and lets a test end the transport.
type fakeSignaling struct {
	id     string
	sent   chan *nethernet.Signal
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// newFakeSignaling returns a transport whose own ID is decimal, like the websocket transport's.
func newFakeSignaling() *fakeSignaling {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &fakeSignaling{id: "4242", sent: make(chan *nethernet.Signal, 16), ctx: ctx, cancel: cancel}
}

func (f *fakeSignaling) Signal(_ context.Context, s *nethernet.Signal) error { f.sent <- s; return nil }
func (f *fakeSignaling) Notify(nethernet.Notifier) func()                    { return func() {} }
func (f *fakeSignaling) Context() context.Context                            { return f.ctx }
func (f *fakeSignaling) NetworkID() string                                   { return f.id }
func (f *fakeSignaling) PongData([]byte)                                     {}
func (f *fakeSignaling) Credentials(context.Context) (*nethernet.Credentials, error) {
	return nil, nil
}

// next waits for the broker's next signal to the client.
func (f *fakeSignaling) next(t *testing.T) *nethernet.Signal {
	t.Helper()
	select {
	case s := <-f.sent:
		return s
	case <-time.After(maxGather + 3*time.Second):
		t.Fatal("broker signaled nothing back to the client")
		return nil
	}
}

// expectSilence fails if the broker signals anything within d.
func (f *fakeSignaling) expectSilence(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case s := <-f.sent:
		t.Fatalf("unexpected signal to client: %s", s)
	case <-time.After(d):
	}
}

// fakeBackend is the backend's HTTP signaling endpoint. It records every offer it is sent.
type fakeBackend struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
	posts  atomic.Int32
}

func newFakeBackend(t *testing.T, status int, reply string) *fakeBackend {
	b := &fakeBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.paths = append(b.paths, r.URL.Path)
		b.bodies = append(b.bodies, string(body))
		b.mu.Unlock()
		b.posts.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *fakeBackend) last(t *testing.T) (path, body string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.paths) == 0 {
		t.Fatal("backend was never sent an offer")
	}
	return b.paths[len(b.paths)-1], b.bodies[len(b.bodies)-1]
}

func newTestBroker(sig nethernet.Signaling, backend *fakeBackend) *signalBroker {
	return &signalBroker{
		name:    "test",
		log:     slog.New(slog.DiscardHandler),
		sig:     sig,
		backend: backend.URL,
		client:  backend.Client(),
		pending: make(map[connectionKey]*brokeredOffer),
	}
}

const (
	clientNetworkID = "client-network-1"
	clientConnID    = uint64(7)
)

func clientSignal(typ, data string) *nethernet.Signal {
	return &nethernet.Signal{Type: typ, ConnectionID: clientConnID, Data: data, NetworkID: clientNetworkID}
}

// TestBrokerForwardsTrickledOffer is the end-to-end path of a Friends-tab join: a trickling
// client's offer and candidates go in, one bundled offer reaches the backend, and the backend's
// answer comes back to the client addressed to the right connection.
func TestBrokerForwardsTrickledOffer(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer) // stands in for an answer with 2 candidates
	broker := newTestBroker(sig, backend)

	trickled, candidates := stripCandidates(canonicalOffer)
	if !broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled)) {
		t.Fatal("offer rejected")
	}
	for _, c := range candidates {
		if !broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, c)) {
			t.Fatal("candidate rejected")
		}
	}

	answer := sig.next(t)
	if answer.Type != nethernet.SignalTypeAnswer {
		t.Fatalf("first signal to client was %s, want an answer", answer.Type)
	}
	// Echoed exactly, or the client drops the answer and the join shows up as a timeout.
	if answer.ConnectionID != clientConnID || answer.NetworkID != clientNetworkID {
		t.Errorf("answer addressed to %d@%s, want %d@%s",
			answer.ConnectionID, answer.NetworkID, clientConnID, clientNetworkID)
	}
	if answer.Data != canonicalOffer {
		t.Errorf("answer body was not passed through unchanged")
	}

	path, body := backend.last(t)
	assertJoinPath(t, path)
	if body != canonicalOffer {
		t.Errorf("backend got a different offer than the canonical one\n got: %q\nwant: %q", body, canonicalOffer)
	}
	if n := backend.posts.Load(); n != 1 {
		t.Errorf("backend was sent %d offers, want exactly 1 bundle", n)
	}

	// Then the backend's candidates are trickled to the client as well.
	for _, want := range answerCandidates(canonicalOffer) {
		got := sig.next(t)
		if got.Type != nethernet.SignalTypeCandidate || got.Data != want {
			t.Errorf("trickled %s %q, want CANDIDATEADD %q", got.Type, got.Data, want)
		}
		if got.ConnectionID != clientConnID || got.NetworkID != clientNetworkID {
			t.Errorf("candidate addressed to %d@%s", got.ConnectionID, got.NetworkID)
		}
	}
}

// assertJoinPath checks the /v1/join/{networkID} segment is one BDS will take: a decimal uint64
// (anything else is a 400), and never the client's ID - the direction mix-up nnendpoint.go
// documents is what produced BDS's 400 the first time round.
func assertJoinPath(t *testing.T, path string) {
	t.Helper()
	id, ok := strings.CutPrefix(path, "/v1/join/")
	if !ok {
		t.Fatalf("offer posted to %s, want /v1/join/{networkID}", path)
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		t.Errorf("join path ID %q is not a decimal uint64, which BDS rejects: %v", id, err)
	}
	if id == clientNetworkID {
		t.Errorf("join path carries the client's network ID %q instead of our own", id)
	}
}

// TestBrokerJoinPathIgnoresSignalingID: the messaging transport's own ID is a UUID (the session's
// PmsgId). Posting that as the join path is a 400 from BDS on every join, so the path must come
// from somewhere other than the signaling transport.
func TestBrokerJoinPathIgnoresSignalingID(t *testing.T) {
	sig := newFakeSignaling()
	sig.id = "3f1c9e2a-7b4d-4e8f-9a01-2c3d4e5f6a7b" // what messaging.Conn.NetworkID returns
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, canonicalOffer))
	sig.next(t)
	path, _ := backend.last(t)
	assertJoinPath(t, path)
	if strings.Contains(path, sig.id) {
		t.Errorf("join path %s carries the messaging UUID", path)
	}
}

// TestBrokerJoinPathIsPerOffer: two joins in flight must never share a path ID.
func TestBrokerJoinPathIsPerOffer(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	for connID := range uint64(2) {
		broker.NotifySignal(&nethernet.Signal{
			Type: nethernet.SignalTypeOffer, ConnectionID: connID, Data: canonicalOffer, NetworkID: clientNetworkID,
		})
	}
	seen := 0
	for seen < 2 {
		if sig.next(t).Type == nethernet.SignalTypeAnswer {
			seen++
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.paths) != 2 || backend.paths[0] == backend.paths[1] {
		t.Errorf("concurrent offers used join paths %q, want two distinct ones", backend.paths)
	}
}

// TestBrokerWaitsForCandidates: the offer must not go out the moment it arrives, or a trickling
// client's candidates would miss the only request the backend will ever see.
func TestBrokerWaitsForCandidates(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	trickled, candidates := stripCandidates(canonicalOffer)
	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
	// Keep trickling inside the quiet window; each one must push the forward back.
	for _, c := range candidates {
		time.Sleep(candidateQuiet / 2)
		if n := backend.posts.Load(); n != 0 {
			t.Fatalf("offer forwarded while candidates were still arriving")
		}
		broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, c))
	}
	sig.next(t)
	if _, body := backend.last(t); body != canonicalOffer {
		t.Errorf("a candidate was lost while waiting:\n%q", body)
	}
}

// TestBrokerForwardsNonTrickleOfferImmediately: an offer that already has its candidates gains
// nothing from waiting, so it should not pay candidateQuiet at all.
func TestBrokerForwardsNonTrickleOfferImmediately(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	start := time.Now()
	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, canonicalOffer))
	sig.next(t)
	if elapsed := time.Since(start); elapsed >= candidateQuiet {
		t.Errorf("non-trickle offer waited %v, want no candidate wait", elapsed)
	}
	if _, body := backend.last(t); body != canonicalOffer {
		t.Errorf("non-trickle offer was altered")
	}
}

// TestBrokerCapsGatherTime: a client that never stops trickling still gets forwarded by maxGather.
func TestBrokerCapsGatherTime(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	trickled, candidates := stripCandidates(canonicalOffer)
	start := time.Now()
	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(candidateQuiet / 3):
				broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, candidates[0]))
			}
		}
	}()
	sig.next(t)
	if elapsed := time.Since(start); elapsed > maxGather+time.Second {
		t.Errorf("forward took %v under a constant trickle, want about maxGather (%v)", elapsed, maxGather)
	}
}

// TestBrokerReportsBackendFailure: without an error signal the player just sits on "loading".
func TestBrokerReportsBackendFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"http error":        {http.StatusServiceUnavailable, "Service unavailable"},
		"in-band rejection": {http.StatusOK, "37"}, // BDS's missing-identity code, sent with a 200
	} {
		t.Run(name, func(t *testing.T) {
			sig := newFakeSignaling()
			broker := newTestBroker(sig, newFakeBackend(t, tc.status, tc.body))
			broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, canonicalOffer))

			got := sig.next(t)
			if got.Type != nethernet.SignalTypeError {
				t.Fatalf("client got %s, want CONNECTERROR", got.Type)
			}
			if want := strconv.Itoa(nethernet.ErrorCodeFailedToCreateAnswer); got.Data != want {
				t.Errorf("error code %q, want %q", got.Data, want)
			}
			if got.ConnectionID != clientConnID || got.NetworkID != clientNetworkID {
				t.Errorf("error addressed to %d@%s", got.ConnectionID, got.NetworkID)
			}
		})
	}
}

// TestBrokerDropsAbandonedOffer: a client that gives up must not leave a negotiation behind that
// later reaches the backend.
func TestBrokerDropsAbandonedOffer(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	trickled, _ := stripCandidates(canonicalOffer)
	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
	broker.NotifySignal(clientSignal(nethernet.SignalTypeError, strconv.Itoa(nethernet.ErrorCodeNegotiationTimeout)))

	sig.expectSilence(t, 3*candidateQuiet)
	if n := backend.posts.Load(); n != 0 {
		t.Errorf("abandoned offer still reached the backend %d time(s)", n)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.pending) != 0 {
		t.Errorf("abandoned offer left %d pending negotiation(s)", len(broker.pending))
	}
}

// TestBrokerIgnoresLateCandidateAndDuplicateOffer: neither may cause a second request.
func TestBrokerIgnoresLateCandidateAndDuplicateOffer(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	trickled, candidates := stripCandidates(canonicalOffer)
	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
	if broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled)) {
		t.Error("duplicate offer for an in-progress connection was accepted")
	}
	sig.next(t) // the answer
	for range answerCandidates(canonicalOffer) {
		sig.next(t)
	}
	broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, candidates[0]))
	sig.expectSilence(t, 3*candidateQuiet)
	if n := backend.posts.Load(); n != 1 {
		t.Errorf("backend was sent %d offers, want 1", n)
	}
}

// TestBrokerIgnoresAnswers: only a client's half of the exchange ever reaches a host.
func TestBrokerIgnoresAnswers(t *testing.T) {
	broker := newTestBroker(newFakeSignaling(), newFakeBackend(t, http.StatusOK, canonicalOffer))
	if broker.NotifySignal(clientSignal(nethernet.SignalTypeAnswer, canonicalOffer)) {
		t.Error("an answer from a client was accepted")
	}
}

// TestBrokerRunStopsWithSignaling: a dead signaling transport must end Run so runSession rebuilds.
func TestBrokerRunStopsWithSignaling(t *testing.T) {
	sig := newFakeSignaling()
	broker := newTestBroker(sig, newFakeBackend(t, http.StatusOK, canonicalOffer))
	done := make(chan error, 1)
	go func() { done <- broker.Run(context.Background()) }()
	sig.cancel(io.ErrUnexpectedEOF)
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil after signaling died")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after signaling died")
	}
}
