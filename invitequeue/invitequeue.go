// Package invitequeue holds the pruning logic shared between cmd/scraper (which writes
// invite_queue.txt) and nether2rak's own invitewatcher.go (which reads it) - see each file's own
// package doc for the full split rationale. They are separate binaries/processes, so this can't
// be an unexported helper in either one; it has to live somewhere both can import.
package invitequeue

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Expiry is how long a discovered player stays in the queue before being dropped. Prevents
// invite_queue.txt from growing forever with people who were seen once on some target server
// months ago and have no realistic chance of still caring about an invite - without this,
// nothing ever removes an entry once written.
const Expiry = 30 * 24 * time.Hour

// Prune reads path, drops any line older than Expiry, and rewrites the file with what remains.
// Safe to call whether or not the file exists (a missing file is treated as already-empty, not
// an error) and safe to call from either the scraper or invitewatcher process - neither depends
// on the other being up for this to run, which is the whole reason this exists as a shared,
// explicitly-invoked step rather than something folded into one side's own read path.
//
// Each line is "xuid,username,discoveredUnixSeconds" (see cmd/scraper's write side). A line from
// before the timestamp field existed (bare "xuid" or "xuid,username") is treated as discovered
// one day ago rather than expiring instantly or living forever exempt from Expiry - this matters
// only on the very first Prune call against a pre-existing file that predates this feature.
func Prune(path string, log *slog.Logger) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("could not read invite queue file for pruning", "path", path, "err", err)
		}
		return
	}

	now := time.Now()
	var kept []string
	expiredCount := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		xuid, discoveredAt := parseLine(line)
		if xuid == "" {
			continue
		}
		if now.Sub(discoveredAt) >= Expiry {
			expiredCount++
			continue
		}
		kept = append(kept, line)
	}
	_ = f.Close()
	if err := scanner.Err(); err != nil {
		log.Warn("error reading invite queue file for pruning", "path", path, "err", err)
		return
	}

	if expiredCount == 0 {
		return
	}
	if err := rewrite(path, kept); err != nil {
		log.Warn("could not rewrite invite queue file after pruning", "path", path, "err", err)
		return
	}
	log.Info("pruned expired invite queue entries", "count", expiredCount, "remaining", len(kept))
}

// parseLine splits one invite_queue.txt line into its xuid and discovery time. xuid is "" for a
// blank or malformed line, which the caller skips.
func parseLine(line string) (xuid string, discoveredAt time.Time) {
	fields := strings.Split(line, ",")
	xuid = strings.TrimSpace(fields[0])
	if xuid == "" {
		return "", time.Time{}
	}
	if len(fields) >= 3 {
		if unixSeconds, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil {
			return xuid, time.Unix(unixSeconds, 0)
		}
	}
	return xuid, time.Now().Add(-24 * time.Hour)
}

// rewrite atomically replaces path's contents with lines, one per line. Writes to a temp file in
// the same directory first and renames over the original, so a crash or a concurrent reader mid-
// write never sees a truncated or partially-written queue file - the scraper may be appending to
// this same file concurrently while this runs.
func rewrite(path string, lines []string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
