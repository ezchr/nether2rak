package invitequeue

// Invite acceptance tracking: which players joined our world after being sent an invite.
//
// SentFile records the first time each player was invited, one "xuid,unixSeconds" line each
// (append-only, deduplicated in memory by the process that sends). AcceptedFile records every
// invited player who later joined, one "xuid,name,firstInviteUnix,joinUnix" line each. Both live
// in the relay's working directory beside invite_queue.txt, so every relay process sharing that
// directory sees the same history: only one process sends invites, but a player can join through
// either host account.

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SentFile     = "invites_sent.txt"
	AcceptedFile = "invite_accepted.txt"
)

var (
	sentMu     sync.Mutex
	sentLoaded bool
	sent       map[string]bool
)

// RecordSent notes that xuid was just invited. Only the first invite is written: the question
// this answers is "had they been invited before they joined", and the first time decides that.
func RecordSent(xuid string) {
	if xuid == "" {
		return
	}
	sentMu.Lock()
	defer sentMu.Unlock()
	if !sentLoaded {
		sent = firstFields(SentFile)
		sentLoaded = true
	}
	if sent[xuid] {
		return
	}
	sent[xuid] = true
	appendLine(SentFile, fmt.Sprintf("%s,%d", xuid, time.Now().Unix()))
}

// RecordAcceptIfInvited adds xuid to AcceptedFile if it was invited before now and is not listed
// yet. It reads SentFile fresh each time because the sending process may be a different relay;
// joins are rare enough next to invites that one scan per join costs nothing noticeable.
func RecordAcceptIfInvited(xuid, name string, log *slog.Logger) {
	if xuid == "" || firstFields(AcceptedFile)[xuid] {
		return
	}
	invitedAt, ok := firstSentTime(xuid)
	if !ok {
		return
	}
	name = strings.ReplaceAll(name, ",", "")
	appendLine(AcceptedFile, fmt.Sprintf("%s,%s,%d,%d", xuid, name, invitedAt.Unix(), time.Now().Unix()))
	log.Info("invited player joined", "xuid", xuid, "name", name,
		"invitedAgo", time.Since(invitedAt).Round(time.Second))
}

// firstSentTime is the earliest recorded invite to xuid.
func firstSentTime(xuid string) (time.Time, bool) {
	f, err := os.Open(SentFile)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	var earliest time.Time
	found := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		x, rest, ok := strings.Cut(scanner.Text(), ",")
		if !ok || strings.TrimSpace(x) != xuid {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			continue
		}
		t := time.Unix(secs, 0)
		if !found || t.Before(earliest) {
			earliest, found = t, true
		}
	}
	return earliest, found
}

// firstFields is the set of first comma-separated fields in path. Missing file = empty set.
func firstFields(path string) map[string]bool {
	set := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return set
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if x, _, _ := strings.Cut(scanner.Text(), ","); strings.TrimSpace(x) != "" {
			set[strings.TrimSpace(x)] = true
		}
	}
	return set
}

// appendLine appends one line. O_APPEND keeps concurrent short writes from two processes whole.
func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, line)
}
