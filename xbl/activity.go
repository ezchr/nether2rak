package xbl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gameparrot/netherconnect/session"
)

// This file is the "join a friend's active world" lookup, the missing piece between
// xbl.ListFriends (who is a friend) and actually dialing their session over NetherNet.
//
// It deliberately does NOT use go-xsapi/v2's mpsd.Client for this, even though that package is
// already a pinned dependency and has a real ActivitiesForUsers/Join pair that does exactly
// this. mpsd.Client authenticates its outgoing requests via its *http.Client's own transport
// (an xsapi.Client acting as a RoundTripper - see go-xsapi/v2's own client.go), which means
// building one for real needs a full xsapi.Client, which needs a TokenSource satisfying both
// xsts.TokenSource and xasd.TokenSource - a real Xbox device-auth flow, not just the
// already-fetched *auth.XBLToken this project's session.Session already holds. That is the
// same architectural gap the aborted gophertunnel-upgrade investigation hit earlier this
// project's history and deferred; reusing it here would mean solving it as a prerequisite to a
// single read-only lookup call.
//
// ActivitiesForUsers's own real implementation, read directly from the dependency, is a plain
// POST with a JSON body and an Authorization header - the exact same shape as every other
// direct HTTP call already hand-rolled in this package (see invite.go, session.go's
// authHeader). Doing the same here avoids the xsapi.Client detour entirely for one endpoint.

// activityHandlesURL is the real Xbox Live endpoint for querying open multiplayer session
// ("activity") handles - same host/path family as CreateSessionRequest's handle creation and
// InviteRequest's invite handles, a different query shape.
const activityHandlesURL = "https://sessiondirectory.xboxlive.com/handles/query?include=relatedInfo,customProperties"

// activitySearchRequest is POSTed to activityHandlesURL. Field shape verified against
// go-xsapi/v2's own mpsd.activitySearchRequest (github.com/df-mc/go-xsapi/v2@v2.0.3,
// mpsd/activity.go) - a real, independently-working implementation of the same endpoint -
// rather than guessed from Microsoft's REST docs directly.
type activitySearchRequest struct {
	Type            string              `json:"type"`
	ServiceConfigID string              `json:"scid"`
	Owners          activitySearchOwner `json:"owners"`
}

type activitySearchOwner struct {
	XUIDs []string `json:"xuids"`
}

// ActiveSession is one open multiplayer session an ActivitiesForXUIDs query found for the
// requested XUIDs. Ref identifies the session for a later mpsd Join-style operation; Custom
// carries the same connection info (RakNetGUID/HostIpAddress/HostPort/NetherNetId/PmsgId) this
// project's own hosting code publishes via SessionCustomProperties, since the endpoint being
// queried here is the read side of the exact same session document CreateSessionRequest writes.
type ActiveSession struct {
	Ref    SessionRef              `json:"sessionRef"`
	Custom SessionCustomProperties `json:"customProperties"`
}

// activityHandleResponse is the raw shape of one element in handles/query's "results" array -
// only the fields this package actually reads are declared; the real response has more.
type activityHandleResponse struct {
	SessionRef       SessionRef              `json:"sessionRef"`
	CustomProperties SessionCustomProperties `json:"customProperties"`
}

// ActivitiesForXUIDs finds every open Minecraft multiplayer session ("activity") associated
// with any of the given XUIDs - in practice, at most one per XUID, since a player is normally
// in at most one world at a time. A XUID with no open session simply has no entry in the
// returned slice; that is not itself an error (the person may not be playing right now).
func ActivitiesForXUIDs(ctx context.Context, authSession *session.Session, xuids []string) ([]ActiveSession, error) {
	if len(xuids) == 0 {
		return nil, nil
	}
	auth, err := activityAuthHeader(ctx, authSession)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(activitySearchRequest{
		Type:            "activity",
		ServiceConfigID: ServiceConfigID,
		Owners:          activitySearchOwner{XUIDs: xuids},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal activity search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, activityHandlesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query activity handles: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("query activity handles: status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Results []activityHandleResponse `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode activity handles response: %w: %s", err, string(respBody))
	}

	out := make([]ActiveSession, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, ActiveSession{Ref: r.SessionRef, Custom: r.CustomProperties})
	}
	return out, nil
}

// activityAuthHeader mirrors Session.authHeader (session.go) exactly - same relying party, same
// SetAuthHeader-then-extract pattern - duplicated rather than exported from Session because this
// lookup is not scoped to any one hosted session the way every Session method is.
func activityAuthHeader(ctx context.Context, authSession *session.Session) (string, error) {
	tok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		return "", fmt.Errorf("request xbl token: %w", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://sessiondirectory.xboxlive.com/", nil)
	tok.SetAuthHeader(req)
	return req.Header.Get("Authorization"), nil
}
