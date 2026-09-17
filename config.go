package main

import (
	"encoding/json"
	"os"
)

type FileConfig struct {
	// BackendTransport selects how the relay reaches the backend server:
	//
	//	"raknet"    - (default) a RakNet server such as Geyser, dialed at GeyserAddress.
	//	"nethernet" - a Bedrock Dedicated Server started with transport=nethernet, reached at
	//	              NetherNetBackendAddress over WebRTC. The relay is then NetherNet on both
	//	              legs and no RakNet is involved anywhere in the path.
	//
	// The client-facing side is always NetherNet regardless; this only changes the backend leg.
	BackendTransport string `json:"backend_transport"`

	// GeyserAddress is host:port of Geyser's RakNet listener. Geyser's listener MUST have
	// encryption disabled for this bridge to work - see proxy/dial.go's ForwardLogin comment.
	// Only used when BackendTransport is "raknet".
	GeyserAddress string `json:"geyser_address"`

	// NetherNetBackendAddress is the base URL of the backend's NetherNet HTTP signaling
	// endpoint, e.g. "http://127.0.0.1:19134". Only used when BackendTransport is "nethernet".
	//
	// This is the backend's TCP server-port - the same port named by server-port in BDS's
	// server.properties - because that is where a BDS running transport=nethernet serves the
	// /v1/join signaling endpoints. It is NOT the UDP LAN-discovery port (7551): BDS does not
	// announce dedicated servers over LAN discovery, confirmed 2026-09-15 by packet capture.
	//
	// A bare "host:port" is accepted and assumed to be plain HTTP.
	NetherNetBackendAddress string `json:"nethernet_backend_address"`

	// World info shown on the Friends tab / session listing.
	HostName   string `json:"host_name"`
	WorldName  string `json:"world_name"`
	MaxPlayers int    `json:"max_players"`

	// FakePlayerCount, when above zero, is shown on the Friends tab instead of the
	// real member count. Zero reports the session's own count honestly.
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

	// FixNativeBDSPersistence works around native BDS discarding a self-signed login's real
	// XUID, which otherwise gives every reconnect a fresh, empty player-data record. Turn this
	// on ONLY when the backend is native Bedrock Dedicated Server. Any other backend (Geyser,
	// Dragonfly, PNX...) resolves player identity from the login chain's real XUID on its own
	// and does not need or want this - see bridge.Config.FixNativeBDSPersistence.
	FixNativeBDSPersistence bool `json:"fix_native_bds_persistence"`

	// DirectIPEnabled turns on the second front door: players joining by typing this
	// machine's address into their Bedrock server list, rather than through the Friends tab.
	//
	// Both doors relay through the same code path, so a player gets the same backend player
	// record either way (see proxy/self_signed_id.go). With this off, a direct IP join would
	// have to reach the backend server directly and would resolve to a different record.
	DirectIPEnabled bool `json:"direct_ip_enabled"`

	// DirectIPListenAddress is the host:port the direct-IP front door listens on, e.g.
	// ":19132" - the address players type. It is TCP: a Bedrock client probes
	// GET /v1/join there before it will consider a RakNet connection.
	//
	// It must not collide with the backend's own port.
	DirectIPListenAddress string `json:"direct_ip_listen_address"`

	// InvitePort is the loopback port the invite-everyone-from-another-server control endpoint
	// listens on (see invitewatcher.go). The feature itself is OFF BY DEFAULT regardless of this
	// port being open - it only starts sending anything after a "start" is issued to it. Same
	// multi-instance caveat as PingPort: give each concurrently-run instance its own port.
	InvitePort int `json:"invite_port"`

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
		// Defaults preserve the original behaviour exactly: RakNet into Geyser. The NetherNet
		// backend is strictly opt-in via backend_transport.
		BackendTransport:        "raknet",
		GeyserAddress:           "127.0.0.1:19132",
		NetherNetBackendAddress: "http://127.0.0.1:19134",
		HostName:                "Nether2Rak",
		WorldName:               "Nether2Rak",
		MaxPlayers:              20,
		// These matched the MCXboxBroadcastStandalone.jar you uploaded (Bedrock_v2168, build
		// 149) at the time this was written. Bedrock's protocol number changes with nearly
		// every release - CHECK Geyser's own startup log for "protocol X" and keep this in
		// sync, or joins will fail with an outdated-client/server disconnect.
		Protocol:                2168,
		Version:                 "1.26.44",
		UpdateIntervalSeconds:   30,
		Compression:             "snappy",
		CompressionThreshold:    256,
		FixNativeBDSPersistence: false,
		DirectIPEnabled:         false,
		DirectIPListenAddress:   ":19132",
		InvitePort:              7782,
		PingPort:                7777,
		PprofPort:               6060,
	}
}

func loadConfig(path string) (FileConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := defaultConfig()
		out, _ := json.MarshalIndent(cfg, "", "  ")
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
