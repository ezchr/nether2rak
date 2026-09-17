// Command scraper joins a Bedrock server as a real, authenticated Xbox Live client and records
// the real XUID of every player it ever sees on that server's player list, for as long as it
// stays connected - not just whoever happens to be on at the moment it joins.
//
// It is the "who do we invite" half of the invite-everyone-from-another-server feature. The
// other half - actually sending Xbox Live game invites - lives in nether2rak itself
// (invitewatcher.go), running under a completely different, dedicated Xbox account: the one
// that's actually hosting the relay's Friends-tab world.
//
// Those two things are split into separate processes/binaries and separate accounts
// deliberately. Sending an invite requires being the account that owns the destination session -
// there's no way around that, Xbox Live checks it. But discovering who's on some arbitrary
// third-party server means actually joining that server, on a loop, as a live game client -
// which is a materially different and riskier thing for an account to be doing than quietly
// hosting a world. Doing both from the same account would mean one Xbox identity simultaneously
// running a public world AND constantly joining strangers' servers; keeping them apart means a
// problem with one (a ban, a flag, a crash) can't take out the other.
//
// The two halves communicate through a single shared file (see nether2rak's
// invitewatcher.go for the reader side): this process appends one newly-seen XUID per line;
// nether2rak tails that file and sends the actual invites.
//
// Usage:
//
//	./scraper <ip:port>
//
// Runs until interrupted (Ctrl+C) or the target connection drops, at which point it reconnects
// and keeps recording - matching "register all the players in a server the scraper is in" for as
// long as it's pointed at that server, not a single snapshot.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gameparrot/netherconnect/invitequeue"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

const tokenCacheFile = "token.json"

// outputFile is where discovered XUIDs are appended. Relative to cwd, matching this whole
// project's convention (config.json, token.json) of a run directory holding everything one
// instance needs - run this binary from its own directory, separate from any hosting instance's.
const outputFile = "invite_queue.txt"

// reconnectDelay is how long to wait before retrying after a dropped connection (target server
// restart, network blip, kick). Short enough that a brief interruption doesn't lose much
// scraping time; long enough not to hammer a server that's actually down.
const reconnectDelay = 10 * time.Second

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <ip:port>\n", os.Args[0])
		os.Exit(1)
	}
	address := os.Args[1]

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tokSrc, err := tokenSource(log)
	if err != nil {
		log.Error("failed to authenticate", "err", err)
		os.Exit(1)
	}

	// Prune before the first read AND before opening the write handle, so a long-stopped
	// scraper does not mean 30-day-old entries survive indefinitely just because this
	// process happened to be off - expiry must not depend on either this process or
	// invitewatcher being up (see invitequeue's own package doc for why this lives in a
	// shared package instead of either side's own code).
	invitequeue.Prune(outputFile, log)

	out, err := newQueueWriter(outputFile)
	if err != nil {
		log.Error("failed to open output file", "path", outputFile, "err", err)
		os.Exit(1)
	}
	defer out.Close()

	// seen persists across reconnects to the same run of this process, so a restart after a
	// drop doesn't re-append XUIDs already recorded - only genuinely new ones get written.
	seen := loadAlreadySeen(outputFile, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runPeriodicPrune(ctx, out, log)

	log.Info("scraper starting", "target", address, "alreadyKnown", len(seen))
	for {
		if ctx.Err() != nil {
			return
		}
		if err := scrapeOnce(ctx, address, tokSrc, seen, out, log); err != nil && ctx.Err() == nil {
			log.Warn("connection ended, reconnecting", "err", err, "after", reconnectDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// scrapeOnce holds one connection to address open until it ends (server closes it, ctx is
// cancelled, or a read error occurs), recording every new XUID that appears on the player list
// for as long as it's connected.
func scrapeOnce(ctx context.Context, address string, tokSrc oauth2.TokenSource, seen map[string]bool, out *queueWriter, log *slog.Logger) error {
	dialCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	dialer := minecraft.Dialer{
		TokenSource: tokSrc,
		ErrorLog:    log,
	}
	conn, err := dialer.DialContext(dialCtx, "raknet", address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()

	// DoSpawn completes the join sequence (resource packs, start game, etc.) - PlayerList
	// entries for players already on the server arrive as part of this, and further Add/Remove
	// entries keep streaming for the lifetime of the connection afterward.
	spawnCtx, cancelSpawn := context.WithTimeout(ctx, 120*time.Second)
	defer cancelSpawn()
	if err := conn.DoSpawnContext(spawnCtx); err != nil {
		return fmt.Errorf("spawn on %s: %w", address, err)
	}
	log.Info("joined target server, recording players", "target", address)

	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			return fmt.Errorf("read packet: %w", err)
		}
		list, ok := pk.(*packet.PlayerList)
		if !ok {
			continue
		}
		for _, entry := range list.Entries {
			// ActionType moved from the packet onto each entry in gophertunnel
			// v1.62.0, so add/remove is now filtered per entry rather than per packet.
			if entry.ActionType != protocol.PlayerListActionAdd {
				continue
			}
			if entry.XUID == "" || seen[entry.XUID] {
				continue
			}
			seen[entry.XUID] = true
			// Format is "xuid,username,discoveredUnixSeconds" - the username is purely for a
			// human reading the file; invitewatcher.go only reads the part before the first
			// comma for the xuid. invitequeue.Prune separately parses the third field to expire
			// entries older than invitequeue.Expiry. Username is whatever the target server
			// itself sent (Bedrock display name, not a verified Xbox gamertag), so treat it as
			// informational only, and strip any stray comma so the xuid/timestamp fields stay
			// at fixed positions even if a display name somehow contains one.
			display := strings.ReplaceAll(entry.Username, ",", "")
			line := fmt.Sprintf("%s,%s,%d", entry.XUID, display, time.Now().Unix())
			if err := out.append(line); err != nil {
				log.Error("failed to write discovered xuid", "xuid", entry.XUID, "err", err)
				continue
			}
			log.Info("discovered player", "xuid", entry.XUID, "username", entry.Username)
		}
	}
}

// loadAlreadySeen reads back whatever this or a previous run already wrote, so a restart doesn't
// duplicate entries in the output file (harmless for nether2rak's own reader, which dedupes on
// its side too, but wasteful and noisy otherwise).
func loadAlreadySeen(path string, log *slog.Logger) map[string]bool {
	seen := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return seen
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// "xuid,username" - only the xuid half matters for dedup. Falls back to treating the
		// whole line as the xuid for output files written before this format existed, so an
		// old-format file left over from a prior run of this binary doesn't get re-scraped.
		xuid, _, _ := strings.Cut(line, ",")
		seen[xuid] = true
	}
	if err := scanner.Err(); err != nil {
		log.Warn("error reading existing output file", "err", err)
	}
	return seen
}

// tokenSource performs (or reloads a cached) Microsoft device-code login, matching main.go's own
// tokenSource exactly - see its doc comment. Deliberately its own separate token.json, never
// shared with any hosting instance's: see this file's package doc for why.
func tokenSource(log *slog.Logger) (oauth2.TokenSource, error) {
	cachePath := filepath.Join(".", tokenCacheFile)

	if b, err := os.ReadFile(cachePath); err == nil {
		tok := new(oauth2.Token)
		if err := json.Unmarshal(b, tok); err == nil {
			src := auth.AndroidConfig.RefreshTokenSource(tok)
			if _, err := src.Token(); err == nil {
				return &cachingTokenSource{src: src, path: cachePath}, nil
			}
			log.Warn("cached token expired, need to sign in again")
		}
	}

	fmt.Println("Signing in with a Microsoft account - this should be a DIFFERENT account from any")
	fmt.Println("account currently hosting a nether2rak world. This account will be the one joining")
	fmt.Println("target servers to scrape their player list.")
	tok, err := auth.AndroidConfig.RequestLiveTokenWriter(os.Stdout)
	if err != nil {
		return nil, fmt.Errorf("request live token: %w", err)
	}
	src := auth.AndroidConfig.RefreshTokenSource(tok)
	return &cachingTokenSource{src: src, path: cachePath}, nil
}

type cachingTokenSource struct {
	src  oauth2.TokenSource
	path string
}

func (c *cachingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := c.src.Token()
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(c.path, b, 0600)
	}
	return tok, nil
}

// queueWriter is a mutex-guarded *os.File for outputFile, reopened whenever invitequeue.Prune
// rewrites the underlying file out from under an already-open append handle.
//
// invitequeue.Prune replaces the file via a temp-file-plus-rename (see its own doc comment) so a
// concurrent reader never sees a half-written file - but that same rename is exactly what breaks
// an O_APPEND handle opened before the rename: the old handle keeps writing to the now-detached
// original inode, which is invisible under the file's real name from that point on. Since this
// scraper prunes on startup AND on a daily ticker while running (see runPeriodicPrune), the
// write handle has to be able to survive a prune happening mid-run, not just once before the
// scrape loop starts.
type queueWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func newQueueWriter(path string) (*queueWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &queueWriter{path: path, f: f}, nil
}

// append writes one already-formatted line (without its trailing newline) to the queue file.
func (w *queueWriter) append(line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := fmt.Fprintln(w.f, line); err != nil {
		return err
	}
	return w.f.Sync()
}

// reopen closes the current handle and opens a fresh one for the same path, picking up whatever
// file currently exists under that name - specifically, the file invitequeue.Prune just renamed
// into place. Call this immediately after every Prune call, not just at startup.
func (w *queueWriter) reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.f.Close()
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.f = f
	return nil
}

func (w *queueWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// runPeriodicPrune prunes outputFile once a day for as long as ctx is alive, reopening qw's
// write handle after each prune so appends keep landing in the real, current file rather than a
// detached inode left behind by the rename inside invitequeue.Prune. Runs in its own goroutine;
// returns when ctx is cancelled.
func runPeriodicPrune(ctx context.Context, qw *queueWriter, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			invitequeue.Prune(outputFile, log)
			if err := qw.reopen(); err != nil {
				log.Error("failed to reopen output file after periodic prune", "path", outputFile, "err", err)
			}
		}
	}
}
