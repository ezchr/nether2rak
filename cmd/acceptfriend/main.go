// Command acceptfriend is a one-off tool that signs in using nether2rak's cached token.json
// and accepts a single pending Xbox Live friend request matching a given gamertag, then exits.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

var deviceAuth = auth.AndroidConfig

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: acceptfriend <gamertag>")
		os.Exit(1)
	}
	targetGamertag := os.Args[1]

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx := context.Background()

	tok := new(oauth2.Token)
	b, err := os.ReadFile("token.json")
	if err != nil {
		log.Error("read token.json", "err", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(b, tok); err != nil {
		log.Error("parse token.json", "err", err)
		os.Exit(1)
	}
	src := deviceAuth.RefreshTokenSource(tok)

	authSession, err := session.SessionFromTokenSource(src, deviceAuth, ctx)
	if err != nil {
		log.Error("start auth session", "err", err)
		os.Exit(1)
	}

	xstsTok, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		log.Error("get xbox live identity", "err", err)
		os.Exit(1)
	}
	gamertag := xstsTok.AuthorizationToken.DisplayClaims.UserInfo[0].GamerTag
	log.Info("authenticated", "gamertag", gamertag)

	authHeader := func() (string, error) {
		t, err := authSession.RequestXBLToken(ctx, "http://xboxlive.com")
		if err != nil {
			return "", err
		}
		req, _ := http.NewRequest(http.MethodGet, "https://social.xboxlive.com/", nil)
		t.SetAuthHeader(req)
		return req.Header.Get("Authorization"), nil
	}

	auth1, err := authHeader()
	if err != nil {
		log.Error("auth header", "err", err)
		os.Exit(1)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://peoplehub.xboxlive.com/users/me/people/friendrequests(received)", nil)
	req.Header.Set("Authorization", auth1)
	req.Header.Set("x-xbl-contract-version", "7")
	req.Header.Set("accept-language", "en-GB")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Error("get pending friend requests", "err", err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var parsed struct {
		People []struct {
			Xuid     string `json:"xuid"`
			Gamertag string `json:"gamertag"`
		} `json:"people"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		log.Error("parse pending friend requests", "err", err, "body", string(body))
		os.Exit(1)
	}

	log.Info("pending friend requests", "count", len(parsed.People))
	for _, p := range parsed.People {
		log.Info("pending request", "gamertag", p.Gamertag, "xuid", p.Xuid)
	}

	var targetXuid string
	for _, p := range parsed.People {
		if p.Gamertag == targetGamertag {
			targetXuid = p.Xuid
			break
		}
	}
	if targetXuid == "" {
		log.Error("no pending friend request found matching gamertag", "target", targetGamertag)
		os.Exit(1)
	}

	auth2, err := authHeader()
	if err != nil {
		log.Error("auth header", "err", err)
		os.Exit(1)
	}
	url := fmt.Sprintf("https://social.xboxlive.com/users/me/people/friends/v2/xuid(%s)", targetXuid)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	req2.Header.Set("Authorization", auth2)
	resp2, err := client.Do(req2)
	if err != nil {
		log.Error("accept friend request failed", "err", err)
		os.Exit(1)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	var acceptResp struct {
		IsFriend bool `json:"isFriend"`
	}
	_ = json.Unmarshal(body2, &acceptResp)

	if !acceptResp.IsFriend {
		log.Error("accept did not result in friendship", "status", resp2.StatusCode, "body", string(body2))
		os.Exit(1)
	}
	log.Info("accepted friend request", "gamertag", targetGamertag, "xuid", targetXuid)
}
