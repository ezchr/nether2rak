package bridge

import (
	"slices"
	"strings"
	"testing"
)

// canonicalOffer is laid out the way go-nethernet's description.encode lays out a non-trickle
// offer: session attributes, then the media section with candidate attributes first. This is the
// shape a native BDS is known to accept, since the relay's own backend dial produces it.
const canonicalOffer = "v=0\r\n" +
	"o=- 123 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0\r\n" +
	"a=extmap-allow-mixed\r\n" +
	"a=msid-semantic: WMS\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=candidate:0 1 udp 2130706431 203.0.113.7 50000 typ host\r\n" +
	"a=candidate:1 1 udp 1694498815 198.51.100.2 50001 typ srflx raddr 0.0.0.0 rport 0\r\n" +
	"a=ice-ufrag:abcd\r\n" +
	"a=ice-pwd:0123456789abcdefghijklmn\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 AA:BB\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:0\r\n" +
	"a=sctp-port:5000\r\n" +
	"a=max-message-size:262145\r\n"

// stripCandidates splits an offer into what a trickling client sends: the SDP without candidate
// lines, and the candidates as separate CANDIDATEADD payloads.
func stripCandidates(sdp string) (string, []string) {
	var kept, candidates []string
	for _, line := range strings.SplitAfter(sdp, "\r\n") {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a="); ok && strings.HasPrefix(value, "candidate:") {
			candidates = append(candidates, value)
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ""), candidates
}

// TestInlineCandidatesRebuildsCanonicalLayout is the property the whole mode rests on: a trickled
// offer plus its candidates must come back out byte-for-byte as the non-trickle offer a backend
// already accepts, candidates in the same place and order.
func TestInlineCandidatesRebuildsCanonicalLayout(t *testing.T) {
	trickled, candidates := stripCandidates(canonicalOffer)
	if len(candidates) != 2 {
		t.Fatalf("test setup: stripped %d candidates, want 2", len(candidates))
	}
	if strings.Contains(trickled, "a=candidate:") {
		t.Fatalf("test setup: candidates left in trickled offer")
	}
	if got := inlineCandidates(trickled, candidates); got != canonicalOffer {
		t.Errorf("rebuilt offer differs from canonical\n got: %q\nwant: %q", got, canonicalOffer)
	}
}

func TestInlineCandidatesPlacesThemBeforeICECredentials(t *testing.T) {
	trickled, candidates := stripCandidates(canonicalOffer)
	got := inlineCandidates(trickled, candidates)
	if strings.Index(got, "a=candidate:") > strings.Index(got, "a=ice-ufrag:") {
		t.Errorf("candidates must precede ice-ufrag, as description.encode writes them:\n%s", got)
	}
	// Session-level attributes must stay above m=, or they would become media attributes.
	if strings.Index(got, "a=group:") > strings.Index(got, "m=") {
		t.Errorf("session attributes moved below m=:\n%s", got)
	}
}

func TestInlineCandidatesNoCandidatesIsIdentity(t *testing.T) {
	for _, candidates := range [][]string{nil, {}, {"", "   "}} {
		if got := inlineCandidates(canonicalOffer, candidates); got != canonicalOffer {
			t.Errorf("inlineCandidates(%q) changed the offer", candidates)
		}
	}
}

func TestInlineCandidatesKeepsLineEnding(t *testing.T) {
	lf := strings.ReplaceAll(canonicalOffer, "\r\n", "\n")
	trickled, candidates := stripCandidates(canonicalOffer)
	trickled = strings.ReplaceAll(trickled, "\r\n", "\n")
	if got := inlineCandidates(trickled, candidates); got != lf {
		t.Errorf("LF offer not rebuilt with LF endings\n got: %q\nwant: %q", got, lf)
	}
}

// TestInlineCandidatesWithoutMediaAttributes covers the fallback: nothing to anchor on, so the
// candidates are appended, which go-nethernet's parser still accepts.
func TestInlineCandidatesWithoutMediaAttributes(t *testing.T) {
	sdp := "v=0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0"
	got := inlineCandidates(sdp, []string{"candidate:0 1 udp 1 203.0.113.7 5 typ host"})
	want := sdp + "\r\na=candidate:0 1 udp 1 203.0.113.7 5 typ host\r\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCandidateAttributeForms: the real client is Mojang's C++ WebRTC, not go-nethernet, so every
// plausible form of a CANDIDATEADD payload must come out as one well-formed attribute line.
func TestCandidateAttributeForms(t *testing.T) {
	const want = "a=candidate:0 1 udp 1 203.0.113.7 5 typ host"
	for _, in := range []string{
		"candidate:0 1 udp 1 203.0.113.7 5 typ host",   // go-nethernet's formatICECandidate
		"a=candidate:0 1 udp 1 203.0.113.7 5 typ host", // already an attribute line
		"0 1 udp 1 203.0.113.7 5 typ host",             // bare, no prefix at all
		"  candidate:0 1 udp 1 203.0.113.7 5 typ host\r\n",
	} {
		if got := candidateAttribute(in); got != want {
			t.Errorf("candidateAttribute(%q) = %q, want %q", in, got, want)
		}
	}
	if got := candidateAttribute("   "); got != "" {
		t.Errorf("blank candidate should be dropped, got %q", got)
	}
}

func TestAnswerCandidates(t *testing.T) {
	got := answerCandidates(canonicalOffer)
	want := []string{
		"candidate:0 1 udp 2130706431 203.0.113.7 50000 typ host",
		"candidate:1 1 udp 1694498815 198.51.100.2 50001 typ srflx raddr 0.0.0.0 rport 0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("answerCandidates = %q, want %q", got, want)
	}
	if got := answerCandidates("v=0\r\nm=application 9 x y\r\na=mid:0\r\n"); len(got) != 0 {
		t.Errorf("answer without candidates gave %q", got)
	}
}

// TestAnswerCandidatesRoundTrip: what the broker trickles back to the client must re-inline to the
// same attributes it was read from.
func TestAnswerCandidatesRoundTrip(t *testing.T) {
	trickled, _ := stripCandidates(canonicalOffer)
	if got := inlineCandidates(trickled, answerCandidates(canonicalOffer)); got != canonicalOffer {
		t.Errorf("round trip lost information\n got: %q\nwant: %q", got, canonicalOffer)
	}
}

func TestHasInlineCandidate(t *testing.T) {
	trickled, _ := stripCandidates(canonicalOffer)
	for sdp, want := range map[string]bool{
		canonicalOffer:             true,
		trickled:                   false,
		"a=candidate:0 1 x\n":      true, // first line
		"":                         false,
		"a=ice-ufrag:candidate:\n": false, // the word alone is not an attribute
	} {
		if got := hasInlineCandidate(sdp); got != want {
			t.Errorf("hasInlineCandidate(%q) = %v, want %v", sdp, got, want)
		}
	}
}
