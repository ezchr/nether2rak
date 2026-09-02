package xbl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gameparrot/netherconnect/session"
)

type friendPerson struct {
	Xuid     string `json:"xuid"`
	Gamertag string `json:"gamertag"`
}

type friendRequestResponse struct {
	People []friendPerson `json:"people"`
}

type friendAcceptResponse struct {
	IsFriend bool `json:"isFriend"`
}

// FriendManager accepts incoming Xbox Live friend requests one at a time (queued, so we don't
// slam Xbox's API and get throttled - see "Nether2Rak will now queue friend requests"), and
// explicitly denies/removes ones that fail to accept cleanly instead of leaving them stuck
// (see "Nether2Rak will now deny friend requests if unable to accept, fixing being unable to
// friend").
type FriendManager struct {
	auth *session.Session
	log  *slog.Logger

	client *http.Client
	queue  chan friendPerson
}

func NewFriendManager(authSession *session.Session, log *slog.Logger) *FriendManager {
	f := &FriendManager{auth: authSession, log: log, client: &http.Client{}, queue: make(chan friendPerson, 64)}
	return f
}

// Run processes the accept queue until ctx is cancelled. Start this once in a goroutine.
func (f *FriendManager) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-f.queue:
			f.process(ctx, p)
			time.Sleep(500 * time.Millisecond) // small delay between accepts to avoid rate limiting
		}
	}
}

func (f *FriendManager) authHeader(ctx context.Context) (string, error) {
	tok, err := f.auth.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequest(http.MethodGet, "https://social.xboxlive.com/", nil)
	tok.SetAuthHeader(req)
	return req.Header.Get("Authorization"), nil
}

// CheckPending fetches the current list of incoming friend requests and enqueues them for
// processing. Call this whenever RTA reports IncomingFriendRequestCountChanged.
func (f *FriendManager) CheckPending(ctx context.Context) error {
	auth, err := f.authHeader(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://peoplehub.xboxlive.com/users/me/people/friendrequests(received)", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", "7")
	req.Header.Set("accept-language", "en-GB")

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("get pending friend requests: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed friendRequestResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse pending friend requests: %w", err)
	}
	for _, p := range parsed.People {
		select {
		case f.queue <- p:
		default:
			f.log.Warn("friend accept queue full, dropping request", "xuid", p.Xuid)
		}
	}
	return nil
}

func (f *FriendManager) process(ctx context.Context, p friendPerson) {
	auth, err := f.authHeader(ctx)
	if err != nil {
		f.log.Error("failed to get auth header for friend accept", "err", err)
		return
	}
	url := fmt.Sprintf("https://social.xboxlive.com/users/me/people/friends/v2/xuid(%s)", p.Xuid)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	req.Header.Set("Authorization", auth)

	resp, err := f.client.Do(req)
	if err != nil {
		f.log.Error("friend accept request failed", "xuid", p.Xuid, "err", err)
		f.deny(ctx, p.Xuid)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed friendAcceptResponse
	_ = json.Unmarshal(body, &parsed)

	if !parsed.IsFriend {
		f.log.Warn("could not accept friend request cleanly, denying to clear stuck state", "xuid", p.Xuid, "gamertag", p.Gamertag)
		f.deny(ctx, p.Xuid)
		return
	}
	f.log.Info("accepted friend request", "gamertag", p.Gamertag, "xuid", p.Xuid)
}

// deny removes/clears a friend relationship that failed to establish cleanly, so it doesn't
// sit stuck forever (mirrors the "fixes being unable to friend" devlog entry).
func (f *FriendManager) deny(ctx context.Context, xuid string) {
	auth, err := f.authHeader(ctx)
	if err != nil {
		return
	}
	url := fmt.Sprintf("https://social.xboxlive.com/users/me/people/xuid(%s)", xuid)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	req.Header.Set("Authorization", auth)
	resp, err := f.client.Do(req)
	if err != nil {
		f.log.Error("failed to deny/clear stuck friend request", "xuid", xuid, "err", err)
		return
	}
	_ = resp.Body.Close()
}
