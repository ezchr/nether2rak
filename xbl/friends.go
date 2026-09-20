package xbl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
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

const (
	// friendCheckMinInterval bounds how close together two pending-request scans can run,
	// whichever triggered them (the periodic timer or an RTA push). Matches
	// go-mcxboxbroadcast's FriendSyncer.friendSyncMinInterval - a real, tested value for how
	// tightly this can poll Xbox Live's PeopleHub endpoint without inviting throttling.
	friendCheckMinInterval = 20 * time.Second
	// friendCheckPollInterval is how often CheckPending runs even with no RTA push at all -
	// the backstop for the push notification silently going quiet after a reconnect (see
	// Run's own doc comment for how this was found).
	friendCheckPollInterval = 5 * time.Minute
)

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

	// Trigger requests an early CheckPending from Run's loop, subject to friendCheckMinInterval
	// spacing. Send to it (non-blocking) from an RTA IncomingFriendRequestCountChanged handler;
	// an unused Trigger just leaves Run on its periodic schedule. Buffered by 1 so a signal
	// arriving while Run is mid-scan is not lost.
	Trigger chan struct{}

	retryAfterMu sync.Mutex
	retryAfter   time.Time
}

func NewFriendManager(authSession *session.Session, log *slog.Logger) *FriendManager {
	f := &FriendManager{
		auth:    authSession,
		log:     log,
		client:  &http.Client{},
		queue:   make(chan friendPerson, 64),
		Trigger: make(chan struct{}, 1),
	}
	return f
}

// Run drives both accepting queued requests and scanning for new ones, until ctx is cancelled.
// Start this once in a goroutine.
//
// The scan side fires once immediately on startup - so a request received entirely while this
// relay was offline is caught up on the moment it comes back, rather than waiting a full
// friendCheckPollInterval - then on Trigger (the RTA push) or every friendCheckPollInterval,
// whichever comes first, the same two-path model as go-mcxboxbroadcast's FriendSyncer.Run.
//
// The periodic fallback exists because the RTA push this used to depend on exclusively was
// found, in real operation, to silently stop arriving after some reconnects: three real pending
// friend requests were found sitting unprocessed with zero related log output, despite the
// friends-feed resubscribe in rta.go's handle() looking correct by inspection. A push going
// silent leaves nothing to trigger a check ever again under the old design, so this no longer
// depends on it being reliable - Trigger is now an optimization for the common case, not the
// only path.
func (f *FriendManager) Run(ctx context.Context) {
	go f.runQueue(ctx)

	timer := time.NewTimer(0) // fires immediately: catch up on anything pending from before startup
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-f.Trigger:
		}
		if ctx.Err() != nil {
			return
		}

		if wait := f.waitBeforeCheck(); wait > 0 {
			timer.Reset(wait)
			continue
		}

		if err := f.checkPending(ctx); err != nil {
			f.log.Error("failed to check pending friend requests", "err", err)
		}
		timer.Reset(friendCheckPollInterval)
	}
}

// waitBeforeCheck returns how long to wait before the next scan is allowed, honouring any
// server-requested Retry-After from a prior 429.
func (f *FriendManager) waitBeforeCheck() time.Duration {
	f.retryAfterMu.Lock()
	defer f.retryAfterMu.Unlock()
	if remaining := time.Until(f.retryAfter); remaining > 0 {
		return remaining
	}
	return 0
}

func (f *FriendManager) setRetryAfter(d time.Duration) {
	if d <= 0 {
		d = friendCheckMinInterval
	}
	f.retryAfterMu.Lock()
	f.retryAfter = time.Now().Add(d)
	f.retryAfterMu.Unlock()
}

// runQueue processes the accept queue until ctx is cancelled.
func (f *FriendManager) runQueue(ctx context.Context) {
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
// processing. Safe to call directly (e.g. from an RTA event handler); it shares the same
// rate-limit budget as Run's own periodic scans via waitBeforeCheck/setRetryAfter.
func (f *FriendManager) CheckPending(ctx context.Context) error {
	return f.checkPending(ctx)
}

func (f *FriendManager) checkPending(ctx context.Context) error {
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

	if resp.StatusCode == http.StatusTooManyRequests {
		f.setRetryAfter(parseRetryAfter(resp.Header.Get("Retry-After")))
		return fmt.Errorf("get pending friend requests: rate limited (status %d)", resp.StatusCode)
	}

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

	if resp.StatusCode == http.StatusTooManyRequests {
		f.setRetryAfter(parseRetryAfter(resp.Header.Get("Retry-After")))
		f.log.Warn("friend accept rate limited, will retry on next scan", "xuid", p.Xuid, "gamertag", p.Gamertag)
		return
	}

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

// parseRetryAfter parses a Retry-After header value in either seconds or HTTP-date form, the
// same convention df-mc/go-xsapi/v2/social's own error.go parses for the same Xbox Live APIs -
// mirroring real, tested parsing rather than inventing a different one for PeopleHub/
// social.xboxlive.com specifically.
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := time.Until(when)
	if delay < 0 {
		return 0
	}
	return delay
}
