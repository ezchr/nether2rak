// Command unfriend removes the friends listed in a JSON file ([{"xuid","gamertag"}]) from an
// account, one per second, printing each result. The token file given is only read.
//
// Usage: unfriend <token.json> <list.json>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/xbl"
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
	var list []struct{ XUID, Gamertag string }
	lb, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(lb, &list); err != nil {
		panic(err)
	}
	ctx := context.Background()
	s, err := session.SessionFromTokenSource(auth.AndroidConfig.RefreshTokenSource(tok), auth.AndroidConfig, ctx)
	if err != nil {
		panic(err)
	}
	xblTok, err := s.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		panic(err)
	}
	removed := 0
	for _, p := range list {
		for attempt := 0; attempt < 5; attempt++ {
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
				fmt.Sprintf("https://social.xboxlive.com/users/me/people/xuid(%s)", p.XUID), nil)
			xblTok.SetAuthHeader(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Println("ERROR", p.Gamertag, err)
				break
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests {
				wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
				wait = max(wait, 5)
				fmt.Println("rate limited, waiting", wait, "s")
				time.Sleep(time.Duration(wait) * time.Second)
				continue
			}
			ok := resp.StatusCode >= 200 && resp.StatusCode < 300
			if ok {
				removed++
			}
			fmt.Println(resp.StatusCode, p.Gamertag, p.XUID)
			break
		}
		time.Sleep(time.Second)
	}
	friends, err := xbl.ListFriends(ctx, s)
	if err == nil {
		fmt.Printf("removed %d of %d; friends now: %d\n", removed, len(list), len(friends))
	}
}
