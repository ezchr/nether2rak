// Command gamertags looks up the current gamertags of a list of XUIDs, with their real
// capitals. Read-only: it changes nothing on the account, and never writes the token file.
//
// Usage: gamertags <token.json> <xuids.json> > names.json
// xuids.json is a JSON list of XUIDs; the output maps XUID to gamertag.
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

	"github.com/ezchr/nether2rak/session"
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
	b, err = os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	var xuids []string
	if err := json.Unmarshal(b, &xuids); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s, err := session.SessionFromTokenSource(auth.AndroidConfig.RefreshTokenSource(tok), auth.AndroidConfig, ctx)
	if err != nil {
		panic(err)
	}
	xbl, err := s.RequestXBLToken(ctx, "http://xboxlive.com")
	if err != nil {
		panic(err)
	}
	names := map[string]string{}
	for i := 0; i < len(xuids); i += 100 {
		reqBody, _ := json.Marshal(map[string]any{"xuids": xuids[i:min(i+100, len(xuids))]})
		url := "https://peoplehub.xboxlive.com/users/me/people/batch/decoration/detail"
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
			People []struct {
				XUID     string `json:"xuid"`
				Gamertag string `json:"gamertag"`
			} `json:"people"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			panic(err)
		}
		for _, p := range page.People {
			names[p.XUID] = p.Gamertag
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := json.MarshalIndent(names, "", " ")
	fmt.Println(string(out))
}
