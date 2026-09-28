package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gameparrot/netherconnect/invitequeue"
)

// The player files hold only what a person reading them cares about (who, and where they were
// seen). When each player was first seen - needed only to expire old entries - lives in a hidden
// companion file next to each one: ".server_players.seen" for "server_players.txt", and so on,
// one "xuid,unixSeconds" line per player.
//
// A player with no line in the companion file never expires. That covers everything recorded
// before discovery times were tracked this way, which is deliberate: those entries were never
// subject to expiry before either (see invitequeue.parseLine's handling of timestamp-less lines).

// seenPath returns the companion file for the player file at path.
func seenPath(path string) string {
	dir, base := filepath.Split(path)
	return filepath.Join(dir, "."+strings.TrimSuffix(base, filepath.Ext(base))+".seen")
}

// pruneExpired removes every player first seen more than invitequeue.Expiry ago from the player
// file at path and from its companion file. Missing files count as empty.
func pruneExpired(path string, log *slog.Logger) {
	times, err := readLines(seenPath(path))
	if err != nil {
		log.Warn("could not read discovery times, skipping cleanup", "path", seenPath(path), "err", err)
		return
	}
	expired := make(map[string]bool)
	for _, line := range times {
		xuid, unix, _ := strings.Cut(line, ",")
		if sec, err := strconv.ParseInt(strings.TrimSpace(unix), 10, 64); err == nil && time.Since(time.Unix(sec, 0)) > invitequeue.Expiry {
			expired[xuid] = true
		}
	}
	if len(expired) == 0 {
		return
	}
	for _, p := range []string{path, seenPath(path)} {
		lines, err := readLines(p)
		if err != nil {
			log.Warn("could not read file for cleanup", "path", p, "err", err)
			return
		}
		kept := lines[:0]
		for _, line := range lines {
			if xuid, _, _ := strings.Cut(line, ","); !expired[xuid] {
				kept = append(kept, line)
			}
		}
		if err := rewriteLines(p, kept); err != nil {
			log.Warn("could not rewrite file after cleanup", "path", p, "err", err)
			return
		}
	}
	log.Info("removed players first seen over 30 days ago", "path", path, "count", len(expired))
}

// readLines returns the non-empty lines of path, or none if it does not exist.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// rewriteLines replaces path's contents through a temp file and a rename, so a crash mid-write
// never leaves a truncated file. Open append handles must be reopened afterwards (queueWriter.reopen).
func rewriteLines(path string, lines []string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
