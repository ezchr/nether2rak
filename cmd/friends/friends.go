// Command friends prints the Xbox Live friends list of the account whose Live
// token nether2rak is using, so we can confirm which gamertags are mutually
// added (chain broadcasting only reaches friends and friends-of-friends).
//
// It deliberately reads token.json without ever writing it back: refreshing
// would rotate the refresh token out from under the running nether2rak.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

type person struct {
	XUID               string `json:"xuid"`
	Gamertag           string `json:"gamertag"`
	DisplayName        string `json:"displayName"`
	IsFavorite         bool   `json:"isFavorite"`
	IsFollowingCaller  bool   `json:"isFollowingCaller"`
	IsFollowedByCaller bool   `json:"isFollowedByCaller"`
}

func main() {
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
		fmt.Println("!! live access token is expired; a refresh would rotate the")
		fmt.Println("!! refresh token nether2rak depends on. Aborting.")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Only some relying parties fill in the caller's own gamertag/XUID, and
	// peoplehub is picky about which audience it accepts. Try the plausible
	// ones and use the first that actually answers.
	parties := []string{
		"http://xboxlive.com",
		"http://xboxlive.com/",
		"http://peoplehub.xboxlive.com/",
		"http://social.xboxlive.com/",
	}

	var resp *http.Response
	for _, rp := range parties {
		xbl, err := auth.RequestXBLToken(ctx, tok, rp)
		if err != nil {
			fmt.Printf("%-34s xsts failed: %v\n", rp, err)
			continue
		}
		me := xbl.AuthorizationToken.DisplayClaims.UserInfo[0]

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://peoplehub.xboxlive.com/users/me/people/social", nil)
		if err != nil {
			panic(err)
		}
		xbl.SetAuthHeader(req)
		req.Header.Set("x-xbl-contract-version", "5")
		req.Header.Set("Accept-Language", "en-US")

		r, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("%-34s request failed: %v\n", rp, err)
			continue
		}
		fmt.Printf("%-34s gtg=%q xuid=%q -> %s\n", rp, me.GamerTag, me.XUID, r.Status)
		if r.StatusCode == http.StatusOK {
			if me.GamerTag != "" {
				fmt.Printf("\nsigned in as: %s (xuid %s)\n", me.GamerTag, me.XUID)
			}
			resp = r
			break
		}
		r.Body.Close()
	}
	if resp == nil {
		fmt.Println("\nno relying party produced a usable peoplehub token")
		os.Exit(1)
	}
	defer resp.Body.Close()
	fmt.Println()

	var body struct {
		People []person `json:"people"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		panic(err)
	}

	sort.Slice(body.People, func(i, j int) bool {
		return strings.ToLower(body.People[i].Gamertag) < strings.ToLower(body.People[j].Gamertag)
	})

	fmt.Printf("%d people on the list\n", len(body.People))
	fmt.Printf("%-24s %-20s %-8s %-8s %s\n", "GAMERTAG", "XUID", "I-ADDED", "THEY-ADD", "FAV")
	for _, p := range body.People {
		fmt.Printf("%-24s %-20s %-8v %-8v %v\n",
			p.Gamertag, p.XUID, p.IsFollowedByCaller, p.IsFollowingCaller, p.IsFavorite)
	}
}
