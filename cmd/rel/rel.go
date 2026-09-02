// Command rel resolves a gamertag and reports the friend relationship in BOTH
// directions between it and the account nether2rak is signed in as.
//
// Xbox "friends" are directional follows, so the two flags answer different
// questions: isFollowedByCaller = we added them; isFollowingCaller = they
// added us. Chain broadcasting only surfaces a session to accounts that have
// added the host, which is the direction that actually matters here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

type person struct {
	XUID               string `json:"xuid"`
	Gamertag           string `json:"gamertag"`
	IsFollowingCaller  bool   `json:"isFollowingCaller"`
	IsFollowedByCaller bool   `json:"isFollowedByCaller"`
	IsFavorite         bool   `json:"isFavorite"`
}

func main() {
	target := os.Args[1]

	f, err := os.Open("/root/mcjava/n2r/nether2rak/token.json")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	tok := new(oauth2.Token)
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		panic(err)
	}
	if !tok.Valid() {
		fmt.Println("live access token expired; refusing to refresh")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	xbl, err := auth.RequestXBLToken(ctx, tok, "http://xboxlive.com")
	if err != nil {
		panic(err)
	}
	me := xbl.AuthorizationToken.DisplayClaims.UserInfo[0]
	fmt.Printf("caller: %s (xuid %s)\n\n", me.GamerTag, me.XUID)

	do := func(method, u string, body []byte, ver string) (*http.Response, error) {
		var r *http.Request
		var err error
		if body == nil {
			r, err = http.NewRequestWithContext(ctx, method, u, nil)
		} else {
			r, err = http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		if err != nil {
			return nil, err
		}
		xbl.SetAuthHeader(r)
		r.Header.Set("x-xbl-contract-version", ver)
		r.Header.Set("Accept-Language", "en-US")
		r.Header.Set("Accept", "application/json")
		return http.DefaultClient.Do(r)
	}

	// 1. gamertag -> xuid
	pu := fmt.Sprintf("https://profile.xboxlive.com/users/gt(%s)/profile/settings?settings=Gamertag",
		url.PathEscape(target))
	resp, err := do(http.MethodGet, pu, nil, "3")
	if err != nil {
		panic(err)
	}
	var prof struct {
		ProfileUsers []struct {
			ID string `json:"id"`
		} `json:"profileUsers"`
	}
	json.NewDecoder(resp.Body).Decode(&prof)
	resp.Body.Close()
	if len(prof.ProfileUsers) == 0 {
		fmt.Printf("could not resolve gamertag %q (profile lookup %s)\n", target, resp.Status)
		os.Exit(1)
	}
	xuid := prof.ProfileUsers[0].ID
	fmt.Printf("resolved %s -> xuid %s\n\n", target, xuid)

	// 2. relationship in both directions
	bu := fmt.Sprintf("https://peoplehub.xboxlive.com/users/me/people/batch")
	payload, _ := json.Marshal(map[string][]string{"xuids": {xuid}})
	resp, err = do(http.MethodPost, bu, payload, "5")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("peoplehub batch returned %s\n", resp.Status)
		os.Exit(1)
	}
	var body struct {
		People []person `json:"people"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.People) == 0 {
		fmt.Println("no relationship record returned")
		return
	}
	p := body.People[0]
	fmt.Printf("%s has %s added : %v\n", target, me.GamerTag, p.IsFollowingCaller)
	fmt.Printf("%s has %s added : %v\n", me.GamerTag, target, p.IsFollowedByCaller)
	fmt.Printf("favorite               : %v\n", p.IsFavorite)
}
