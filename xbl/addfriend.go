package xbl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gameparrot/netherconnect/session"
)

// AddFriendResult is the outcome of one SendFriendRequest call.
type AddFriendResult struct {
	// Status is the HTTP status Xbox Live answered with; 2xx means the request was sent (or,
	// if they had already requested us, accepted).
	Status int
	// RetryAfter is set when Status is 429: how long Xbox Live asked us to wait.
	RetryAfter time.Duration
	// Body is the response body, kept for logging failures.
	Body string
}

// SendFriendRequest adds xuid as a friend. It is the same call FriendManager uses to accept an
// incoming request; for someone who has not requested us, it sends them a request instead.
func SendFriendRequest(ctx context.Context, authSession *session.Session, xuid string) (AddFriendResult, error) {
	tok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return AddFriendResult{}, fmt.Errorf("request xbl token: %w", err)
	}
	url := fmt.Sprintf("https://social.xboxlive.com/users/me/people/friends/v2/xuid(%s)", xuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return AddFriendResult{}, err
	}
	tok.SetAuthHeader(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return AddFriendResult{}, fmt.Errorf("send friend request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	res := AddFriendResult{Status: resp.StatusCode, Body: string(body)}
	if resp.StatusCode == http.StatusTooManyRequests {
		res.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return res, nil
}
