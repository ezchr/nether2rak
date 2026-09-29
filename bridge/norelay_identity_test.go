package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/go-jose/go-jose/v4"
)

// testPlayer is a joining client: its identity key, and the token the fake verifier accepts for it.
type testPlayer struct {
	key   *ecdsa.PrivateKey
	token string
	xuid  string
	name  string
}

func newTestPlayer(t *testing.T, xuid, name string) testPlayer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Three dot-separated parts is all parseOfferIdentity checks; the fake verifier does the rest.
	return testPlayer{key: key, token: "hdr.token-for-" + xuid + ".sig", xuid: xuid, name: name}
}

// fakeVerifier stands in for Minecraft's authorization service: it knows these players' tokens.
func fakeVerifier(players ...testPlayer) func(context.Context, string) (PlayerToken, error) {
	return func(_ context.Context, token string) (PlayerToken, error) {
		for _, p := range players {
			if p.token == token {
				return PlayerToken{XUID: p.xuid, Name: p.name, PublicKey: &p.key.PublicKey}, nil
			}
		}
		return PlayerToken{}, errors.New("token not issued by the authorization service")
	}
}

// offerFrom returns canonicalOffer carrying an identity attribute the way a Bedrock client writes
// it: the fingerprint assertion signed by signer over the offer's own fingerprint, with p's token.
func offerFrom(t *testing.T, p testPlayer, signer *ecdsa.PrivateKey) string {
	t.Helper()
	payload, err := fingerprintPayload(canonicalOffer)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES384, Key: signer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	detached, err := jws.DetachedCompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	// go-nethernet's identityAssertion marshals itself as a JSON string holding the object.
	inner, _ := json.Marshal(map[string]string{"fingerprints": detached, "token": p.token})
	outer, _ := json.Marshal(map[string]any{
		"assertion": string(inner),
		"idp":       map[string]string{"domain": "https://authorization.franchise.minecraft-services.net", "protocol": "default"},
	})
	return withSessionAttribute(canonicalOffer, "a=identity:"+base64.StdEncoding.EncodeToString(outer))
}

// withSessionAttribute inserts a session-level attribute line just above m=.
func withSessionAttribute(sdp, line string) string {
	i := strings.Index(sdp, "m=")
	return sdp[:i] + line + "\r\n" + sdp[i:]
}

func TestFingerprintPayloadMatchesGoNethernet(t *testing.T) {
	got, err := fingerprintPayload(canonicalOffer)
	if err != nil {
		t.Fatal(err)
	}
	// Byte for byte what go-nethernet's generateFingerprints builds, which is what gets signed.
	if want := `{"fingerprint":[{"algorithm":"sha-256","digest":"AA:BB"}]}`; string(got) != want {
		t.Errorf("payload %s, want %s", got, want)
	}
}

func TestSDPAttributePrefersMedia(t *testing.T) {
	sdp := withSessionAttribute(canonicalOffer, "a=fingerprint:sha-256 SESSION")
	if v, _ := sdpAttribute(sdp, "fingerprint", true); v != "sha-256 AA:BB" {
		t.Errorf("preferMedia returned %q, want the media-level fingerprint", v)
	}
	if v, _ := sdpAttribute(sdp, "fingerprint", false); v != "sha-256 SESSION" {
		t.Errorf("first match returned %q, want the session-level one", v)
	}
	if _, ok := sdpAttribute(canonicalOffer, "identity", false); ok {
		t.Error("found an attribute that is not there")
	}
}

func TestVerifyOfferIdentity(t *testing.T) {
	alice := newTestPlayer(t, "2535400000000001", "Alice")
	mallory := newTestPlayer(t, "2535400000000666", "Mallory")
	verify := fakeVerifier(alice, mallory)
	ctx := context.Background()

	t.Run("genuine", func(t *testing.T) {
		got, err := verifyOfferIdentity(ctx, offerFrom(t, alice, alice.key), verify)
		if err != nil {
			t.Fatal(err)
		}
		if got.XUID != alice.xuid || got.Name != alice.name {
			t.Errorf("identified as %s/%s, want %s/%s", got.XUID, got.Name, alice.xuid, alice.name)
		}
	})
	t.Run("anonymous", func(t *testing.T) {
		if _, err := verifyOfferIdentity(ctx, canonicalOffer, verify); !errors.Is(err, errNoIdentity) {
			t.Errorf("got %v, want errNoIdentity", err)
		}
	})
	t.Run("token not issued", func(t *testing.T) {
		// Claims Alice and is signed consistently with its own key, but no issuer made the token.
		forged := newTestPlayer(t, alice.xuid, "Alice")
		forged.token = "hdr.forged-claiming-alice.sig"
		if _, err := verifyOfferIdentity(ctx, offerFrom(t, forged, forged.key), verify); err == nil {
			t.Error("a token the issuer never issued was accepted")
		}
	})
	t.Run("replayed token", func(t *testing.T) {
		// Mallory copies Alice's genuine token but can only sign with her own key.
		if _, err := verifyOfferIdentity(ctx, offerFrom(t, alice, mallory.key), verify); err == nil {
			t.Error("Alice's token was accepted on a connection Mallory signed")
		}
	})
	t.Run("fingerprint swapped after signing", func(t *testing.T) {
		offer := strings.Replace(offerFrom(t, alice, alice.key), "sha-256 AA:BB", "sha-256 CC:DD", 1)
		if _, err := verifyOfferIdentity(ctx, offer, verify); err == nil {
			t.Error("an assertion over different fingerprints was accepted")
		}
	})
	t.Run("candidates added later", func(t *testing.T) {
		// What the broker does to every trickled offer must not break the assertion.
		trickled, candidates := stripCandidates(offerFrom(t, alice, alice.key))
		if _, err := verifyOfferIdentity(ctx, inlineCandidates(trickled, candidates), verify); err != nil {
			t.Errorf("inlining candidates broke the identity: %v", err)
		}
	})
}

func TestParseOfferIdentityAcceptsObjectAssertion(t *testing.T) {
	inner := map[string]string{"fingerprints": "a.b.c", "token": "d.e.f"}
	outer, _ := json.Marshal(map[string]any{"assertion": inner, "idp": map[string]string{"protocol": "default"}})
	id, err := parseOfferIdentity(withSessionAttribute(canonicalOffer, "a=identity:"+base64.StdEncoding.EncodeToString(outer)))
	if err != nil {
		t.Fatal(err)
	}
	if id.token != "d.e.f" || id.fingerprints != "a.b.c" {
		t.Errorf("parsed %+v", id)
	}
}

func TestParseCPK(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	b64, _ := json.Marshal(base64.StdEncoding.EncodeToString(der)) // as the authorization service writes it
	jwk, _ := json.Marshal(jose.JSONWebKey{Key: &key.PublicKey})
	for name, raw := range map[string][]byte{"base64 DER": b64, "JWK": jwk} {
		got, err := parseCPK(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !got.Equal(&key.PublicKey) {
			t.Errorf("%s: decoded a different key", name)
		}
	}
	if _, err := parseCPK([]byte(`"not base64!"`)); err == nil {
		t.Error("garbage cpk accepted")
	}
}

// joinRecorder collects OnJoin calls.
type joinRecorder struct {
	mu    sync.Mutex
	joins []string
}

func (r *joinRecorder) record(xuid, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.joins = append(r.joins, xuid+"/"+name)
}

func (r *joinRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.joins...)
}

// TestBrokerAllowlistAndActivity: allowed_xuids and friend activity, carried over from HandleConn.
func TestBrokerAllowlistAndActivity(t *testing.T) {
	alice := newTestPlayer(t, "2535400000000001", "Alice")
	bob := newTestPlayer(t, "2535400000000002", "Bob")
	unknown := newTestPlayer(t, "2535400000000003", "Nobody") // token the issuer never issued
	onlyAlice := func(x string) bool { return x == alice.xuid }

	for _, tc := range []struct {
		name       string
		offer      func(t *testing.T) string
		allow      func(string) bool
		wantJoin   bool   // reached the backend and was answered
		wantRecord string // friend activity written, "" for none
	}{
		{"allowed player", func(t *testing.T) string { return offerFrom(t, alice, alice.key) }, onlyAlice, true, alice.xuid + "/Alice"},
		{"player not on the list", func(t *testing.T) string { return offerFrom(t, bob, bob.key) }, onlyAlice, false, ""},
		{"anonymous, list set", func(*testing.T) string { return canonicalOffer }, onlyAlice, false, ""},
		{"unverifiable, list set", func(t *testing.T) string { return offerFrom(t, unknown, unknown.key) }, onlyAlice, false, ""},
		{"replayed token, list set", func(t *testing.T) string { return offerFrom(t, alice, bob.key) }, onlyAlice, false, ""},
		{"no list, verified", func(t *testing.T) string { return offerFrom(t, bob, bob.key) }, nil, true, bob.xuid + "/Bob"},
		// Without a list an unverifiable player is still let through - the backend's own login
		// check decides - but is not written to friend activity.
		{"no list, anonymous", func(*testing.T) string { return canonicalOffer }, nil, true, ""},
		{"no list, unverifiable", func(t *testing.T) string { return offerFrom(t, unknown, unknown.key) }, nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := newFakeSignaling()
			backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
			broker := newTestBroker(sig, backend)
			var joins joinRecorder
			broker.verify, broker.allow, broker.onJoin = fakeVerifier(alice, bob), tc.allow, joins.record

			broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, tc.offer(t)))
			got := sig.next(t)
			if tc.wantJoin {
				if got.Type != nethernet.SignalTypeAnswer {
					t.Fatalf("client got %s %q, want an answer", got.Type, got.Data)
				}
			} else {
				if got.Type != nethernet.SignalTypeError || got.Data != strconv.Itoa(nethernet.ErrorCodeIdentityNotAllowed) {
					t.Fatalf("client got %s %q, want CONNECTERROR %d", got.Type, got.Data, nethernet.ErrorCodeIdentityNotAllowed)
				}
				if n := backend.posts.Load(); n != 0 {
					t.Errorf("a refused player still reached the backend (%d requests)", n)
				}
			}
			// Drain the trickled candidates so the forward has finished before checking.
			if tc.wantJoin {
				for range answerCandidates(canonicalOffer) {
					sig.next(t)
				}
			}
			recorded := joins.got()
			switch {
			case tc.wantRecord == "" && len(recorded) != 0:
				t.Errorf("friend activity recorded %q, want nothing", recorded)
			case tc.wantRecord != "" && (len(recorded) != 1 || recorded[0] != tc.wantRecord):
				t.Errorf("friend activity recorded %q, want exactly %q", recorded, tc.wantRecord)
			}
			assertIdle(t, broker)
		})
	}
}

// assertIdle checks the broker holds no negotiation and no in-flight slot.
func assertIdle(t *testing.T, broker *signalBroker) {
	t.Helper()
	// The in-flight slot is released in a deferred call after the last signal; give it a moment.
	for range 50 {
		broker.mu.Lock()
		idle := broker.inFlight == 0 && len(broker.pending) == 0
		broker.mu.Unlock()
		if idle {
			return
		}
		sleepBriefly()
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	t.Errorf("broker not idle: inFlight=%d pending=%d", broker.inFlight, len(broker.pending))
}

func sleepBriefly() { time.Sleep(10 * time.Millisecond) }

func TestIsRelayCandidate(t *testing.T) {
	for candidate, want := range map[string]bool{
		"candidate:2 1 udp 41885439 192.0.2.10 3478 typ relay raddr 198.51.100.20 rport 50000": true,
		"a=candidate:2 1 udp 41885439 192.0.2.10 3478 typ relay raddr 0.0.0.0 rport 0":         true,
		"candidate:0 1 udp 2130706431 203.0.113.7 50000 typ host":                              false,
		"candidate:1 1 udp 1694498815 198.51.100.2 50001 typ srflx raddr 0.0.0.0 rport 0":      false,
		"candidate:3 1 udp 1 203.0.113.9 5 typ prflx":                                          false,
		"relay":                                 false, // the word alone is not a candidate type
		"candidate:4 1 udp 1 203.0.113.4 5 typ": false,
	} {
		if got := isRelayCandidate(candidate); got != want {
			t.Errorf("isRelayCandidate(%q) = %v, want %v", candidate, got, want)
		}
	}
}

func TestStripRelayCandidates(t *testing.T) {
	relay := "a=candidate:2 1 udp 41885439 192.0.2.10 3478 typ relay raddr 0.0.0.0 rport 0\r\n"
	withRelay := strings.Replace(canonicalOffer, "a=ice-ufrag:", relay+"a=ice-ufrag:", 1)
	got, removed := stripRelayCandidates(withRelay)
	if removed != 1 || got != canonicalOffer {
		t.Errorf("removed %d, result differs from the offer without it:\n%q", removed, got)
	}
	if got, removed := stripRelayCandidates(canonicalOffer); removed != 0 || got != canonicalOffer {
		t.Errorf("an offer without relay candidates was changed (%d removed)", removed)
	}
}

// TestBrokerDropsRelayCandidates: neither a trickled nor an inline relay candidate reaches the backend.
func TestBrokerDropsRelayCandidates(t *testing.T) {
	sig := newFakeSignaling()
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)

	const relayInline = "a=candidate:9 1 udp 41885439 192.0.2.11 3478 typ relay raddr 0.0.0.0 rport 0\r\n"
	const relayTrickled = "candidate:8 1 udp 41885439 192.0.2.10 3478 typ relay raddr 0.0.0.0 rport 0"
	trickled, candidates := stripCandidates(canonicalOffer)
	trickled = strings.Replace(trickled, "a=ice-ufrag:", relayInline+"a=ice-ufrag:", 1)

	broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
	broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, relayTrickled))
	for _, c := range candidates {
		broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate, c))
	}
	sig.next(t)
	_, body := backend.last(t)
	if strings.Contains(body, "typ relay") {
		t.Errorf("a relay candidate reached the backend:\n%s", body)
	}
	if body != canonicalOffer {
		t.Errorf("the direct candidates were not all kept:\n got %q\nwant %q", body, canonicalOffer)
	}
}

// TestBrokerInFlightLimit: offers past maxInFlight are refused without reaching the backend, and
// the slots come back once negotiations finish.
func TestBrokerInFlightLimit(t *testing.T) {
	sig := newFakeSignaling()
	sig.sent = make(chan *nethernet.Signal, 4*maxInFlight)
	backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
	broker := newTestBroker(sig, backend)
	trickled, _ := stripCandidates(canonicalOffer) // trickling, so each is held for candidateQuiet

	offer := func(id uint64) bool {
		return broker.NotifySignal(&nethernet.Signal{
			Type: nethernet.SignalTypeOffer, ConnectionID: id, Data: trickled, NetworkID: clientNetworkID,
		})
	}
	for id := range uint64(maxInFlight) {
		if !offer(id) {
			t.Fatalf("offer %d refused below the limit", id)
		}
	}
	if offer(maxInFlight) {
		t.Fatal("an offer over maxInFlight was accepted")
	}
	answers := 0
	for answers < maxInFlight {
		if sig.next(t).Type == nethernet.SignalTypeAnswer {
			answers++
		}
	}
	if n := backend.posts.Load(); n != maxInFlight {
		t.Errorf("backend got %d offers, want %d", n, maxInFlight)
	}
	assertIdle(t, broker)
	if !offer(1000) {
		t.Error("slots were not given back after the negotiations finished")
	}
}

func TestBrokerSizeLimits(t *testing.T) {
	t.Run("oversized offer", func(t *testing.T) {
		broker := newTestBroker(newFakeSignaling(), newFakeBackend(t, http.StatusOK, canonicalOffer))
		huge := canonicalOffer + "a=x:" + strings.Repeat("x", maxOfferBytes) + "\r\n"
		if broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, huge)) {
			t.Error("an offer over maxOfferBytes was accepted")
		}
		assertIdle(t, broker)
	})
	t.Run("candidate flood", func(t *testing.T) {
		sig := newFakeSignaling()
		backend := newFakeBackend(t, http.StatusOK, canonicalOffer)
		broker := newTestBroker(sig, backend)
		trickled, _ := stripCandidates(canonicalOffer)
		broker.NotifySignal(clientSignal(nethernet.SignalTypeOffer, trickled))
		for i := range maxCandidates + 20 {
			broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate,
				fmt.Sprintf("candidate:%d 1 udp 1 203.0.113.%d 5 typ host", i, i%250)))
		}
		broker.NotifySignal(clientSignal(nethernet.SignalTypeCandidate,
			"candidate:999 1 udp 1 203.0.113.1 5 typ host "+strings.Repeat("x", maxCandidateBytes)))
		sig.next(t)
		_, body := backend.last(t)
		if n := strings.Count(body, "a=candidate:"); n != maxCandidates {
			t.Errorf("backend got %d candidates, want the first %d", n, maxCandidates)
		}
	})
}
