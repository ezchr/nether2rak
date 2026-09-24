package xbl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gameparrot/netherconnect/session"
)

// This file is the gamertag<->XUID lookup, the missing piece for callers (like cmd/scraper's
// -friend flag) that only have a human-readable gamertag, not a raw XUID.
//
// Endpoint and response shape verified against OpenXbox/xbox-webapi-python's real, working
// ProfileProvider (xbox/webapi/api/provider/profile/__init__.py) rather than guessed from
// Microsoft's own REST docs directly - same discipline this package already follows elsewhere
// (see invite.go, session.go's own doc comments citing real captured/working implementations).

const profileBaseURL = "https://profile.xboxlive.com/users/"

// profileSettingsResponse is GET .../users/{selector}/profile/settings's response shape.
type profileSettingsResponse struct {
	ProfileUsers []profileUser `json:"profileUsers"`
}

type profileUser struct {
	ID       string           `json:"id"`
	Settings []profileSetting `json:"settings"`
}

type profileSetting struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// XUIDForGamertag resolves a real Xbox Live gamertag to its XUID. Returns an error if the
// gamertag doesn't exist or the lookup otherwise fails - there is no "not found" bool return,
// since Xbox Live itself reports that as a 404, which becomes a real Go error here rather than
// a silently zero-valued result.
func XUIDForGamertag(ctx context.Context, authSession *session.Session, gamertag string) (string, error) {
	url := profileBaseURL + "gt(" + gamertag + ")/profile/settings?settings=Gamertag"
	user, err := fetchProfile(ctx, authSession, url)
	if err != nil {
		return "", fmt.Errorf("look up xuid for gamertag %q: %w", gamertag, err)
	}
	return user.ID, nil
}

// GamertagForXUID resolves a real Xbox Live XUID to its current gamertag. Returns an error if
// the XUID doesn't exist or the lookup otherwise fails.
func GamertagForXUID(ctx context.Context, authSession *session.Session, xuid string) (string, error) {
	url := profileBaseURL + "xuid(" + xuid + ")/profile/settings?settings=Gamertag"
	user, err := fetchProfile(ctx, authSession, url)
	if err != nil {
		return "", fmt.Errorf("look up gamertag for xuid %q: %w", xuid, err)
	}
	for _, s := range user.Settings {
		if s.ID == "Gamertag" {
			return s.Value, nil
		}
	}
	return "", fmt.Errorf("look up gamertag for xuid %q: response had no Gamertag setting", xuid)
}

// fetchProfile performs the actual profile.xboxlive.com request and returns the single expected
// profileUser. Both XUIDForGamertag and GamertagForXUID hit the same endpoint shape with only
// the selector (gt(...) vs xuid(...)) differing.
func fetchProfile(ctx context.Context, authSession *session.Session, url string) (profileUser, error) {
	auth, err := activityAuthHeader(ctx, authSession)
	if err != nil {
		return profileUser{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return profileUser{}, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", "3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return profileUser{}, fmt.Errorf("request profile: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return profileUser{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var parsed profileSettingsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return profileUser{}, fmt.Errorf("decode profile response: %w: %s", err, string(body))
	}
	if len(parsed.ProfileUsers) == 0 {
		return profileUser{}, fmt.Errorf("no profile found")
	}
	return parsed.ProfileUsers[0], nil
}
