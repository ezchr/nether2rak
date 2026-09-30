// Command friendaudit saves an account's full Xbox friends list with details (gamertag, date
// added, gamerscore, follower/following counts, presence) as JSON. Read-only: it changes nothing
// on the account, and never writes the token file it is given.
//
// Usage: friendaudit <token.json> > people.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gameparrot/netherconnect/session"
	xblpkg "github.com/gameparrot/netherconnect/xbl"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	tok := new(oauth2.Token)
	if err := json.Unmarshal(b, tok); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := session.SessionFromTokenSource(auth.AndroidConfig.RefreshTokenSource(tok), auth.AndroidConfig, ctx)
	if err != nil {
		panic(err)
	}
	xbl, err := s.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		panic(err)
	}
	friends, err := xblpkg.ListFriends(ctx, s)
	if err != nil {
		panic(err)
	}
	var all []json.RawMessage
	for i := 0; i < len(friends); i += 100 {
		var xuids []string
		for _, f := range friends[i:min(i+100, len(friends))] {
			xuids = append(xuids, f.XUID)
		}
		reqBody, _ := json.Marshal(map[string]any{"xuids": xuids})
		url := "https://peoplehub.xboxlive.com/users/me/people/batch/decoration/detail,presenceDetail"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			panic(err)
		}
		xbl.SetAuthHeader(req)
		req.Header.Set("x-xbl-contract-version", "3")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "en-US")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			panic(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "batch %d: status %d: %s\n", i, resp.StatusCode, body)
			os.Exit(1)
		}
		var page struct {
			People []json.RawMessage `json:"people"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			panic(err)
		}
		all = append(all, page.People...)
	}
	out, _ := json.Marshal(map[string]any{"people": all, "friendCount": len(friends)})
	os.Stdout.Write(out)
}
