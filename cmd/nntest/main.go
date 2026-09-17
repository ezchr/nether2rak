// Command nntest dials the configured NetherNet backend once and reports what happened, without
// involving the Friends tab, an Xbox Live session, or a real player.
//
// This exists because the full path has a lot of moving parts (device auth -> RTA -> Xbox Live
// session -> client WebRTC -> backend WebRTC), and a failure anywhere in it looks identical from
// the outside: the player sits on a loading screen. This isolates the backend leg alone.
//
// It still signs in, because a Bedrock Dedicated Server refuses an anonymous NetherNet peer - see
// bridge.NewClientIdentity. It reuses token.json from the working directory and never prompts.
//
// Usage:
//
//	./nntest                          # address from config.json
//	./nntest http://127.0.0.1:19134   # explicit address
//	./nntest -anon                    # dial without an identity, to see the server refuse it
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/bridge"
	"github.com/gameparrot/netherconnect/session"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

func main() {
	anon := flag.Bool("anon", false, "dial without a client identity (expected to be rejected)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	address := flag.Arg(0)
	if address == "" {
		b, err := os.ReadFile("config.json")
		if err != nil {
			log.Error("no address given and config.json unreadable", "err", err)
			os.Exit(1)
		}
		var cfg struct {
			Address string `json:"nethernet_backend_address"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Error("parse config.json", "err", err)
			os.Exit(1)
		}
		address = cfg.Address
	}

	normalized, err := bridge.NormalizeNetherNetAddress(address)
	if err != nil {
		log.Error("address is not usable", "err", err)
		os.Exit(1)
	}
	fmt.Printf("backend address: %s\n", normalized)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Println("\n--- step 1: capability probe (GET /v1/join) ---")
	if err := bridge.ProbeNetherNetBackend(ctx, normalized, log); err != nil {
		fmt.Printf("FAILED: %s\n", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	var identity *nethernet.Identity
	if *anon {
		fmt.Println("\n--- step 2: skipped (-anon), dialing with no identity ---")
	} else {
		fmt.Println("\n--- step 2: mint client identity (sign in + multiplayer token) ---")
		s, err := loadSession(ctx)
		if err != nil {
			fmt.Printf("FAILED: %s\n", err)
			os.Exit(1)
		}
		if identity, err = bridge.NewClientIdentity(ctx, s); err != nil {
			fmt.Printf("FAILED: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("ok - issuer %s, token %d bytes\n", identity.Domain, len(identity.Token))
	}

	fmt.Println("\n--- step 3: full nethernet dial (SDP exchange + ICE + DTLS + SCTP) ---")
	start := time.Now()
	conn, err := bridge.DialNetherNetBackend(ctx, normalized, identity, log)
	if err != nil {
		fmt.Printf("FAILED after %s: %s\n", time.Since(start).Round(time.Millisecond), err)
		os.Exit(1)
	}
	defer conn.Close()

	fmt.Printf("ok - connected in %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("  local:   %s\n", conn.LocalAddr())
	fmt.Printf("  remote:  %s\n", conn.RemoteAddr())
	fmt.Printf("  latency: %s\n", conn.Latency())
	fmt.Println("\nbackend leg works.")
}

// loadSession reuses the cached token.json, matching what the relay itself does at startup. It
// deliberately does not fall back to an interactive device-code login: this is a diagnostic tool
// and a hang waiting on a code nobody is watching for would be worse than a clear error.
func loadSession(ctx context.Context) (*session.Session, error) {
	b, err := os.ReadFile("token.json")
	if err != nil {
		return nil, fmt.Errorf("read token.json (run the relay once to sign in): %w", err)
	}
	tok := new(oauth2.Token)
	if err := json.Unmarshal(b, tok); err != nil {
		return nil, fmt.Errorf("parse token.json: %w", err)
	}
	src := auth.AndroidConfig.RefreshTokenSource(tok)
	s, err := session.SessionFromTokenSource(src, auth.AndroidConfig, ctx)
	if err != nil {
		return nil, fmt.Errorf("start minecraft auth session: %w", err)
	}
	return s, nil
}
