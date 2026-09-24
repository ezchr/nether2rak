package friendactivity

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRecordJoinPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.txt")

	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.RecordJoin("xuid-1", "Alice")
	s.RecordJoin("xuid-2", "Bob")

	last, ok := s.LastJoin("xuid-1")
	if !ok {
		t.Fatal("expected a recorded join for xuid-1")
	}
	if time.Since(last) > time.Second {
		t.Fatalf("recorded join time too old: %v", last)
	}

	// Reload from disk into a fresh Store, proving RecordJoin actually persisted rather than
	// only updating memory.
	reloaded, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reloaded.LastJoin("xuid-1"); !ok {
		t.Fatal("xuid-1 missing after reload")
	}
	if _, ok := reloaded.LastJoin("xuid-2"); !ok {
		t.Fatal("xuid-2 missing after reload")
	}
}

func TestOpenMissingFileIsEmptyNotError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.txt")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open on a missing file should not error, got %v", err)
	}
	if _, ok := s.LastJoin("anyone"); ok {
		t.Fatal("expected no records in a freshly-opened, previously-nonexistent store")
	}
}

func TestStaleOnlyReturnsRecordedAndOldEnough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.txt")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Recent join: must not be reported as stale even against a very short window.
	s.RecordJoin("recent", "Recent")

	// A genuinely old join, written directly into the backing store rather than via RecordJoin
	// (which always stamps "now"), to exercise the actual staleness comparison.
	s.lock()
	s.records["old"] = Record{XUID: "old", DisplayName: "Old", LastJoin: time.Now().Add(-48 * time.Hour)}
	_ = s.writeLocked()
	s.unlock()

	stale := s.Stale(24 * time.Hour)
	if len(stale) != 1 || stale[0].XUID != "old" {
		t.Fatalf("expected exactly [old] to be stale, got %+v", stale)
	}
}

func TestForgetRemovesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.txt")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.RecordJoin("xuid-1", "Alice")
	s.Forget("xuid-1")

	if _, ok := s.LastJoin("xuid-1"); ok {
		t.Fatal("expected xuid-1 to be gone after Forget")
	}

	reloaded, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reloaded.LastJoin("xuid-1"); ok {
		t.Fatal("xuid-1 should not reappear after reload post-Forget")
	}
}

func TestParseLineRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	rec, ok := parseLine("2535000000000000,SomeGamertag," + strconv.FormatInt(now.Unix(), 10))
	if !ok {
		t.Fatal("expected a valid parse")
	}
	if rec.XUID != "2535000000000000" || rec.DisplayName != "SomeGamertag" || !rec.LastJoin.Equal(now) {
		t.Fatalf("round-trip mismatch: %+v", rec)
	}

	for _, bad := range []string{"", "no-commas-here", "xuid,name,not-a-number"} {
		if _, ok := parseLine(bad); ok {
			t.Fatalf("expected parseLine(%q) to fail, it did not", bad)
		}
	}
}
