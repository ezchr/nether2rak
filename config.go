package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type FileConfig struct {
	// BackendTransport selects how the relay reaches the backend server:
	//
	//	"raknet"            - (default) any RakNet Bedrock server (Geyser, older BDS, PocketMine,
	//	                      Nukkit/PNX...), dialed at ServerAddress.
	//	"nethernet"         - a server started with transport=nethernet, reached at
	//	                      ServerAddress over WebRTC. The relay is then NetherNet on
	//	                      both legs and no RakNet is involved anywhere in the path.
	//	"nethernet-norelay" - as above, except this process stays out of the data path: it keeps
	//	                      the Xbox Live session and signaling (which no Bedrock server can hold
	//	                      itself) but forwards each SDP offer to the backend, so the player's
	//	                      client and the backend negotiate WebRTC directly. No game packet ever
	//	                      passes through here. See bridge/norelay.go.
	//
	// The client-facing side is always NetherNet regardless; the first two only change the backend
	// leg, while the third removes the backend leg entirely.
	//
	// "nethernet-norelay" moves player authentication to the backend, because nothing verifies an
	// identity here and forwards it on any more - the backend receives the player's own signed
	// login. That is a security improvement, but only if the backend actually checks it: a native
	// BDS in this mode wants online-mode=true, since the online-mode=false it needs behind a relay
	// would let anyone claim any XUID once players can reach it directly.
	BackendTransport string `json:"backend_transport"`

	// ServerAddress is the game server players end up on, as "host:port" - the one setting for it
	// whatever BackendTransport is:
	//
	//	"raknet"                         - the server's RakNet (UDP) port, e.g. "127.0.0.1:19132".
	//	                                   A Geyser listener here MUST have encryption disabled -
	//	                                   see proxy/dial.go's ForwardLogin comment.
	//	"nethernet", "nethernet-norelay" - the server's TCP server-port, where a BDS running
	//	                                   transport=nethernet serves /v1/join, e.g.
	//	                                   "127.0.0.1:19134". "http://host:port" also works. It is
	//	                                   NOT the UDP LAN-discovery port (7551): BDS does not
	//	                                   announce dedicated servers over LAN discovery,
	//	                                   confirmed 2026-09-15 by packet capture.
	//
	// Empty means the mode's default (127.0.0.1:19132 for raknet, 127.0.0.1:19134 otherwise).
	ServerAddress string `json:"server_address"`

	// GeyserAddress and NetherNetBackendAddress are what ServerAddress used to be split into, one
	// per kind of backend. They are still read so older config files keep working (see
	// resolveServerAddress), and after loading they hold ServerAddress in the form each transport
	// needs, so the rest of the code reads them as before. New configs should not set them.
	GeyserAddress           string `json:"geyser_address,omitempty"`
	NetherNetBackendAddress string `json:"nethernet_backend_address,omitempty"`

	// Notes are things worth telling the operator about the file that was loaded - old setting
	// names, settings the chosen mode ignores. Never read from or written to the file.
	Notes []string `json:"-"`

	// World info shown on the Friends tab / session listing.
	HostName   string `json:"host_name"`
	WorldName  string `json:"world_name"`
	MaxPlayers int    `json:"max_players"`

	// FakePlayerCount, when above zero, is shown on the Friends tab instead of the real count.
	// Zero (with no fake_player_drift) shows the backend's real player count - everyone on it,
	// however they joined - read every 15s and pushed as soon as it changes, never below 1. See
	// advertisedPlayers.
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

	// ControlPort is the one loopback-only (127.0.0.1) HTTP port for controlling and reading the
	// relay - see control.go for every path on it: /ping (the BDS ping plugin), /invites/... and
	// /friends/add/... (invites, OFF until started), /debug/pprof/... (only with -debug). Give
	// each relay process on one machine its own. Default 7777.
	ControlPort int `json:"control_port"`

	// InvitePort, PingPort and PprofPort were ports of their own before ControlPort. Still read
	// so older files keep working: with no control_port set, the control port takes ping_port's
	// number (so a ping plugin pointed at it keeps working), else invite_port's. See
	// resolveControlPort.
	InvitePort int `json:"invite_port,omitempty"`
	PingPort   int `json:"ping_port,omitempty"`
	PprofPort  int `json:"pprof_port,omitempty"`

	// NoFriendAccept stops this account auto-accepting incoming Xbox Live friend requests. By
	// default every broadcasting account accepts them, since a friend is who can see the world;
	// set it for an account that is also somebody's own, whose friends list they want to manage
	// themselves. Applies to the primary broadcast; each extra broadcast has its own.
	NoFriendAccept bool `json:"no_friend_accept"`

	// ExtraBroadcasts publishes the same backend as additional Friends-tab worlds, each hosted
	// by its OWN Xbox account (its own token file). Every Xbox session is capped at 30 members
	// (maxMembersCount in Minecraft's MinecraftLobby session template - read from a live
	// session document 2026-09-23), so each extra account adds another 30 join slots. The
	// primary broadcast (token.json, host_name, world_name above) is unchanged and always runs.
	// See broadcasts.go.
	ExtraBroadcasts []BroadcastConfig `json:"extra_broadcasts"`

	// BroadcastName and InvitesEnabled describe the one broadcast a copy of the config is being
	// run for (see runBroadcast and BroadcastConfig.forBroadcast). Never read from the file.
	BroadcastName  string `json:"-"`
	InvitesEnabled bool   `json:"-"`
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
	// Invites puts this broadcast's invite controls on the control port, under
	// /broadcast/<name>/invites/... (see control.go). Default false: off. The primary broadcast's
	// are always there, at /invites/...
	Invites bool `json:"invites"`
	// InvitePort is what Invites used to be: a port of its own. Above zero, it is read as
	// Invites: true.
	InvitePort int `json:"invite_port,omitempty"`
	// NoFriendAccept: as FileConfig.NoFriendAccept, for this broadcast's account. Not inherited
	// from the primary - each account's friends list is its own.
	NoFriendAccept bool `json:"no_friend_accept"`
}

func defaultConfig() FileConfig {
	return FileConfig{
		// Defaults preserve the original behaviour exactly: RakNet into Geyser. The NetherNet
		// backend is strictly opt-in via backend_transport. ServerAddress is left empty on
		// purpose: its default depends on the transport, and an older file may still name the
		// address under geyser_address / nethernet_backend_address (see resolveServerAddress).
		BackendTransport: "raknet",
		HostName:         "Nether2Rak",
		WorldName:        "Nether2Rak",
		MaxPlayers:       20,
		// These matched the MCXboxBroadcastStandalone.jar you uploaded (Bedrock_v2168, build
		// 149) at the time this was written. Bedrock's protocol number changes with nearly
		// every release - CHECK Geyser's own startup log for "protocol X" and keep this in
		// sync, or joins will fail with an outdated-client/server disconnect.
		Protocol:                   2168,
		Version:                    "1.26.44",
		UpdateIntervalSeconds:      30,
		Compression:                "snappy",
		CompressionThreshold:       256,
		FixNativeBDSPersistence:    false,
		RelayDirectIP:              false,
		RelayDirectIPListenAddress: ":19132",
		// ControlPort is left 0 on purpose, like ServerAddress: an older file may still carry
		// the ports it replaced (see resolveControlPort).
	}
}

func loadConfig(path string) (FileConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := defaultConfig()
		resolveServerAddress(&cfg, nil)
		resolveControlPort(&cfg, nil)
		cfg.GeyserAddress, cfg.NetherNetBackendAddress = "", "" // new files carry server_address only
		out, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(path, out, 0644)
		resolveServerAddress(&cfg, nil)
		return cfg, nil
	}
	if err != nil {
		return FileConfig{}, err
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return FileConfig{}, err
	}
	var present map[string]json.RawMessage
	_ = json.Unmarshal(b, &present)
	resolveServerAddress(&cfg, present)
	resolveControlPort(&cfg, present)
	noteIgnoredSettings(&cfg, present)
	return cfg, nil
}

const (
	defaultRakNetAddress    = "127.0.0.1:19132"
	defaultNetherNetAddress = "127.0.0.1:19134"
)

// usesNetherNet reports whether the configured transport reaches the backend over NetherNet
// (its /v1/join HTTP endpoint) rather than RakNet.
func usesNetherNet(cfg FileConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.BackendTransport)) {
	case "nethernet", "nethernet-norelay":
		return true
	}
	return false
}

// resolveServerAddress settles the one backend address: server_address if the file sets it,
// otherwise the older per-transport key that applies to the chosen transport, otherwise the
// transport's default. It then fills GeyserAddress / NetherNetBackendAddress from it in the form
// the transport uses, which is what the rest of the code reads. present is the set of keys in
// the file (nil for a freshly written default) and only decides which notes are added.
func resolveServerAddress(cfg *FileConfig, present map[string]json.RawMessage) {
	nether := usesNetherNet(*cfg)
	usedKey, unusedKey := "geyser_address", "nethernet_backend_address"
	if nether {
		usedKey, unusedKey = unusedKey, usedKey
	}
	_, hasUsed := present[usedKey]
	_, hasUnused := present[unusedKey]

	if cfg.ServerAddress == "" {
		legacy := cfg.GeyserAddress
		if nether {
			legacy = cfg.NetherNetBackendAddress
		}
		switch {
		case hasUsed && legacy != "":
			cfg.ServerAddress = legacy
			cfg.Notes = append(cfg.Notes, fmt.Sprintf(
				"config.json names the server under %q - that still works, but it is now just "+
					"\"server_address\"; renaming it is enough", usedKey))
		case nether:
			cfg.ServerAddress = defaultNetherNetAddress
		default:
			cfg.ServerAddress = defaultRakNetAddress
		}
	} else if hasUsed {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf(
			"%q is ignored because \"server_address\" is set - it can be deleted", usedKey))
	}
	if hasUnused {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf(
			"%q is ignored with backend_transport %q (only \"server_address\" is used) - it can be deleted",
			unusedKey, cfg.BackendTransport))
	}

	address := strings.TrimSpace(cfg.ServerAddress)
	if nether {
		cfg.NetherNetBackendAddress = address
	} else {
		// RakNet has no URL form; tolerate one pasted from a NetherNet config.
		address = strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
		cfg.GeyserAddress = strings.TrimSuffix(address, "/")
	}
}

const defaultControlPort = 7777

// resolveControlPort settles the control port: control_port if the file sets it, otherwise the
// port an older file gave the ping API (so a BDS ping plugin still pointed at it keeps working),
// otherwise its invite port, otherwise the default. It notes each old port setting it finds.
func resolveControlPort(cfg *FileConfig, present map[string]json.RawMessage) {
	old := []string{}
	for _, key := range []string{"ping_port", "invite_port", "pprof_port"} {
		if _, ok := present[key]; ok {
			old = append(old, fmt.Sprintf("%q", key))
		}
	}
	switch {
	case cfg.ControlPort > 0:
	case cfg.PingPort > 0:
		cfg.ControlPort = cfg.PingPort
	case cfg.InvitePort > 0:
		cfg.ControlPort = cfg.InvitePort
	default:
		cfg.ControlPort = defaultControlPort
	}
	if len(old) > 0 {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf(
			"%s replaced by \"control_port\" - ping, invites and pprof are all served on %d now "+
				"(same paths as before); set \"control_port\": %d and delete the old ones",
			strings.Join(old, ", "), cfg.ControlPort, cfg.ControlPort))
	}
	// The primary broadcast's invite controls are always on the control port; forBroadcast sets
	// these again for each extra broadcast.
	cfg.BroadcastName, cfg.InvitesEnabled = primaryBroadcast, true
}

// noteIgnoredSettings adds a note for each setting the file sets that the chosen mode never
// reads, so a leftover from another setup can't pass for something that matters.
func noteIgnoredSettings(cfg *FileConfig, present map[string]json.RawMessage) {
	has := func(key string) bool { _, ok := present[key]; return ok }
	if strings.EqualFold(strings.TrimSpace(cfg.BackendTransport), "nethernet-norelay") {
		// Players connect straight to the backend in this mode: nothing is compressed, logged in
		// or encrypted by this process.
		for _, key := range []string{"compression", "compression_threshold", "fix_native_bds_persistence", "disable_client_encryption"} {
			if has(key) {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf(
					"%q does nothing with backend_transport \"nethernet-norelay\" (players never pass "+
						"through the relay) - it can be deleted", key))
			}
		}
	}
	if !cfg.RelayDirectIP && has("relay_direct_ip_listen_address") {
		cfg.Notes = append(cfg.Notes,
			"\"relay_direct_ip_listen_address\" does nothing while \"relay_direct_ip\" is false - it can be deleted")
	}
}
