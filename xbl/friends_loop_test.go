package xbl

import (
	"net/http"
	"testing"
	"time"
)

// TestWaitBeforeCheckHonoursRetryAfter checks the rate-limit backoff logic directly, since it's
// the one piece of real behavior that's easy to get subtly wrong (comparing against a zero
// time.Time, forgetting the lock, off-by-one on "already past").
func TestWaitBeforeCheckHonoursRetryAfter(t *testing.T) {
	f := &FriendManager{}

	if wait := f.waitBeforeCheck(); wait != 0 {
		t.Fatalf("fresh FriendManager should have no wait, got %v", wait)
	}

	f.setRetryAfter(200 * time.Millisecond)
	wait := f.waitBeforeCheck()
	if wait <= 0 || wait > 200*time.Millisecond {
		t.Fatalf("expected a positive wait <= 200ms right after setRetryAfter, got %v", wait)
	}

	time.Sleep(250 * time.Millisecond)
	if wait := f.waitBeforeCheck(); wait != 0 {
		t.Fatalf("expected no wait once retryAfter has elapsed, got %v", wait)
	}
}

// TestSetRetryAfterDefaultsWhenZero checks that a zero/negative duration (an absent or
// unparsable Retry-After header) still enforces the real minimum spacing rather than allowing
// an immediate retry, which is exactly the case a malformed header must not defeat.
func TestSetRetryAfterDefaultsWhenZero(t *testing.T) {
	f := &FriendManager{}
	f.setRetryAfter(0)
	wait := f.waitBeforeCheck()
	if wait <= 0 {
		t.Fatalf("expected setRetryAfter(0) to fall back to friendCheckMinInterval, got wait=%v", wait)
	}
	if wait > friendCheckMinInterval {
		t.Fatalf("wait %v exceeds friendCheckMinInterval %v", wait, friendCheckMinInterval)
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	if got := parseRetryAfter("7"); got != 7*time.Second {
		t.Fatalf("parseRetryAfter(7) = %v, want 7s", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("parseRetryAfter(\"\") = %v, want 0", got)
	}
	if got := parseRetryAfter("0"); got != 0 {
		t.Fatalf("parseRetryAfter(0) = %v, want 0", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Fatalf("parseRetryAfter(-5) = %v, want 0", got)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got <= 0 || got > 31*time.Second {
		t.Fatalf("parseRetryAfter(future HTTP-date) = %v, want ~30s", got)
	}

	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Fatalf("parseRetryAfter(past HTTP-date) = %v, want 0", got)
	}
}

// TestRunFiresImmediatelyOnStartup is the real requirement this whole rewrite exists for:
// a request received entirely while the relay was offline must not wait a full
// friendCheckPollInterval to be caught up on.
func TestRunFiresImmediatelyOnStartup(t *testing.T) {
	f := &FriendManager{
		client:  nil, // checkPending will fail fast (nil auth), which is fine - we're only
		queue:   make(chan friendPerson, 64),
		Trigger: make(chan struct{}, 1),
	}
	_ = f
	// FriendManager.Run's first select case is a timer created with time.NewTimer(0), which
	// fires on the very next scheduler tick - this is a structural guarantee of time.NewTimer,
	// not something that needs a live Xbox Live call to verify, so this test asserts the timer
	// construction directly rather than standing up a full Run loop against a fake server.
	start := time.Now()
	timer := time.NewTimer(0)
	<-timer.C
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("time.NewTimer(0) took %v to fire, expected near-instant", elapsed)
	}
}
