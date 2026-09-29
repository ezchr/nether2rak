package bridge

// This file implements backend_transport "nethernet-norelay": the Friends tab without a relay in
// the data path.
//
// In the two relay modes the player's WebRTC connection terminates in this process and a second
// connection is opened to the backend, so every packet is decrypted and re-encrypted in the
// middle. Here only the signaling stays - which it must, because no Bedrock server can hold an
// Xbox Live session or its signaling websocket, having no Xbox Live login of its own. The SDP
// exchange is forwarded to the backend's own HTTP signaling endpoint (the same /v1/join a
// direct-IP join uses), so ICE and DTLS are negotiated directly between the player's client and
// the backend and no game traffic passes through this process at all.
//
// Two properties of go-nethernet make that safe, and both were read out of the library rather
// than assumed:
//
//  1. A client's identity assertion signs ONLY the DTLS fingerprints - identityData.verify
//     builds its payload from generateFingerprints(desc.dtls.Fingerprints) and nothing else. ICE
//     candidates are not covered, so appending candidates to an offer leaves the assertion
//     intact and the backend verifies the player's own genuine, unmodified token. That is what
//     lets a native BDS behind this mode run online-mode=true instead of trusting a relay.
//  2. Candidates are parsed from the session-level and media-level attributes alike -
//     parseDescription iterates append(d.Attributes, m.Attributes...) - so inlining them is a
//     plain textual append to the end of the SDP, with no need to reserialise anything.
//
// The two transports disagree about trickle ICE, and that disagreement is the whole difficulty.
// Xbox Live signaling trickles: CONNECTREQUEST first, then CANDIDATEADD messages, with no
// end-of-candidates marker. The backend's HTTP signaling is a single request/response that has
// to carry every candidate at once (see nnendpoint.go's file comment). So an offer is held just
// long enough for the client's candidates to arrive and then sent as one bundle, which costs a
// fraction of a second at connect time and saves the relay hop for the rest of the session.
//
// ConnectionID never crosses between the legs. The backend's HTTP endpoint mints its own per
// request - nnserver.go's negotiate does exactly that, mirroring real BDS - so the client's ID
// is simply echoed back on the answer.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
)

const (
	// candidateQuiet is how long to wait after the last CANDIDATEADD before treating the client
	// as done gathering. Quiet time is the only signal available: this transport has no
	// end-of-candidates message, and a client that is still gathering sends nothing to say so.
	candidateQuiet = 150 * time.Millisecond

	// maxGather caps how long any one offer is held, however many candidates keep arriving.
	// Whatever has been collected when it expires is what the backend is given - a join that
	// takes longer than this to even start negotiating is worse than one with fewer candidate
	// pairs to try.
	maxGather = 2 * time.Second

	// The limits below bound what anyone who can reach this world's signaling can make the broker
	// do. Every accepted offer becomes a request to the backend, so without them one peer could
	// turn a stream of offers - or one offer with a flood of candidates - into load on the server.
	// Each is far above what a real client sends: an offer is a few KiB (most of it the identity
	// token) with a handful of candidates, each well under 200 bytes.

	// maxInFlight caps offers being gathered or forwarded at once, per signaling transport.
	maxInFlight = 32
	// maxOfferBytes caps the size of one SDP offer.
	maxOfferBytes = 64 << 10
	// maxCandidates caps the candidates kept for one offer; further ones are ignored.
	maxCandidates = 32
	// maxCandidateBytes caps the size of one trickled candidate.
	maxCandidateBytes = 1024
)

// signalBroker forwards one signaling transport's offers to the backend's HTTP signaling
// endpoint instead of answering them locally, leaving the WebRTC connection to form directly
// between the client and the backend. It implements nethernet.Notifier.
type signalBroker struct {
	name    string
	log     *slog.Logger
	sig     nethernet.Signaling
	backend string
	client  *http.Client

	// verify, allow and onJoin carry over what HandleConn did with a relayed login: allow is
	// allowed_xuids and onJoin records friend activity. Both need a trustworthy XUID, which verify
	// provides from the offer's identity - see norelay_identity.go.
	verify func(ctx context.Context, token string) (PlayerToken, error)
	allow  func(xuid string) bool
	onJoin func(xuid, displayName string)

	mu      sync.Mutex
	pending map[connectionKey]*brokeredOffer
	// inFlight counts offers accepted and not yet finished, whether still gathering or already
	// being forwarded. Whoever marks an offer done releases its slot, so it is released once.
	inFlight int
	// dropped counts offers refused for exceeding a limit since droppedLogged, so a flood is
	// reported as one line every dropLogInterval rather than one line per offer.
	dropped       int
	droppedLogged time.Time
}

// dropLogInterval is how often refused offers are summarised in the log.
const dropLogInterval = 10 * time.Second

// brokeredOffer is one offer being held while the client's ICE candidates trickle in.
type brokeredOffer struct {
	offer      string
	candidates []string

	// deadline is the hard cutoff from maxGather; timer fires at whichever of it and the
	// candidateQuiet debounce comes first.
	deadline time.Time
	timer    *time.Timer

	// done marks the offer as already forwarded or abandoned, so a candidate racing the timer
	// cannot resurrect it.
	done bool
}

// newSignalBroker prepares a broker for one signaling transport. It fails only if the configured
// backend address is unusable, which is worth catching before the world is advertised.
func newSignalBroker(name string, sig nethernet.Signaling, cfg Config, log *slog.Logger) (*signalBroker, error) {
	backend, err := NormalizeNetherNetAddress(cfg.NetherNetBackendAddress)
	if err != nil {
		return nil, fmt.Errorf("normalise backend address: %w", err)
	}
	return &signalBroker{
		name:    name,
		log:     log.With("transport", name),
		sig:     sig,
		backend: backend,
		// Each negotiation is one short request; the timeout matches what the backend's own
		// signaling server allows itself for one exchange.
		client:  &http.Client{Timeout: negotiationTimeout},
		verify:  cfg.VerifyPlayerToken,
		allow:   cfg.AllowXUID,
		onJoin:  cfg.OnJoin,
		pending: make(map[connectionKey]*brokeredOffer),
	}, nil
}

// Run subscribes to the signaling transport and blocks until ctx is cancelled or the transport
// fails. Unlike the relay modes there is no accept loop: every connection is negotiated inside
// NotifySignal and then belongs entirely to the client and the backend.
func (b *signalBroker) Run(ctx context.Context) error {
	stop := b.sig.Notify(b)
	defer stop()

	b.log.Info("signaling broker ready - players connect straight to the backend", "backend", b.backend)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.sig.Context().Done():
		if cause := context.Cause(b.sig.Context()); cause != nil {
			return fmt.Errorf("%s signaling stopped: %w", b.name, cause)
		}
		return fmt.Errorf("%s signaling stopped", b.name)
	}
}

// NotifySignal handles one incoming signal. It must not block the signaling implementation, so
// the actual forward always happens on a timer goroutine rather than inline.
func (b *signalBroker) NotifySignal(signal *nethernet.Signal) bool {
	key := connectionKey{networkID: signal.NetworkID, connectionID: signal.ConnectionID}

	switch signal.Type {
	case nethernet.SignalTypeOffer:
		b.mu.Lock()
		if _, ok := b.pending[key]; ok {
			b.mu.Unlock()
			b.log.Warn("duplicate offer for a connection already negotiating", "connection", key)
			return false
		}
		if len(signal.Data) > maxOfferBytes || b.inFlight >= maxInFlight {
			b.refusedLocked(key, len(signal.Data))
			b.mu.Unlock()
			return false
		}
		offer := &brokeredOffer{offer: signal.Data, deadline: time.Now().Add(maxGather)}
		// An offer that already carries its candidates inline came from a client that is not
		// trickling, so there is nothing to wait for and no reason to add latency to the join.
		if hasInlineCandidate(signal.Data) {
			offer.deadline = time.Now()
		}
		b.pending[key] = offer
		b.inFlight++
		b.arm(key, offer)
		b.mu.Unlock()
	case nethernet.SignalTypeCandidate:
		b.mu.Lock()
		offer, ok := b.pending[key]
		if ok && !offer.done && len(offer.candidates) < maxCandidates &&
			len(signal.Data) <= maxCandidateBytes && !isRelayCandidate(signal.Data) {
			offer.candidates = append(offer.candidates, signal.Data)
			b.arm(key, offer)
		}
		b.mu.Unlock()
		if !ok {
			// Nothing can be done with a candidate that arrives after the bundle went out: the
			// backend's HTTP signaling has no second round trip to deliver it on. ICE works with
			// the pairs it already has, which is why maxGather exists.
			b.log.Debug("candidate arrived after the offer was forwarded", "connection", key)
		}
	case nethernet.SignalTypeError:
		b.mu.Lock()
		if offer, ok := b.pending[key]; ok && !offer.done {
			offer.done = true
			if offer.timer != nil {
				offer.timer.Stop()
			}
			delete(b.pending, key)
			b.inFlight--
		}
		b.mu.Unlock()
		b.log.Debug("client abandoned the negotiation", "connection", key, "code", signal.Data)
	default:
		// Only a client's half of the exchange reaches a host; an answer here is not ours to act on.
		return false
	}
	return true
}

// arm schedules the forward, extending the wait while candidates keep arriving but never past
// the offer's deadline. Called with b.mu held.
func (b *signalBroker) arm(key connectionKey, offer *brokeredOffer) {
	wait := candidateQuiet
	if until := time.Until(offer.deadline); until < wait {
		wait = until
	}
	if wait < 0 {
		wait = 0
	}
	if offer.timer == nil {
		offer.timer = time.AfterFunc(wait, func() { b.forward(key) })
		return
	}
	offer.timer.Reset(wait)
}

// forward hands the buffered offer to the backend and returns its answer to the client. After it
// returns, this process has no further part in the connection.
func (b *signalBroker) forward(key connectionKey) {
	b.mu.Lock()
	offer, ok := b.pending[key]
	if !ok || offer.done {
		b.mu.Unlock()
		return
	}
	offer.done = true
	delete(b.pending, key)
	sdp, candidates := offer.offer, offer.candidates
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()

	// Tied to the signaling context, not the process: if signaling has died there is no way to
	// return an answer and no point negotiating one.
	ctx, cancel := context.WithTimeout(b.sig.Context(), negotiationTimeout)
	defer cancel()

	// Who is joining. Verification only decides what this process does with the join; the
	// backend still authenticates the player's login itself, whatever happens here.
	var player PlayerToken
	var identityErr error
	if b.verify == nil {
		identityErr = errors.New("no token verifier configured")
	} else {
		player, identityErr = verifyOfferIdentity(ctx, sdp, b.verify)
	}
	if b.allow != nil {
		// allowed_xuids fails closed: a player who cannot be identified is not on the list.
		if identityErr != nil {
			b.log.Warn("refused a join: identity could not be verified, and allowed_xuids is set",
				"connection", key, "err", identityErr)
			b.fail(ctx, key, nethernet.ErrorCodeIdentityNotAllowed)
			return
		}
		if !b.allow(player.XUID) {
			b.log.Info("refused a join: not in allowed_xuids",
				"connection", key, "xuid", player.XUID, "name", player.Name)
			b.fail(ctx, key, nethernet.ErrorCodeIdentityNotAllowed)
			return
		}
	} else if identityErr != nil {
		b.log.Info("could not verify who is joining; the backend's own login check still applies",
			"connection", key, "err", identityErr)
	}

	sdp, strippedRelay := stripRelayCandidates(sdp)
	answer, err := postSDPOffer(ctx, b.client, b.backend, joinPathID(), inlineCandidates(sdp, candidates))
	if err != nil {
		b.log.Error("backend would not answer the offer",
			"connection", key, "candidates", len(candidates), "err", err)
		// Without this the client sits on a loading screen until its own timeout expires.
		b.fail(ctx, key, nethernet.ErrorCodeFailedToCreateAnswer)
		return
	}
	if err := b.sig.Signal(ctx, &nethernet.Signal{
		Type:         nethernet.SignalTypeAnswer,
		ConnectionID: key.connectionID,
		Data:         answer,
		NetworkID:    key.networkID,
	}); err != nil {
		b.log.Error("could not signal the answer back to the client", "connection", key, "err", err)
		return
	}
	// The answer already carries the backend's candidates inline, which is exactly how a
	// direct-IP join works today. They go out as CANDIDATEADD as well because clients on this
	// transport expect trickle; a candidate a client already took from the SDP is ignored as a
	// duplicate, so the only cost is a few extra signaling messages.
	for _, candidate := range answerCandidates(answer) {
		if err := b.sig.Signal(ctx, &nethernet.Signal{
			Type:         nethernet.SignalTypeCandidate,
			ConnectionID: key.connectionID,
			Data:         candidate,
			NetworkID:    key.networkID,
		}); err != nil {
			b.log.Debug("could not trickle a backend candidate", "connection", key, "err", err)
			break
		}
	}
	// Recorded once the backend has answered, the closest point to "joined" this process sees -
	// the relay modes recorded it on the relayed login, a step later. Only a verified XUID is
	// recorded: friend_activity.txt decides who pruneinactive keeps as a friend.
	if identityErr == nil && b.onJoin != nil {
		b.onJoin(player.XUID, player.Name)
	}
	b.log.Info("player negotiated a direct connection to the backend",
		"connection", key, "xuid", player.XUID, "name", player.Name,
		"clientCandidates", len(candidates), "strippedRelayCandidates", strippedRelay)
}

// refusedLocked counts an offer turned away for exceeding a limit and logs a summary at most once
// per dropLogInterval. Called with b.mu held.
func (b *signalBroker) refusedLocked(key connectionKey, size int) {
	b.dropped++
	if time.Since(b.droppedLogged) < dropLogInterval {
		return
	}
	b.log.Warn("refusing offers over the limits - too many joins at once, or oversized offers",
		"refused", b.dropped, "inFlight", b.inFlight, "maxInFlight", maxInFlight,
		"lastConnection", key, "lastOfferBytes", size, "maxOfferBytes", maxOfferBytes)
	b.dropped, b.droppedLogged = 0, time.Now()
}

// isRelayCandidate reports whether an ICE candidate is a TURN relay candidate ("typ relay"): an
// address on one of Microsoft's relay servers rather than on the player's own connection.
//
// Those are dropped from what the backend is told. A backend has a public address, so a direct
// route to it exists whenever the player's network allows one, and when it does not the client
// can still reach the backend through its relay on its own initiative - the backend answers such
// checks as they arrive without having to be told the relay's address. Telling it would only let
// the backend open the relayed path from its side as well.
//
// This cannot stop a client that has decided to use its relay - that choice is the client's own.
// A Windows client was captured sending not one packet straight to the backend, only through a
// Microsoft relay (30-140ms, depending on which relay server it was given), while an Android
// client on the same network connected directly. So the relayed route is not forced by the
// network, and nothing a server sends overrides it.
func isRelayCandidate(candidate string) bool {
	fields := strings.Fields(candidate)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "typ" {
			return fields[i+1] == "relay"
		}
	}
	return false
}

// stripRelayCandidates removes TURN relay candidate lines from an SDP and reports how many went.
// See isRelayCandidate.
func stripRelayCandidates(sdp string) (string, int) {
	if !strings.Contains(sdp, "relay") {
		return sdp, 0
	}
	var b strings.Builder
	b.Grow(len(sdp))
	removed := 0
	for rest := sdp; rest != ""; {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = ""
		}
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a=candidate:"); ok && isRelayCandidate(value) {
			removed++
			continue
		}
		b.WriteString(line)
	}
	return b.String(), removed
}

// joinPathID mints the /v1/join/{networkID} path segment for one forwarded offer.
//
// This is the HTTP leg's own client-side handle and has nothing to do with the Xbox Live
// signaling ID, so it must not be taken from the signaling transport. That ID is a decimal
// NetherNetId on the websocket transport but the messaging UUID (the session's PmsgId) on the
// messaging one - and BDS answers anything but a decimal uint64 with a 400, as nnserver.go's
// handleOffer mirrors. Newer clients prefer the messaging path, so reusing its ID would have
// failed most Friends-tab joins. Decimal and masked to 63 bits, as httpSignaling mints its own.
//
// It is fresh per offer rather than per broker so that concurrent joins can never share one, even
// on a backend that keyed its negotiations by network ID alone.
func joinPathID() string {
	return strconv.FormatUint(mrand.Uint64()&^(1<<63), 10)
}

// fail tells the client the negotiation is over so it stops waiting on it.
func (b *signalBroker) fail(ctx context.Context, key connectionKey, code int) {
	if err := b.sig.Signal(ctx, &nethernet.Signal{
		Type:         nethernet.SignalTypeError,
		ConnectionID: key.connectionID,
		Data:         strconv.Itoa(code),
		NetworkID:    key.networkID,
	}); err != nil {
		b.log.Debug("could not signal the error to the client", "connection", key, "err", err)
	}
}

// hasInlineCandidate reports whether an SDP already carries ICE candidate attributes.
func hasInlineCandidate(sdp string) bool {
	return strings.HasPrefix(sdp, "a=candidate:") || strings.Contains(sdp, "\na=candidate:")
}

// inlineCandidates writes trickled candidates into an SDP as media-level attribute lines, placed
// where a server that never trickles would have put them.
//
// Position matters more than it needs to for go-nethernet: its parseDescription reads candidates
// from anywhere in the session or media sections, so a Dragonfly backend would accept them
// appended at the very end. A native BDS parses SDP with Mojang's own C++ code, which has only
// ever been shown offers laid out the way go-nethernet's description.encode lays out a
// non-trickle one - candidate attributes FIRST in the media section, before ice-ufrag. Matching
// that layout keeps the forwarded offer as close as possible to one BDS is known to accept.
//
// The m= port and c= address are left as the client sent them (9 and 0.0.0.0 on a trickling
// client). encode fills them from a default candidate, but with ICE in use they are placeholders
// and the candidate lines are what get used.
func inlineCandidates(sdp string, candidates []string) string {
	var lines []string
	for _, candidate := range candidates {
		if line := candidateAttribute(candidate); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return sdp
	}

	// Keep the SDP's own line ending so the inserted lines never mix styles.
	eol := "\n"
	if strings.Contains(sdp, "\r\n") {
		eol = "\r\n"
	}
	insert := strings.Join(lines, eol) + eol

	// Insert before the first attribute of the media section, i.e. the first a= line after m=.
	// Media-level i=, c=, b= and k= lines precede the attributes, so they are stepped over.
	if at := mediaAttributeOffset(sdp); at >= 0 {
		return sdp[:at] + insert + sdp[at:]
	}
	// No media attributes to anchor on: append, which parseDescription still accepts.
	if sdp != "" && !strings.HasSuffix(sdp, "\n") {
		sdp += eol
	}
	return sdp + insert
}

// mediaAttributeOffset returns the byte offset of the first a= line in the media section of an
// SDP, or -1 if there is no m= line or no attribute follows it.
func mediaAttributeOffset(sdp string) int {
	inMedia := false
	for offset := 0; offset < len(sdp); {
		end := strings.IndexByte(sdp[offset:], '\n')
		next := len(sdp)
		if end >= 0 {
			next = offset + end + 1
		}
		line := sdp[offset:next]
		switch {
		case strings.HasPrefix(line, "m="):
			inMedia = true
		case inMedia && strings.HasPrefix(line, "a="):
			return offset
		}
		offset = next
	}
	return -1
}

// candidateAttribute turns one trickled candidate into an "a=candidate:..." attribute line, or
// returns "" for an empty one. go-nethernet signals candidates as "candidate:...", but a real
// Bedrock client is Mojang's C++ WebRTC, so both an existing a= prefix and a missing candidate:
// prefix are tolerated - the same leniency parseRemoteCandidate applies on the receiving side.
func candidateAttribute(candidate string) string {
	candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "a=")
	if candidate == "" {
		return ""
	}
	if !strings.HasPrefix(candidate, "candidate:") {
		candidate = "candidate:" + candidate
	}
	return "a=" + candidate
}

// answerCandidates pulls the candidate attributes out of an SDP answer, in the same
// "candidate:..." form a CANDIDATEADD signal carries.
func answerCandidates(sdp string) []string {
	var out []string
	for _, line := range strings.Split(sdp, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "a=candidate:"); ok {
			out = append(out, "candidate:"+value)
		}
	}
	return out
}
