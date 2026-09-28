package xbl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/google/uuid"
)

// This file is the joining side of the nonce handshake whose hosting side is
// Session.RefreshNonces: a vanilla host only accepts a Minecraft login that carries the nonce it
// generated for that player's XUID, and it only generates one for a XUID that has joined its
// Xbox Live session as a member. So before dialing a friend's world, a client must
//
//  1. join the host's session through its activity handle (JoinSession),
//  2. wait for the host to publish a nonce for its own XUID in the session's custom properties
//     (JoinedSession.WaitNonce), and
//  3. send that nonce as login.ClientData.Nonce.
//
// Without step 1 the host never issues a nonce, and without step 3 it closes the connection as
// soon as the login arrives - which is exactly what the scraper's -friend mode hit against a
// vanilla world (2026-09-28: WebRTC fully established, then closed ~0.2s later on the first
// Minecraft-level read). The request shapes mirror go-xsapi/v2's mpsd.Client.Join and
// Session.CloseContext (github.com/df-mc/go-xsapi/v2@v2.0.3, mpsd/join.go and mpsd/session.go),
// hand-rolled for the same reason as activity.go: that client needs a full xsapi.Client this
// project does not build.

const joinByHandleURLFmt = "https://sessiondirectory.xboxlive.com/handles/%s/session"

// joinSessionRequest carries only a member entry. Unlike CreateSessionRequest it has no
// properties, since a joining member must not overwrite the host's session properties.
type joinSessionRequest struct {
	Members map[string]*SessionMember `json:"members"`
}

// JoinedSession is our own membership in another host's session, created by JoinSession. Leave
// it when done so the host stops holding a member slot and a nonce for this XUID.
type JoinedSession struct {
	auth *session.Session
	// url is the session document itself, resolved from the join response's Content-Location.
	url string
}

// JoinSession adds xuid as a member of the session behind handleID. connectionID must be the
// RTA connection ID of a live RTA websocket (see RTA.ConnectionID): Xbox Live only treats a
// member as active while the connection it references is, and a host only issues nonces to
// active members.
func JoinSession(ctx context.Context, authSession *session.Session, xuid, connectionID, handleID string) (*JoinedSession, error) {
	auth, err := activityAuthHeader(ctx, authSession)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(joinSessionRequest{
		Members: map[string]*SessionMember{
			"me": {
				Constants: map[string]MemberConstantsSystem{
					"system": {Xuid: xuid, Initialize: true},
				},
				Properties: map[string]MemberPropertiesSystem{
					"system": {
						Active:     true,
						Connection: connectionID,
						Subscription: MemberSubscription{
							ID:          strings.ToUpper(uuid.NewString()),
							ChangeTypes: []string{"everything"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal join request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf(joinByHandleURLFmt, handleID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("join session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("join session: status %d: %s", resp.StatusCode, string(respBody))
	}
	loc := resp.Header.Get("Content-Location")
	if loc == "" {
		return nil, fmt.Errorf("join session: response has no Content-Location header")
	}
	if strings.HasPrefix(loc, "/") {
		loc = "https://sessiondirectory.xboxlive.com" + loc
	}
	return &JoinedSession{auth: authSession, url: loc}, nil
}

// WaitNonce polls the session until the host has published a nonce for xuid, returning it along
// with the session's custom properties at that moment. Those properties should be preferred over
// the ones the activity lookup returned, since a host may update its connection details as
// members join.
func (j *JoinedSession) WaitNonce(ctx context.Context, xuid string, timeout time.Duration) (string, SessionCustomProperties, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		custom, err := j.custom(ctx)
		if err == nil {
			if nonce := custom.Nonces[xuid]; nonce != "" {
				return nonce, custom, nil
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", SessionCustomProperties{}, fmt.Errorf("host published no nonce for %s: %w (last read: %v)", xuid, ctx.Err(), err)
			}
			return "", SessionCustomProperties{}, fmt.Errorf("host published no nonce for %s: %w", xuid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// custom reads the session's current custom properties.
func (j *JoinedSession) custom(ctx context.Context) (SessionCustomProperties, error) {
	auth, err := activityAuthHeader(ctx, j.auth)
	if err != nil {
		return SessionCustomProperties{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return SessionCustomProperties{}, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SessionCustomProperties{}, fmt.Errorf("get session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return SessionCustomProperties{}, fmt.Errorf("get session: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		Properties struct {
			Custom SessionCustomProperties `json:"custom"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return SessionCustomProperties{}, fmt.Errorf("decode session: %w", err)
	}
	return parsed.Properties.Custom, nil
}

// Leave removes our member entry from the session.
func (j *JoinedSession) Leave(ctx context.Context) error {
	auth, err := activityAuthHeader(ctx, j.auth)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(joinSessionRequest{Members: map[string]*SessionMember{"me": nil}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, j.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("leave session: %w", err)
	}
	defer resp.Body.Close()
	// 204 means our leaving emptied and deleted the session, which is fine too.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("leave session: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
