package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestParseInviteInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":     inviteRateLimit,
		"1s":   time.Second,
		"2.5s": 2500 * time.Millisecond,
		"1m":   time.Minute,
	} {
		if got, err := parseInviteInterval(in); err != nil || got != want {
			t.Errorf("parseInviteInterval(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"500ms", "0s", "-1s", "fast", "1"} {
		if _, err := parseInviteInterval(in); err == nil {
			t.Errorf("parseInviteInterval(%q) accepted", in)
		}
	}
}

func TestResumeIndex(t *testing.T) {
	q := []string{"a", "b", "c"}
	for last, want := range map[string]int{"": 0, "a": 1, "b": 2, "c": 3, "gone": 0} {
		if got := resumeIndex(q, last); got != want {
			t.Errorf("resumeIndex(%q) = %d, want %d", last, got, want)
		}
	}
}

// testController is an invite controller with no session. Its loop never sends anything in these
// tests: there is no invite_queue.txt in the test directory, so it only waits.
func testController(t *testing.T, statePath string) *inviteController {
	t.Helper()
	return &inviteController{log: slog.New(slog.DiscardHandler), statePath: statePath}
}

// TestInviteStateCarriesToNextSession: a session ending does not stop invites - the next one's
// controller picks them up after the last player invited - while an explicit stop does.
func TestInviteStateCarriesToNextSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".invite_state_1.json")

	// Session 1 starts invites and gets through player "b".
	s1ctx, endSession1 := context.WithCancel(context.Background())
	first := testController(t, path)
	first.Start(s1ctx, time.Second, false)
	first.recordProgress(s1ctx, time.Second, inviteListQueue, "b")
	endSession1() // a rebuild: nobody called Stop

	st := first.loadState()
	if !st.Running || st.Last != "b" || st.Interval != "1s" {
		t.Fatalf("after the session ended: %+v, want running, last b, 1s", st)
	}

	// Session 2's controller resumes by itself.
	s2ctx, endSession2 := context.WithCancel(context.Background())
	defer endSession2()
	second := testController(t, path)
	second.ResumeIfWanted(s2ctx)
	if running, interval := second.Running(); !running || interval != time.Second {
		t.Fatalf("new session did not resume: running=%v interval=%v", running, interval)
	}
	if st := second.loadState(); st.Last != "b" {
		t.Errorf("resumed with last %q, want b", st.Last)
	}

	// An explicit stop is remembered: the session after that does not resume.
	second.Stop()
	if st := second.loadState(); st.Running || st.Last != "b" {
		t.Errorf("after stop: %+v, want stopped with the position kept", st)
	}
	third := testController(t, path)
	third.ResumeIfWanted(context.Background())
	if running, _ := third.Running(); running {
		t.Error("a stopped loop resumed on the next session")
	}
}

// TestStopWinsOverInFlightInvite: progress recorded after the loop was cancelled must not undo a
// stop, or the next session would resume invites that were stopped.
func TestStopWinsOverInFlightInvite(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".invite_state_1.json")
	c := testController(t, path)
	c.Start(context.Background(), time.Second, false)
	c.Stop()

	// The loop's context, as it is once Stop has cancelled it.
	loopCtx, cancel := context.WithCancel(context.Background())
	cancel()
	c.recordProgress(loopCtx, time.Second, inviteListQueue, "late")
	if st := c.loadState(); st.Running || st.Last == "late" {
		t.Errorf("an invite finishing after stop overwrote it: %+v", st)
	}
}

func TestStartFromTopIgnoresPosition(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".invite_state_1.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := testController(t, path)
	c.stateMu.Lock()
	c.saveStateLocked(inviteState{Running: false, Interval: "1s", Last: "b"})
	c.stateMu.Unlock()

	c.Start(ctx, time.Second, true)
	if st := c.loadState(); st.Last != "" {
		t.Errorf("restart from the top kept position %q", st.Last)
	}
}
