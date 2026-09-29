// Command cbedit edits command blocks on a running Bedrock Dedicated Server
// (transport=nethernet) the way a player's client does: it joins as a real Xbox
// Live account and sends CommandBlockUpdate packets.
//
// BDS only accepts those from an op in creative mode near the block, which the
// wrapper script (cbedit.sh) arranges through the server console: cbedit prints
// "cbedit: joined as <name>" once spawned and then waits until the -ready file
// exists before sending anything.
//
// Usage: cbedit -token token.json -ready /tmp/cbedit.ready edits.json
//
// edits.json is a list of
//
//	{"pos": [x, y, z], "mode": "impulse|repeat|chain", "conditional": false,
//	 "needs_redstone": false, "delay": 0, "command": "...", "name": "", "track_output": true}
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/bridge"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

type edit struct {
	Pos           [3]int32 `json:"pos"`
	Mode          string   `json:"mode"`
	Conditional   bool     `json:"conditional"`
	NeedsRedstone bool     `json:"needs_redstone"`
	Delay         uint32   `json:"delay"`
	Command       string   `json:"command"`
	Name          string   `json:"name"`
	TrackOutput   *bool    `json:"track_output"`
}

var modes = map[string]uint32{
	"impulse": packet.CommandBlockImpulse,
	"repeat":  packet.CommandBlockRepeating,
	"chain":   packet.CommandBlockChain,
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:19134", "BDS NetherNet signaling address")
	tokenPath := flag.String("token", "token.json", "cached Xbox Live token (the scraper's format)")
	ready := flag.String("ready", "/tmp/cbedit.ready", "file whose appearance means op/creative/tp are done")
	leave := flag.String("leave", "/tmp/cbedit.leave", "file whose appearance means the console has put the account back (game mode, op), so it may disconnect")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cbedit [-addr url] [-token file] [-ready file] edits.json")
		os.Exit(2)
	}
	var edits []edit
	raw, err := os.ReadFile(flag.Arg(0))
	if err == nil {
		err = json.Unmarshal(raw, &edits)
	}
	if err != nil {
		fail("read edits", err)
	}
	for _, e := range edits {
		if _, ok := modes[e.Mode]; !ok {
			fail("edit", fmt.Errorf("unknown mode %q at %v", e.Mode, e.Pos))
		}
	}

	tokSrc, err := cachedTokenSource(*tokenPath)
	if err != nil {
		fail("token", err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetICECredentials(randomString(4), randomString(24)) // Bedrock-style ICE credentials
	network := minecraft.NetherNet{
		Signaling: bridge.NewHTTPSignaling(log),
		Dialer: nethernet.Dialer{
			API:               webrtc.NewAPI(webrtc.WithSettingEngine(settings)),
			Log:               log,
			DisableTrickleICE: true, // the whole exchange is one HTTP request
		},
		Log: log,
	}
	dialer := minecraft.Dialer{
		TokenSource: tokSrc,
		ErrorLog:    log,
		// No packs needed to edit blocks, and declining them avoids gophertunnel's BOM bug.
		DownloadResourcePack: func(uuid.UUID, string, int, int) bool { return false },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := dialer.DialContextNetwork(ctx, network, *addr)
	if err != nil {
		fail("join", err)
	}
	defer conn.Close()
	if err := conn.DoSpawnContext(ctx); err != nil {
		fail("spawn", err)
	}
	fmt.Printf("cbedit: joined as %s\n", conn.IdentityData().DisplayName)

	// Keep reading so the server's packets never back up while we wait.
	go func() {
		for {
			if _, err := conn.ReadPacket(); err != nil {
				return
			}
		}
	}()
	waitFor(*ready)

	for _, e := range edits {
		track := true
		if e.TrackOutput != nil {
			track = *e.TrackOutput
		}
		pk := &packet.CommandBlockUpdate{
			Block:             true,
			Position:          protocol.BlockPos{e.Pos[0], e.Pos[1], e.Pos[2]},
			Mode:              modes[e.Mode],
			NeedsRedstone:     e.NeedsRedstone,
			Conditional:       e.Conditional,
			Command:           e.Command,
			Name:              e.Name,
			ShouldTrackOutput: track,
			TickDelay:         e.Delay,
		}
		if err := conn.WritePacket(pk); err != nil {
			fail("send", err)
		}
		_ = conn.Flush()
		fmt.Printf("cbedit: sent %v %s %q\n", e.Pos, e.Mode, e.Command)
		time.Sleep(250 * time.Millisecond)
	}
	// Stay online until the console has restored the account (game mode, op):
	// those commands only reach an online player.
	fmt.Println("cbedit: sent all")
	waitFor(*leave)
	fmt.Println("cbedit: done")
}

// waitFor blocks until path exists, giving up after a minute.
func waitFor(path string) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			fail("wait", fmt.Errorf("%s never appeared", path))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "cbedit: %s: %v\n", what, err)
	os.Exit(1)
}

func randomString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// cachedTokenSource refreshes the scraper's cached token and writes refreshed
// tokens back, so the cache stays valid.
func cachedTokenSource(path string) (oauth2.TokenSource, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tok := new(oauth2.Token)
	if err := json.Unmarshal(b, tok); err != nil {
		return nil, err
	}
	src := &writeBack{src: auth.AndroidConfig.RefreshTokenSource(tok), path: path}
	if _, err := src.Token(); err != nil {
		return nil, fmt.Errorf("token expired, sign the scraper in again: %w", err)
	}
	return src, nil
}

type writeBack struct {
	src  oauth2.TokenSource
	path string
}

func (w *writeBack) Token() (*oauth2.Token, error) {
	tok, err := w.src.Token()
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(w.path, b, 0o600)
	}
	return tok, nil
}
