package main

import (
	"encoding/json"
	"os"
)

type FileConfig struct {
	// ServerAddress is host:port of the backend RakNet listener (Geyser, native BDS, or any
	// other RakNet-speaking server). It MUST have encryption disabled for this bridge to
	// work - see proxy/dial.go's ForwardLogin comment.
	ServerAddress string `json:"server_address"`

	// World info shown on the Friends tab / session listing.
	HostName  string `json:"host_name"`
	WorldName string `json:"world_name"`

	// MaxPlayers is display-only, same as MCXboxBroadcast: it's sent to Xbox Live as part
	// of the session listing, but nothing here checks it before accepting a connection, so
	// it does not actually limit how many players can join.
	MaxPlayers int `json:"max_players"`

	// FakePlayerCount, when above zero, is shown on the Friends tab instead of the real
	// count. Zero reports the real, live count of players currently connected through this
	// relay instead (see bridge.ConnectedPlayerCount and displayPlayerCount in main.go).
	FakePlayerCount int `json:"fake_player_count"`

	// Protocol/Version must match the Bedrock protocol Geyser is actually speaking. Check
	// Geyser's own logs/supported-versions for the current value - this changes with every
	// Bedrock update (see the 26.40 devlog notes about protocol drift).
	Protocol int32  `json:"protocol"`
	Version  string `json:"version"`

	// UpdateIntervalSeconds controls how often the session document is refreshed on Xbox
	// Live's side (world name/player count etc).
	UpdateIntervalSeconds int `json:"update_interval_seconds"`

	// AllowedXUIDs, if non-empty, restricts who may connect to exactly these XUIDs. Leave
	// empty to allow anyone who is a genuine Xbox Live friend that can see/join the session.
	AllowedXUIDs []string `json:"allowed_xuids"`

	// Compression is the algorithm advertised to joining Bedrock clients: "snappy"
	// (cheap, what modern Bedrock negotiates), "flate" (smaller payloads, several
	// times the CPU), or "none".
	//
	// This relay sits on a decompress/recompress boundary - every batch is inflated
	// on the way in and re-deflated on the way out - so the codec cost is paid twice
	// per batch in each direction. That makes the choice matter more here than it
	// does for an ordinary server.
	Compression string `json:"compression"`

	// CompressionThreshold is the smallest payload, in bytes, that gets compressed.
	// This was previously hardcoded to 1, meaning even single-byte packets were run
	// through the codec - strictly overhead, since they cannot compress. Bedrock
	// itself negotiates values in the 256-512 range.
	CompressionThreshold int `json:"compression_threshold"`

	// PingPort is the loopback port the real-latency ping API (for the Folia-side
	// PingDisplay plugin) listens on. Only needs to change from the default if running
	// more than one nether2rak instance on the same machine (e.g. two accounts
	// broadcasting the same world) - each instance needs its own port or the second
	// one fails to start with "address already in use".
	PingPort int `json:"ping_port"`

	// PprofPort is the loopback port the Go profiler listens on in -debug mode. Same
	// multi-instance caveat as PingPort.
	PprofPort int `json:"pprof_port"`
}

func defaultConfig() FileConfig {
	return FileConfig{
		ServerAddress: "127.0.0.1:19132",
		HostName:      "Nether2Rak",
		WorldName:     "Nether2Rak",
		MaxPlayers:    20,
		// These matched the MCXboxBroadcastStandalone.jar you uploaded (Bedrock_v2168, build
		// 149) at the time this was written. Bedrock's protocol number changes with nearly
		// every release - CHECK Geyser's own startup log for "protocol X" and keep this in
		// sync, or joins will fail with an outdated-client/server disconnect.
		Protocol:              2168,
		Version:               "1.26.44",
		UpdateIntervalSeconds: 30,
		Compression:           "snappy",
		CompressionThreshold:  256,
		PingPort:              7777,
		PprofPort:             6060,
	}
}

// fakePlayerCountNote is written alongside fake_player_count whenever this program generates
// a fresh config.json (see loadConfig), so the 0-vs-fixed-value behavior stays documented in
// the file itself even if the shipped example config.json is ever deleted and regenerated.
// It's a plain string key, not a FileConfig field, so json.Unmarshal on read silently ignores
// it - the same reason the shipped config.json can safely carry this key already.
const fakePlayerCountNote = "0 = show the real, live connected player count. Above 0 = always show that fixed number instead."

func loadConfig(path string) (FileConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := defaultConfig()
		out, _ := json.MarshalIndent(cfg, "", "  ")
		var withNote map[string]any
		if err := json.Unmarshal(out, &withNote); err == nil {
			withNote["_fake_player_count_note"] = fakePlayerCountNote
			if noted, err := json.MarshalIndent(withNote, "", "  "); err == nil {
				out = noted
			}
		}
		_ = os.WriteFile(path, out, 0644)
		return cfg, nil
	}
	if err != nil {
		return FileConfig{}, err
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}
