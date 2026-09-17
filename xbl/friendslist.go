package xbl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gameparrot/netherconnect/session"
)

// This file is the "invite my actual Xbox friends" source, a completely separate feature from
// invite_queue.txt/cmd/scraper (see nether2rak's INVITE_FEATURE.md for that one). Where the
// scraper discovers XUIDs by joining an arbitrary third-party server as a live client, this
// reads the host account's own real Xbox Live friends list directly - no scraping, no second
// account, just a normal Xbox Live API call under the same account that's already hosting the
// world.

// peopleResponse is GET https://social.xboxlive.com/users/me/people's response shape. Field
// names/casing verified against Microsoft's own GDK REST reference
// (docs/reference/live/rest/uri/people/uri-usersowneridpeopleget) - the same "social.xboxlive.com"
// host and "me/people" path shape this package's friends.go already calls successfully for
// friendrequests(received), just the base collection instead of the requests sub-resource.
type peopleResponse struct {
	People     []person `json:"people"`
	TotalCount int      `json:"totalCount"`
}

type person struct {
	Xuid              string   `json:"xuid"`
	IsFavorite        bool     `json:"isFavorite"`
	IsFollowingCaller bool     `json:"isFollowingCaller"`
	SocialNetworks    []string `json:"socialNetworks"`
}

// FriendInfo is one entry in the caller's people list, returned by ListFriends.
type FriendInfo struct {
	XUID string
	// Mutual reports whether this is a two-way relationship (the caller follows them AND they
	// follow the caller back) as opposed to a one-way follow. Xbox Live's own "people" list
	// includes both; this project's existing friend-request accept flow (see friends.go) treats
	// any accepted relationship as a friend, so callers that want the same definition should
	// generally not filter on this - it's exposed for callers that want to be stricter.
	Mutual bool
}

// ListFriends fetches the host account's own Xbox Live people ("friends") list. This is a plain,
// direct account query - unlike cmd/scraper, nothing here joins any game server, and unlike
// SendInvite it isn't scoped to any one session.
func ListFriends(ctx context.Context, authSession *session.Session) ([]FriendInfo, error) {
	tok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return nil, fmt.Errorf("request xbl token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://social.xboxlive.com/users/me/people", nil)
	if err != nil {
		return nil, err
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get friends list: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read friends list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get friends list: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed peopleResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse friends list response: %w", err)
	}

	friends := make([]FriendInfo, 0, len(parsed.People))
	for _, p := range parsed.People {
		if p.Xuid == "" {
			continue
		}
		friends = append(friends, FriendInfo{
			XUID:   p.Xuid,
			Mutual: p.IsFollowingCaller,
		})
	}
	return friends, nil
}
