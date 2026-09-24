// Command pruneinactive lists (and, with -delete, removes) friends who have not joined this
// relay's worlds within a configurable window, using the join history friendactivity.Store
// records from bridge/relay.go on every real world entry.
//
// This is deliberately a separate, manually-run command rather than something the relay does on
// its own on a timer: removing a friend is a real, one-way Xbox Live action, and someone with a
// long break between sessions (a vacation, a busy month) is a real, sympathetic case that an
// unattended job has no way to distinguish from someone who is genuinely gone. Run this by hand,
// review the -delete=false listing first, then re-run with -delete once you're sure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl/friendactivity"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

var deviceAuth = auth.AndroidConfig

func main() {
	activityPath := flag.String("activity", "friend_activity.txt", "path to the friend activity file written by the relay")
	tokenPath := flag.String("token", "token.json", "path to the relay's cached token.json")
	window := flag.Duration("older-than", 60*24*time.Hour, "minimum time since last join to count as inactive")
	del := flag.Bool("delete", false, "actually remove listed friends from Xbox Live (default: list only)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx := context.Background()

	store, err := friendactivity.Open(*activityPath, log)
	if err != nil {
		log.Error("open friend activity file", "path", *activityPath, "err", err)
		os.Exit(1)
	}

	stale := store.Stale(*window)
	if len(stale) == 0 {
		fmt.Printf("no friends inactive for longer than %s\n", *window)
		return
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].LastJoin.Before(stale[j].LastJoin) })

	fmt.Printf("%d friend(s) inactive for longer than %s:\n", len(stale), *window)
	for _, rec := range stale {
		fmt.Printf("  %-20s xuid=%-20s last joined %s ago (%s)\n",
			rec.DisplayName, rec.XUID, time.Since(rec.LastJoin).Round(time.Hour), rec.LastJoin.Format(time.RFC3339))
	}

	// A friend the store has never recorded a join for at all - added before this feature
	// existed, or a real friend who genuinely never joined - is not in Stale at all (see its own
	// doc comment for why), so it is never a -delete candidate here. Worth surfacing anyway, as
	// information only, since it is a real gap in what this tool can judge.
	if !*del {
		fmt.Println("\nre-run with -delete to remove these from Xbox Live.")
		return
	}

	tok := new(oauth2.Token)
	b, err := os.ReadFile(*tokenPath)
	if err != nil {
		log.Error("read token file", "path", *tokenPath, "err", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(b, tok); err != nil {
		log.Error("parse token file", "err", err)
		os.Exit(1)
	}
	src := deviceAuth.RefreshTokenSource(tok)

	authSession, err := session.SessionFromTokenSource(src, deviceAuth, ctx)
	if err != nil {
		log.Error("start auth session", "err", err)
		os.Exit(1)
	}

	client := &http.Client{}
	removed := 0
	for _, rec := range stale {
		if err := removeFriend(ctx, authSession, client, rec.XUID); err != nil {
			log.Error("failed to remove friend", "displayName", rec.DisplayName, "xuid", rec.XUID, "err", err)
			continue
		}
		store.Forget(rec.XUID)
		log.Info("removed inactive friend", "displayName", rec.DisplayName, "xuid", rec.XUID,
			"lastJoin", rec.LastJoin.Format(time.RFC3339))
		removed++
		time.Sleep(500 * time.Millisecond) // same spacing FriendManager uses between mutations
	}
	fmt.Printf("removed %d of %d listed friend(s)\n", removed, len(stale))
}

func removeFriend(ctx context.Context, authSession *session.Session, client *http.Client, xuid string) error {
	xstsTok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return fmt.Errorf("get xbox live token: %w", err)
	}
	url := fmt.Sprintf("https://social.xboxlive.com/users/me/people/xuid(%s)", xuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	xstsTok.SetAuthHeader(req)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete request: status %s: %s", resp.Status, body)
	}
	return nil
}
