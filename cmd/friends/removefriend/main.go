// Command removefriend deletes one XUID from the Xbox Live friends list of the account whose
// Live token nether2rak is using. Reads token.json read-only, same as cmd/friends, so it never
// rotates the refresh token nether2rak depends on.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

func main() {
	const targetXUID = "2535470912054074" // ezchr

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
		fmt.Println("!! live access token is expired; aborting rather than refresh it.")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	xbl, err := auth.RequestXBLToken(ctx, tok, "http://xboxlive.com")
	if err != nil {
		panic(fmt.Errorf("xsts: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("https://social.xboxlive.com/users/me/people/xuid(%s)", targetXUID), nil)
	if err != nil {
		panic(err)
	}
	xbl.SetAuthHeader(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(fmt.Errorf("delete request: %w", err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println("delete status:", resp.Status)
	if len(body) > 0 {
		fmt.Println(string(body))
	}
}
