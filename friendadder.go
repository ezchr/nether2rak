package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
)

// This file sends friend requests, from the account hosting this relay's world, to players
// cmd/scraper recorded on one server or in one friend's world - the same account that invites
// them, so once they accept they see the world on their Friends tab. It reads the scraper's
// player files from this relay's run directory:
//
//	server_players.txt  xuid,name,server
//	world_players.txt   xuid,name,world,host
//
// OFF BY DEFAULT, controlled over the same loopback control port as the inviters:
//
//	curl 'http://127.0.0.1:<control_port>/friends/add/start?from=play.example.net:19132'
//	curl 'http://127.0.0.1:<control_port>/friends/add/start?from=<host gamertag or world name>'
//	curl http://127.0.0.1:<control_port>/friends/add/stop
//	curl http://127.0.0.1:<control_port>/friends/add/status
//
// Unlike the inviters, one adder lives for the whole process rather than one session generation:
// a run at one request per second can take hours, and session rebuilds happen several times a
// day. Every generation's control server routes to the same adder, so status stays truthful.

const (
	friendAddInterval = time.Second // chosen to stay under Xbox Live's rate limit
	// friendsAddedFile records who this account already sent a request to ("xuid,unixSeconds"),
	// so a rerun continues where the last one stopped.
	friendsAddedFile = ".added_friends"
	// maxAddFailures stops a run once Xbox Live keeps refusing requests in a row - most likely
	// the account's friend limit, which retrying cannot fix.
	maxAddFailures = 10
)

var scraperPlayerFiles = []string{"server_players.txt", "world_players.txt"}

type friendAdder struct {
	authSession *session.Session
	selfXUID    string
	log         *slog.Logger

	mu      sync.Mutex
	running bool
	stop    context.CancelFunc
	from    string
	sent    int
	total   int
}

// friendAdders holds one adder per broadcast, keyed by its name (unique per broadcast).
var friendAdders sync.Map

func friendAdderFor(broadcast string, authSession *session.Session, selfXUID string, log *slog.Logger) *friendAdder {
	a, _ := friendAdders.LoadOrStore(broadcast, &friendAdder{authSession: authSession, selfXUID: selfXUID, log: log.With("src", "friend-adder")})
	return a.(*friendAdder)
}

type addTarget struct{ xuid, name string }

// Start begins sending friend requests to everyone recorded from `from` who is not already a
// friend or already requested, running until done, stopped, or ctx ends. It returns how many
// requests it is about to send.
func (a *friendAdder) Start(ctx context.Context, from string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return 0, fmt.Errorf("already adding players from %q (%d/%d sent)", a.from, a.sent, a.total)
	}
	todo, err := a.pending(ctx, from)
	if err != nil {
		return 0, err
	}
	if len(todo) == 0 {
		return 0, nil
	}
	loopCtx, cancel := context.WithCancel(ctx)
	a.stop, a.running, a.from, a.sent, a.total = cancel, true, from, 0, len(todo)
	go a.run(loopCtx, todo)
	return len(todo), nil
}

func (a *friendAdder) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		a.stop()
	}
}

func (a *friendAdder) Status() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		if a.from == "" {
			return "stopped"
		}
		return fmt.Sprintf("stopped (last run: %d/%d sent from %q)", a.sent, a.total, a.from)
	}
	return fmt.Sprintf("running: %d/%d sent from %q", a.sent, a.total, a.from)
}

// pending lists who to add from `from`, skipping current friends, earlier requests and ourselves.
func (a *friendAdder) pending(ctx context.Context, from string) ([]addTarget, error) {
	recorded := recordedFrom(from)
	if len(recorded) == 0 {
		return nil, fmt.Errorf("no recorded players from %q in %s", from, strings.Join(scraperPlayerFiles, " or "))
	}
	skip := map[string]bool{a.selfXUID: true}
	for _, line := range readNonEmptyLines(friendsAddedFile) {
		xuid, _, _ := strings.Cut(line, ",")
		skip[xuid] = true
	}
	friends, err := xbl.ListFriends(ctx, a.authSession)
	if err != nil {
		return nil, fmt.Errorf("list current friends: %w", err)
	}
	for _, f := range friends {
		skip[f.XUID] = true
	}
	var todo []addTarget
	for _, t := range recorded {
		if !skip[t.xuid] {
			todo = append(todo, t)
			skip[t.xuid] = true
		}
	}
	a.log.Info("friend add planned", "from", from, "recorded", len(recorded), "toAdd", len(todo))
	return todo, nil
}

// recordedFrom returns every player in the scraper's files whose server address, world name or
// world host equals from (case-insensitive), in file order.
func recordedFrom(from string) []addTarget {
	var recorded []addTarget
	for _, file := range scraperPlayerFiles {
		for _, line := range readNonEmptyLines(file) {
			f := strings.Split(line, ",")
			if len(f) < 3 {
				continue
			}
			for _, where := range f[2:] { // server, or world and host
				if strings.EqualFold(strings.TrimSpace(where), from) {
					recorded = append(recorded, addTarget{xuid: f[0], name: f[1]})
					break
				}
			}
		}
	}
	return recorded
}

func (a *friendAdder) run(ctx context.Context, todo []addTarget) {
	defer func() {
		a.mu.Lock()
		a.running, a.stop = false, nil
		a.mu.Unlock()
	}()
	added, err := os.OpenFile(friendsAddedFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		a.log.Error("cannot record sent requests, not starting", "err", err)
		return
	}
	defer added.Close()

	ticker := time.NewTicker(friendAddInterval)
	defer ticker.Stop()
	failures := 0
	for i := 0; i < len(todo); {
		select {
		case <-ctx.Done():
			a.log.Info("friend add stopped", "remaining", len(todo)-i)
			return
		case <-ticker.C:
		}
		t := todo[i]
		res, err := xbl.SendFriendRequest(ctx, a.authSession, t.xuid)
		switch {
		case err != nil:
			a.log.Warn("friend request failed", "name", t.name, "xuid", t.xuid, "err", err)
			failures++
		case res.Status == http.StatusTooManyRequests:
			wait := max(res.RetryAfter, time.Minute)
			a.log.Warn("rate limited by xbox live, waiting", "wait", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue // retry the same player
		case res.Status >= 200 && res.Status < 300:
			fmt.Fprintf(added, "%s,%d\n", t.xuid, time.Now().Unix())
			failures = 0
			a.mu.Lock()
			a.sent++
			a.mu.Unlock()
			a.log.Info("sent friend request", "name", t.name, "xuid", t.xuid, "progress", fmt.Sprintf("%d/%d", i+1, len(todo)))
		default:
			a.log.Warn("friend request refused", "name", t.name, "xuid", t.xuid, "status", res.Status, "body", res.Body)
			failures++
		}
		if failures >= maxAddFailures {
			a.log.Error("xbox live refused the last requests in a row, stopping (friend limit reached?)", "remaining", len(todo)-i)
			return
		}
		i++
	}
	a.log.Info("friend add finished", "total", len(todo))
}

// registerFriendAddRoutes adds /friends/add/* to a control server's mux. ctx must be the
// process/broadcast lifetime, not a session generation's - see this file's doc comment.
func registerFriendAddRoutes(ctx context.Context, mux *http.ServeMux, a *friendAdder) {
	mux.HandleFunc("/friends/add/start", func(w http.ResponseWriter, r *http.Request) {
		from := strings.TrimSpace(r.URL.Query().Get("from"))
		if from == "" {
			http.Error(w, "missing ?from=<server address, host gamertag or world name>", http.StatusBadRequest)
			return
		}
		n, err := a.Start(ctx, from)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if n == 0 {
			fmt.Fprintln(w, "nothing to do: everyone recorded from there is already a friend or was already sent a request")
			return
		}
		fmt.Fprintf(w, "started: sending %d friend requests, one per second\n", n)
	})
	mux.HandleFunc("/friends/add/stop", func(w http.ResponseWriter, r *http.Request) {
		a.Stop()
		fmt.Fprintln(w, "stopped")
	})
	mux.HandleFunc("/friends/add/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, a.Status())
	})
}

// readNonEmptyLines returns path's non-empty lines; a missing or unreadable file reads as empty.
func readNonEmptyLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if line := s.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
