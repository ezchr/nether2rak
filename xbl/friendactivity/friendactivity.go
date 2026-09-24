// Package friendactivity records the last time each friend actually joined this relay's worlds,
// and prunes friends who have not joined within a configurable window. It exists to answer "who
// is still using this" separately from Xbox Live's own friends list, which has no notion of
// activity at all - a friend added a year ago and never seen since looks identical there to one
// who joined an hour ago.
//
// Modelled on invitequeue's own split: a plain, atomically-rewritten text file that a writer
// (RecordJoin, called from bridge/relay.go on every real world entry) and a reader (Prune,
// invoked separately, on demand or on a timer - see its own doc comment for why this is not
// automatic) can both touch without depending on each other being the same process or running at
// the same time.
package friendactivity

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// sanitizeDisplayName strips commas from a display name before it goes into a comma-separated
// line, the same treatment cmd/scraper's own discovered-player writer gives Bedrock display
// names for the same file shape - a display name is untrusted, server-supplied text (or, here,
// Xbox Live's own DisplayName, which nether2rak's login-time identity capture does not otherwise
// validate the shape of), and a stray comma would silently shift every later field on that line.
func sanitizeDisplayName(name string) string {
	return strings.ReplaceAll(name, ",", "")
}

// Record is one friend's last known join.
type Record struct {
	XUID        string
	DisplayName string
	LastJoin    time.Time
}

// Store tracks last-join times in memory and mirrors them to a file on every update, so a
// restart does not lose history. Safe for concurrent use.
type Store struct {
	path string
	log  *slog.Logger

	mu      chan struct{} // 1-buffered mutex: see lock/unlock
	records map[string]Record
}

// Open loads path (if it exists) and returns a Store backed by it. A missing file is treated as
// an empty store, not an error, matching invitequeue.Prune's own convention.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{path: path, log: log, mu: make(chan struct{}, 1), records: map[string]Record{}}
	s.mu <- struct{}{}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("open friend activity file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		rec, ok := parseLine(line)
		if !ok {
			continue
		}
		s.records[rec.XUID] = rec
	}
	return s, nil
}

func (s *Store) lock()   { <-s.mu }
func (s *Store) unlock() { s.mu <- struct{}{} }

// RecordJoin updates xuid's last-join time to now and persists the store. Call this once a
// player has actually been handed into a real backend/world - not merely on Xbox Live
// authentication, which can happen for someone later rejected by an XUID allowlist and never
// actually let into any world.
func (s *Store) RecordJoin(xuid, displayName string) {
	s.lock()
	defer s.unlock()
	s.records[xuid] = Record{XUID: xuid, DisplayName: sanitizeDisplayName(displayName), LastJoin: time.Now()}
	if err := s.writeLocked(); err != nil {
		s.log.Error("failed to persist friend activity", "xuid", xuid, "err", err)
	}
}

// LastJoin reports the last recorded join for xuid, if any.
func (s *Store) LastJoin(xuid string) (time.Time, bool) {
	s.lock()
	defer s.unlock()
	rec, ok := s.records[xuid]
	return rec.LastJoin, ok
}

// Stale returns every recorded friend whose last join is older than window, sorted by XUID for a
// stable, diffable listing. A friend with no recorded join at all (added before this feature
// existed, or a real friend who has genuinely never joined) is NOT included - see Prune's doc
// comment for why that case needs a human decision, not an automatic one.
func (s *Store) Stale(window time.Duration) []Record {
	s.lock()
	defer s.unlock()
	cutoff := time.Now().Add(-window)
	var stale []Record
	for _, rec := range s.records {
		if rec.LastJoin.Before(cutoff) {
			stale = append(stale, rec)
		}
	}
	sortRecords(stale)
	return stale
}

// Forget removes xuid from the store and persists the removal. Call this after actually removing
// someone as a friend (see cmd/friends/removefriend), not before - Forget only clears this
// store's own bookkeeping, it does not touch the real Xbox Live friendship.
func (s *Store) Forget(xuid string) {
	s.lock()
	defer s.unlock()
	if _, ok := s.records[xuid]; !ok {
		return
	}
	delete(s.records, xuid)
	if err := s.writeLocked(); err != nil {
		s.log.Error("failed to persist friend activity after removal", "xuid", xuid, "err", err)
	}
}

// writeLocked rewrites the backing file from the current in-memory records. Caller must hold the
// lock. Uses the same temp-file-then-rename pattern as invitequeue.rewrite, so a crash or a
// concurrent reader mid-write never sees a truncated file.
func (s *Store) writeLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	records := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}
	sortRecords(records)
	for _, rec := range records {
		if _, err := fmt.Fprintf(f, "%s,%s,%d\n", rec.XUID, rec.DisplayName, rec.LastJoin.Unix()); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func sortRecords(records []Record) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && records[j-1].XUID > records[j].XUID; j-- {
			records[j-1], records[j] = records[j], records[j-1]
		}
	}
}

// parseLine splits one activity file line ("xuid,displayName,lastJoinUnixSeconds") into a
// Record. ok is false for a blank or malformed line, which the caller skips.
func parseLine(line string) (Record, bool) {
	fields := strings.SplitN(line, ",", 3)
	if len(fields) != 3 {
		return Record{}, false
	}
	xuid := strings.TrimSpace(fields[0])
	if xuid == "" {
		return Record{}, false
	}
	unixSeconds, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
	if err != nil {
		return Record{}, false
	}
	return Record{XUID: xuid, DisplayName: fields[1], LastJoin: time.Unix(unixSeconds, 0)}, true
}
