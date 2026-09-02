package xbl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gameparrot/netherconnect/session"
)

const userPresenceURLFmt = "https://userpresence.xboxlive.com/users/xuid(%s)/devices/current/titles/current"

// Presence keeps the account marked as actively playing Minecraft on Xbox Live.
//
// This is a SEPARATE API from the session directory, and it is not optional: creating a
// session document succeeds fine without it, but the account shows as offline and the world
// never surfaces on anyone's Friends tab. The Friends tab is driven by presence ("what are my
// friends playing right now"), and only then resolves the session attached to that presence.
//
// Xbox Live expects this to be re-sent periodically. The response carries an X-Heartbeat-After
// header (seconds) telling us when to send the next one; we honour it, defaulting to 300s.
// Ported from SessionManagerCore.updatePresence().
type Presence struct {
	auth *session.Session
	xuid string
	log  *slog.Logger

	client *http.Client
}

func NewPresence(authSession *session.Session, xuid string, log *slog.Logger) *Presence {
	return &Presence{auth: authSession, xuid: xuid, log: log, client: &http.Client{}}
}

// update sends a single presence heartbeat and returns how long to wait before the next one.
func (p *Presence) update(ctx context.Context) (time.Duration, error) {
	const defaultHeartbeat = 300 * time.Second

	tok, err := p.auth.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return defaultHeartbeat, fmt.Errorf("request xbl token for presence: %w", err)
	}
	url := fmt.Sprintf(userPresenceURLFmt, p.xuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"state": "active"}`))
	if err != nil {
		return defaultHeartbeat, err
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-xbl-contract-version", "3")

	resp, err := p.client.Do(req)
	if err != nil {
		return defaultHeartbeat, fmt.Errorf("post presence: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	heartbeatHeader := resp.Header.Get("X-Heartbeat-After")

	// Log the raw outcome. Xbox Live can return 200 while still not surfacing the account as
	// online, so the status code alone is not proof this worked - the presence of an
	// X-Heartbeat-After header is a much better signal that Xbox actually registered a live
	// session for this device/title. If that header is missing, treat the result as suspect.
	p.log.Debug("presence response",
		"status", resp.StatusCode,
		"xHeartbeatAfter", heartbeatHeader,
		"body", strings.TrimSpace(string(body)),
	)

	if resp.StatusCode != http.StatusOK {
		return defaultHeartbeat, fmt.Errorf("post presence: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if heartbeatHeader == "" {
		p.log.Warn("presence accepted but Xbox Live returned no X-Heartbeat-After header; the account may still show as offline",
			"status", resp.StatusCode)
	}

	next := defaultHeartbeat
	if heartbeatHeader != "" {
		if secs, err := strconv.Atoi(heartbeatHeader); err == nil && secs > 0 {
			next = time.Duration(secs) * time.Second
		}
	}
	return next, nil
}

// Run sends an initial presence heartbeat and then keeps re-sending it on the interval Xbox
// Live asks for, until ctx is cancelled. Returns an error only if the FIRST heartbeat fails,
// since that means the world will not appear at all; later failures are logged and retried.
func (p *Presence) Run(ctx context.Context) error {
	next, err := p.update(ctx)
	if err != nil {
		return err
	}
	p.log.Info("xbox live presence set to active", "nextHeartbeat", next)

	go func() {
		for {
			timer := time.NewTimer(next)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			d, err := p.update(ctx)
			if err != nil {
				p.log.Error("failed to update presence, retrying in 60s", "err", err)
				next = 60 * time.Second
				continue
			}
			p.log.Debug("presence heartbeat sent", "nextHeartbeat", d)
			next = d
		}
	}()
	return nil
}
