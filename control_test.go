package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// echo answers with the path it was given, so tests can see what reached a broadcast's routes.
func echo(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tag+" "+r.URL.Path)
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestControlPing(t *testing.T) {
	c := newControlServer(false)
	if code, _ := get(t, c, "/ping"); code != http.StatusBadRequest {
		t.Fatalf("no xuid: %d", code)
	}
	if code, _ := get(t, c, "/ping?xuid=123"); code != http.StatusNotFound {
		t.Fatalf("unknown player: %d", code)
	}
}

func TestControlInvitesFollowTheSession(t *testing.T) {
	c := newControlServer(false)
	if code, _ := get(t, c, "/invites/status"); code != http.StatusServiceUnavailable {
		t.Fatalf("before any session: %d", code)
	}

	first, endFirst := context.WithCancel(context.Background())
	c.setInvites(first, primaryBroadcast, echo("first"))
	if code, body := get(t, c, "/invites/status"); code != 200 || body != "first /invites/status" {
		t.Fatalf("live session: %d %q", code, body)
	}

	// The next session takes the slot before the old one is seen to end: the old one ending
	// must not take the new one's routes out.
	c.setInvites(context.Background(), primaryBroadcast, echo("second"))
	endFirst()
	time.Sleep(20 * time.Millisecond)
	if _, body := get(t, c, "/invites/status"); body != "second /invites/status" {
		t.Fatalf("new session's routes were removed: %q", body)
	}

	third, endThird := context.WithCancel(context.Background())
	c.setInvites(third, primaryBroadcast, echo("third"))
	endThird()
	waitFor(t, func() bool {
		code, _ := get(t, c, "/invites/status")
		return code == http.StatusServiceUnavailable
	})
}

func TestControlExtraBroadcastPaths(t *testing.T) {
	c := newControlServer(false)
	c.setInvites(context.Background(), primaryBroadcast, echo("primary"))
	c.setInvites(context.Background(), "alt", echo("alt"))
	if _, body := get(t, c, "/broadcast/alt/invites/start?interval=1s"); body != "alt /invites/start" {
		t.Fatalf("got %q", body)
	}
	if _, body := get(t, c, "/friends/add/status"); body != "primary /friends/add/status" {
		t.Fatalf("got %q", body)
	}
	if code, _ := get(t, c, "/broadcast/nobody/invites/status"); code != http.StatusServiceUnavailable {
		t.Fatalf("unknown broadcast: %d", code)
	}
	if code, _ := get(t, c, "/something"); code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestControlPprofOnlyWithDebug(t *testing.T) {
	if code, _ := get(t, newControlServer(false), "/debug/pprof/"); code != http.StatusNotFound {
		t.Fatalf("pprof without -debug: %d", code)
	}
	if code, body := get(t, newControlServer(true), "/debug/pprof/"); code != 200 || !strings.Contains(body, "goroutine") {
		t.Fatalf("pprof with -debug: %d", code)
	}
}

func TestControlPortFromOldSettings(t *testing.T) {
	cfg := loadFrom(t, `{"ping_port":7779,"invite_port":7784,"pprof_port":6062}`)
	if cfg.ControlPort != 7779 {
		t.Fatalf("old ping port should be kept for the ping plugin, got %d", cfg.ControlPort)
	}
	if !hasNote(cfg, `replaced by "control_port"`) {
		t.Fatalf("notes %v", cfg.Notes)
	}
	if cfg := loadFrom(t, `{"invite_port":7784}`); cfg.ControlPort != 7784 {
		t.Fatalf("invite port alone: %d", cfg.ControlPort)
	}
	if cfg := loadFrom(t, `{"control_port":8000,"ping_port":7779}`); cfg.ControlPort != 8000 {
		t.Fatalf("control_port must win: %d", cfg.ControlPort)
	}
	cfg = loadFrom(t, `{}`)
	if cfg.ControlPort != defaultControlPort || len(cfg.Notes) != 0 || cfg.BroadcastName != primaryBroadcast || !cfg.InvitesEnabled {
		t.Fatalf("default: %+v", cfg)
	}
}

func inTempDir(t *testing.T) {
	t.Helper()
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
}

func TestAdoptLegacyInviteState(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := ".invite_state_primary.json"

	inTempDir(t)
	os.WriteFile(".invite_state_7784.json", []byte(`{"running":true,"last":"123"}`), 0644)
	adoptLegacyInviteState(path, primaryBroadcast, log)
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), `"123"`) {
		t.Fatalf("the old file was not taken over: %v %s", err, b)
	}

	inTempDir(t)
	os.WriteFile(".invite_state_7784.json", []byte(`{}`), 0644)
	os.WriteFile(".invite_state_7790.json", []byte(`{}`), 0644)
	adoptLegacyInviteState(path, primaryBroadcast, log)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("two old files: which is the primary's can't be told, nothing should be taken")
	}

	inTempDir(t)
	os.WriteFile(".invite_state_7790.json", []byte(`{}`), 0644)
	adoptLegacyInviteState(".invite_state_alt.json", "alt", log)
	if _, err := os.Stat(".invite_state_alt.json"); err == nil {
		t.Fatal("only the primary adopts an old file")
	}
}
