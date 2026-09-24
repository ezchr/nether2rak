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

	// FakePlayerDrift, when set with max above zero, makes the advertised count wander inside
	// [min, max] instead of sitting on fake_player_count: every update_interval_seconds it moves
	// by a random amount of at most max_step (possibly 0). It starts at fake_player_count clamped
	// into the range (or the range's middle), and is capped at max_players-1 because a world
	// showing as full can't be joined. One value is shared by every broadcast in the process.
	// Example: "fake_player_drift": {"min": 16, "max": 26, "max_step": 2}. See drift.go.
	FakePlayerDrift *PlayerDriftConfig `json:"fake_player_drift,omitempty"`

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

	// DisableClientEncryption skips the Minecraft encryption handshake with joining players. Keep
	// it false: the handshake proves a player holds the key behind their login token, so a token
	// copied from another server cannot be replayed here to join as that player - which, with
	// xuidforward restoring real XUIDs on BDS, would mean their save data and op. Only turn it on
	// if a client version starts failing to join with "client failed to log in" in the log.
	DisableClientEncryption bool `json:"disable_client_encryption"`

	// RelayDirectIP does NOT by itself decide whether a direct-IP join can reach this machine -
	// that is up to whatever listener the backend itself has open (or a firewall, or a
	// backend-native public door like Dragonfly's own listener). What this flag decides is
	// whether a direct-IP connection, once it does arrive, gets routed through the relay's
	// identity-unifying code path (the same HandleConn Friends-tab joins use) instead of
	// reaching the backend raw and unrelayed.
	//
	// Both doors relay through the same code path when this is on, so a player gets the same
	// backend player record either way (see proxy/self_signed_id.go). With this off, a direct
	// IP join - if the backend is reachable at all - goes straight to the backend and resolves
	// to a different record than a Friends-tab join would.
	//
	// This exists for backends like native BDS, which discards the real XUID on a raw
	// self-signed login and needs the relay's derived SelfSignedID to resolve the same player
	// record every time (see FixNativeBDSPersistence above). Backends that resolve identity by
	// real XUID regardless of SelfSignedID (Dragonfly, Geyser, PNX) do not need this - turning
	// it on for them just adds a second, redundant listener with no persistence benefit.
	RelayDirectIP bool `json:"relay_direct_ip"`

	// RelayDirectIPListenAddress is the host:port this relay's own direct-IP listener binds to
	// when RelayDirectIP is on, e.g. ":19132" - the address players type. It is TCP: a Bedrock
	// client probes GET /v1/join there before it will consider a RakNet connection. This
	// listener IS the relay (see bridge/directip.go) - there is no mode where this address is
	// open without going through RelayDirectIP's identity-unifying logic.
	//
	// It must not collide with the backend's own port.
	RelayDirectIPListenAddress string `json:"relay_direct_ip_listen_address"`

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

	// ExtraBroadcasts publishes the same backend as additional Friends-tab worlds, each hosted
	// by its OWN Xbox account (its own token file). Every Xbox session is capped at 30 members
	// (maxMembersCount in Minecraft's MinecraftLobby session template - read from a live
	// session document 2026-09-23), so each extra account adds another 30 join slots. The
	// primary broadcast (token.json, host_name, world_name above) is unchanged and always runs.
	// See broadcasts.go.
	ExtraBroadcasts []BroadcastConfig `json:"extra_broadcasts"`
}

// BroadcastConfig is one extra Friends-tab broadcast. Only TokenFile is required.
type BroadcastConfig struct {
	// Name labels this broadcast's log lines. Default: "extra-1", "extra-2", ...
	Name string `json:"name"`
	// TokenFile is this account's cached Microsoft token, relative to the run directory, e.g.
	// "token2.json". Create it with `./nether2rak-unified -login token2.json`, signing in with
	// a DIFFERENT Xbox account from token.json and every other broadcast.
	TokenFile string `json:"token_file"`
	// HostName / WorldName shown on the Friends tab. Default: the primary's.
	HostName  string `json:"host_name"`
	WorldName string `json:"world_name"`
	// InvitePort, if above zero, starts this broadcast's own invite control server on that
	// loopback port (see invitewatcher.go). Default 0: off - the primary's invite_port can't be
	// shared, and every broadcast's control server needs a port of its own.
	InvitePort int `json:"invite_port"`
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
		RelayDirectIP:              false,
		RelayDirectIPListenAddress: ":19132",
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
