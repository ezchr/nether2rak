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
// The two halves communicate through files (see nether2rak's invitewatcher.go for the reader
// side): this process appends one newly-seen XUID per line to server_players.txt or
// world_players.txt; a batch of those lines, copied into a relay's run directory as
// invite_queue.txt, is what that relay sends the actual invites to.
//
// Usage:
//
//	./scraper <ip:port>             scrape one server by address
//	./scraper -friend <gamertag>    stay in one friend's open world
//	./scraper                       visit every friend's open world in turn (see worlds.go)
//
// Runs until interrupted (Ctrl+C) or the target connection drops, at which point it reconnects
// and keeps recording - matching "register all the players in a server the scraper is in" for as
// long as it's pointed at that server, not a single snapshot.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

const tokenCacheFile = "token.json"

// Discovered XUIDs are appended to one of two files, depending on where they were seen: players
// from servers joined by address go to serverPlayersFile (tagged with that address), players from
// friends' Friends-tab worlds go to worldPlayersFile (tagged with world name and host). Both are
// relative to cwd, matching this whole project's convention (config.json, token.json) of a run
// directory holding everything one instance needs.
const (
	serverPlayersFile = "server_players.txt"
	worldPlayersFile  = "world_players.txt"
)

// defaultReconnectDelay is how long to wait before retrying after a dropped connection (target
// server restart, network blip, kick). Short enough that a brief interruption doesn't lose much
// scraping time; long enough not to hammer a server that's actually down. Overridable via
// -reconnect-delay: confirmed 2026-09-17 that rapid-fire NetherNet offers against a real
// Bedrock client's Friends-tab world can go repeatedly unanswered after an initial successful
// handshake, consistent with some form of client-side throttling/dedup on repeated connection
// attempts from the same signaling identity - a longer delay is a cheap thing to try first.
const defaultReconnectDelay = 10 * time.Second

func main() {
	friendTarget := flag.String("friend", "", "Gamertag or XUID of a friend to scrape from their currently open Friends-tab world, instead of a direct-IP target")
	reconnectDelay := flag.Duration("reconnect-delay", defaultReconnectDelay, "How long to wait between reconnect attempts after a dropped or failed connection")
	flag.Parse()

	var address string
	if *friendTarget == "" && flag.NArg() > 0 {
		address = flag.Arg(0)
	}
	// No target at all means friends'-worlds mode: every friend's open world, in turn.
	friendWorldsMode := *friendTarget == "" && address == ""

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Created here, ahead of everything else that needs to run for the process's lifetime
	// (presence's heartbeat goroutine below, runPeriodicPrune, the main scrape loop), rather than
	// right before the scrape loop as before - presence.Run needs a real, long-lived ctx to tie
	// its heartbeat goroutine to, and that has to exist before the -friend setup block runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokSrc, err := tokenSource(log)
	if err != nil {
		log.Error("failed to authenticate", "err", err)
		os.Exit(1)
	}

	// Only constructed for -friend and friends'-worlds mode: it is a real Xbox Live device-auth login (see
	// session.SessionFromTokenSource), not free, and scrapeOnce's plain direct-IP path has never
	// needed it - nether2rak's own main.go is the only other place in this project that builds
	// one of these today.
	var authSession *session.Session
	var selfXUID string
	friendXUID := *friendTarget
	if *friendTarget != "" || friendWorldsMode {
		authSession, err = session.SessionFromTokenSource(tokSrc, auth.AndroidConfig, context.Background())
		if err != nil {
			log.Error("failed to establish xbox live session for friend lookup", "err", err)
			os.Exit(1)
		}

		// This account (e.g. ezchr) needs its OWN Xbox Live presence set to "active" before a
		// friend's session will show it any real connection info - nether2rak's own main.go
		// requires the same thing for the account it uses to host (see xbl.Presence's doc
		// comment: "creating a session document does not itself mark the account active", and
		// without it the account doesn't even show online on Xbox, let alone see friends'
		// SupportedConnections). This scraper never needed presence before because scrapeOnce's
		// direct-IP path doesn't go through the session directory at all - only -friend mode
		// does, via xbl.ActivitiesForXUIDs.
		selfXBLTok, err := authSession.RequestXBLToken(context.Background(), "http://xboxlive.com")
		if err != nil {
			log.Error("failed to get own xbox live identity for presence", "err", err)
			os.Exit(1)
		}
		selfXUID = selfXBLTok.AuthorizationToken.DisplayClaims.UserInfo[0].XUID
		presence := xbl.NewPresence(authSession, selfXUID, log)
		if err := presence.Run(ctx); err != nil {
			log.Error("failed to set xbox live presence", "err", err)
			os.Exit(1)
		}

		// -friend accepts either form: a real XUID is a plain decimal string, so anything that
		// doesn't parse as one is treated as a gamertag and resolved first. This means a
		// gamertag that happens to be all digits (unusual but not impossible) would be
		// misread as an XUID - acceptable, since a real XUID and an all-digit gamertag look
		// identical on the wire with no way to distinguish them from the string alone.
		if _, numErr := strconv.ParseUint(friendXUID, 10, 64); friendXUID != "" && numErr != nil {
			resolved, resolveErr := xbl.XUIDForGamertag(context.Background(), authSession, friendXUID)
			if resolveErr != nil {
				log.Error("failed to resolve gamertag to xuid", "gamertag", friendXUID, "err", resolveErr)
				os.Exit(1)
			}
			log.Info("resolved gamertag to xuid", "gamertag", friendXUID, "xuid", resolved)
			friendXUID = resolved
		}
	}

	// Prune before the first read AND before opening the write handle, so a long-stopped
	// scraper does not mean 30-day-old entries survive indefinitely just because this
	// process happened to be off - expiry must not depend on either this process or
	// invitewatcher being up (see invitequeue's own package doc for why this lives in a
	// shared package instead of either side's own code).
	outputFile := worldPlayersFile
	if address != "" {
		outputFile = serverPlayersFile
	}
	pruneExpired(outputFile, log)

	out, err := newQueueWriter(outputFile)
	if err != nil {
		log.Error("failed to open output file", "path", outputFile, "err", err)
		os.Exit(1)
	}
	defer out.Close()

	// seen persists across reconnects to the same run of this process, so a restart after a
	// drop doesn't re-append XUIDs already recorded - only genuinely new ones get written.
	seen := loadAlreadySeen(outputFile, log)
	if selfXUID != "" {
		seen[selfXUID] = true // the bot shows up on every player list it reads; never record it
	}

	go runPeriodicPrune(ctx, out, log)

	// -friend mode needs the target to actually be a friend before their session's connection
	// info is visible at all (Xbox Live's default readRestriction on a session is "followed" -
	// see xbl.DefaultSessionSystemProperties - so a stranger's ActivitiesForXUIDs lookup finds
	// the session but SupportedConnections comes back empty). Auto-accepting incoming requests
	// here means a friend-join target only has to send one, not also get a human to click
	// accept - the same xbl.FriendManager nether2rak's own main.go already runs for its hosting
	// account, reused here with polling instead of RTA (an RTA connection is real additional
	// setup this scraper has no other use for; CheckPending is a plain, idempotent GET that
	// costs nothing extra to call on a timer with nothing pending).
	if authSession != nil {
		friendMgr := xbl.NewFriendManager(authSession, log)
		go friendMgr.Run(ctx)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				if err := friendMgr.CheckPending(ctx); err != nil {
					log.Warn("failed to check pending friend requests", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	if friendWorldsMode {
		log.Info("scraper starting", "mode", "friends' worlds", "alreadyKnown", len(seen))
		runFriendWorlds(ctx, tokSrc, authSession, selfXUID, seen, out, log)
		return
	}
	if friendXUID != "" {
		log.Info("scraper starting", "mode", "friend", "friendXuid", friendXUID, "alreadyKnown", len(seen))
	} else {
		log.Info("scraper starting", "mode", "direct-ip", "target", address, "alreadyKnown", len(seen))
	}
	for {
		if ctx.Err() != nil {
			return
		}
		var runErr error
		if friendXUID != "" {
			runErr = scrapeFriendOnce(ctx, tokSrc, authSession, selfXUID, friendXUID, seen, out, log)
		} else {
			runErr = scrapeOnce(ctx, address, tokSrc, seen, out, log)
		}
		if runErr != nil && ctx.Err() == nil {
			log.Warn("connection ended, reconnecting", "err", runErr, "after", *reconnectDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*reconnectDelay):
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
	return recordPlayerList(ctx, conn, csvField(address), seen, out, log)
}

// recordPlayerList reads packets from conn until it errors (connection drop, ctx cancellation),
// recording every newly-seen XUID from PlayerList add entries. Shared between direct-IP scraping
// (scrapeOnce) and friend-session scraping (scrapeFriendOnce, scraper_friend.go) - both join a
// real *minecraft.Conn by different paths (raknet dial vs. resolved NetherNet session) but
// record players identically from that point on.
// source is the already-joined trailing field(s) of each line, saying where the player was seen:
// the server address, or world name and host (see csvField).
func recordPlayerList(ctx context.Context, conn *minecraft.Conn, source string, seen map[string]bool, out *queueWriter, log *slog.Logger) error {
	// ReadPacket does not watch ctx, so without this a stop signal left the process blocked here
	// indefinitely and the callers' cleanup (e.g. leaving a friend's Xbox Live session) never ran.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
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
			// Format is "xuid,username,source..." - invitewatcher.go only reads the part before
			// the first comma for the xuid; the rest is for a human reading the file. The
			// discovery time goes to the companion file instead (see expiry.go). Username is
			// whatever the target server itself sent (Bedrock display name, not a verified Xbox
			// gamertag), so treat it as informational only; commas are stripped so every field
			// stays at a fixed position.
			line := fmt.Sprintf("%s,%s,%s", entry.XUID, csvField(entry.Username), source)
			if err := out.append(entry.XUID, line); err != nil {
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

// queueWriter is a mutex-guarded pair of append handles: the player file and its hidden
// companion file of discovery times (see expiry.go). Both are reopened whenever pruneExpired
// rewrites them out from under the already-open handles.
//
// pruneExpired replaces each file via a temp-file-plus-rename so a concurrent reader never sees a
// half-written file - but that same rename is exactly what breaks an O_APPEND handle opened before
// the rename: the old handle keeps writing to the now-detached original inode, which is invisible
// under the file's real name from that point on. Since this scraper prunes on startup AND on a
// daily ticker while running (see runPeriodicPrune), the write handles have to be able to survive
// a prune happening mid-run, not just once before the scrape loop starts.
type queueWriter struct {
	mu    sync.Mutex
	path  string
	f     *os.File
	seenF *os.File
}

func newQueueWriter(path string) (*queueWriter, error) {
	w := &queueWriter{path: path}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *queueWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	seenF, err := os.OpenFile(seenPath(w.path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.seenF = f, seenF
	return nil
}

// append writes one already-formatted player line (without its trailing newline) and records
// xuid's discovery time in the companion file.
func (w *queueWriter) append(xuid, line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := fmt.Fprintln(w.f, line); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.seenF, "%s,%d\n", xuid, time.Now().Unix()); err != nil {
		return err
	}
	if err := w.seenF.Sync(); err != nil {
		return err
	}
	return w.f.Sync()
}

// reopen closes the current handles and opens fresh ones for the same paths, picking up whatever
// files currently exist under those names - specifically, the ones pruneExpired just renamed into
// place. Call this immediately after every prune, not just at startup.
func (w *queueWriter) reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.f.Close()
	_ = w.seenF.Close()
	return w.open()
}

func (w *queueWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.seenF.Close()
	return w.f.Close()
}

// csvField makes s safe as one field of an output line: no commas (the field separator) and no
// line breaks. World names are player-chosen and can contain either.
func csvField(s string) string {
	return strings.NewReplacer(",", "", "\n", " ", "\r", " ").Replace(s)
}

// runPeriodicPrune prunes the output file once a day for as long as ctx is alive, reopening qw's
// write handles after each prune so appends keep landing in the real, current files rather than
// detached inodes left behind by the renames inside pruneExpired. Runs in its own goroutine;
// returns when ctx is cancelled.
func runPeriodicPrune(ctx context.Context, qw *queueWriter, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneExpired(qw.path, log)
			if err := qw.reopen(); err != nil {
				log.Error("failed to reopen output file after periodic prune", "path", qw.path, "err", err)
			}
		}
	}
}
