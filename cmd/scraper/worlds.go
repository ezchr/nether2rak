package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
	"golang.org/x/oauth2"
)

// Friends'-worlds mode: what the scraper does when started with no target. Each round it looks up
// every world the account's friends have open on the Friends tab, logs them (host, world name,
// players/limit), then visits each in turn - busiest first - long enough to record who is there.
const (
	// worldStay is how long each world is visited. The full player list arrives right after
	// spawning, so this only needs to cover the join plus a little margin for late PlayerList adds.
	worldStay = 20 * time.Second
	// worldRoundInterval is the pause between rounds, i.e. how often every open world is revisited
	// to catch players who joined since.
	worldRoundInterval = 5 * time.Minute
)

// runFriendWorlds loops over rounds of friendWorlds + visits until ctx ends.
func runFriendWorlds(ctx context.Context, tokSrc oauth2.TokenSource, authSession *session.Session, selfXUID string, seen map[string]bool, out *queueWriter, log *slog.Logger) {
	for {
		worlds, err := friendWorlds(ctx, authSession)
		if err != nil {
			log.Warn("failed to list friends' worlds", "err", err)
		}
		for _, w := range worlds {
			c := w.Custom
			log.Info("friend's world", "host", c.HostName, "world", c.WorldName, "players", c.MemberCount, "maxPlayers", c.MaxMemberCount, "version", c.Version)
		}
		for _, w := range worlds {
			if ctx.Err() != nil {
				return
			}
			c := w.Custom
			if c.MaxMemberCount > 0 && c.MemberCount >= c.MaxMemberCount {
				log.Info("skipping full world", "host", c.HostName, "world", c.WorldName)
				continue
			}
			before := len(seen)
			if err := scrapeWorld(ctx, tokSrc, authSession, selfXUID, w, worldStay, seen, out, log); err != nil && ctx.Err() == nil {
				log.Warn("could not scrape world", "host", c.HostName, "world", c.WorldName, "err", err)
				continue
			}
			log.Info("visited world", "host", c.HostName, "world", c.WorldName, "newPlayers", len(seen)-before)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(worldRoundInterval):
		}
	}
}

// friendWorlds returns every world the account's friends currently have open, busiest first.
func friendWorlds(ctx context.Context, authSession *session.Session) ([]xbl.ActiveSession, error) {
	friends, err := xbl.ListFriends(ctx, authSession)
	if err != nil {
		return nil, fmt.Errorf("list friends: %w", err)
	}
	xuids := make([]string, 0, len(friends))
	for _, f := range friends {
		xuids = append(xuids, f.XUID)
	}
	// Queried in batches: the account auto-accepts friend requests, so its list can be far longer
	// than one request should carry.
	const batch = 100
	var worlds []xbl.ActiveSession
	for i := 0; i < len(xuids); i += batch {
		part, err := xbl.ActivitiesForXUIDs(ctx, authSession, xuids[i:min(i+batch, len(xuids))])
		if err != nil {
			return nil, fmt.Errorf("look up open worlds: %w", err)
		}
		worlds = append(worlds, part...)
	}
	sort.Slice(worlds, func(i, j int) bool { return worlds[i].Custom.MemberCount > worlds[j].Custom.MemberCount })
	return worlds, nil
}
