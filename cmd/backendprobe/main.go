// Command backendprobe logs a made-up player into a backend the way the relay does (relay mode,
// self-signed login) and prints what the backend sends at the join: the StartGame settings, item
// registry, custom blocks, entity identifiers, resource packs and packet counts. Used to compare
// backends a relay moves players between. The backend must accept self-signed logins (a relay
// door), and the made-up player really joins it for a moment.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/ezchr/nether2rak/bridge"
	"github.com/ezchr/nether2rak/session"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

// savingTokenSource writes a refreshed token back to its file, as the relay does, so the refresh
// token the file holds stays the newest one.
type savingTokenSource struct {
	src  oauth2.TokenSource
	path string
	last string
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err == nil && t.RefreshToken != "" && t.RefreshToken != s.last {
		if b, err := json.Marshal(t); err == nil && os.WriteFile(s.path, b, 0o600) == nil {
			s.last = t.RefreshToken
		}
	}
	return t, err
}

func main() {
	raknetAddr := flag.String("raknet", "", "backend RakNet address (host:port)")
	nnAddr := flag.String("nethernet", "", "backend NetherNet signaling address (http://host:port)")
	name := flag.String("name", "n2rprobe", "player name to join as")
	xuid := flag.String("xuid", "2535400000000001", "XUID to join as")
	out := flag.String("out", "", "also write the full report as JSON to this file")
	stay := flag.Duration("stay", 3*time.Second, "how long to stay after spawning, counting packets")
	tokenFile := flag.String("token", "", "token.json of a relay account, for backends that need a NetherNet client identity (the refreshed token is written back)")
	flag.Parse()

	var identity func(ctx context.Context) (*nethernet.Identity, error)
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			os.Exit(1)
		}
		tok := new(oauth2.Token)
		if err := json.Unmarshal(b, tok); err != nil {
			fmt.Fprintln(os.Stderr, "probe: token:", err)
			os.Exit(1)
		}
		src := &savingTokenSource{src: auth.AndroidConfig.RefreshTokenSource(tok), path: *tokenFile, last: tok.RefreshToken}
		s, err := session.SessionFromTokenSource(src, auth.AndroidConfig, context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe: session:", err)
			os.Exit(1)
		}
		identity = func(ctx context.Context) (*nethernet.Identity, error) { return bridge.NewClientIdentity(ctx, s) }
	}

	rep, err := bridge.ProbeBackend(context.Background(), bridge.ProbeConfig{
		RakNetAddress: *raknetAddr, NetherNetAddress: *nnAddr, Name: *name, XUID: *xuid, Stay: *stay, Identity: identity,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		if rep == nil {
			os.Exit(1)
		}
	}
	fmt.Print(rep.Summary())
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", " ")
		_ = os.WriteFile(*out, b, 0o644)
	}
}
