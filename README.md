# nether2rak
Broadcast your minecraft server as a joinable world on players worlds list, allowing anyone to discover your server if they have a friend playing it.

This is what it does:
1. **FriendConnect** — broadcasts a locally running Bedrock dedicated server (RakNet or
   NetherNet) over Xbox Live as a joinable Friends-tab world (the Xbox Live
   session/MPSD/Friends-tab-visibility side).
2. **Relay** — actually moves a joining player's traffic into that backend server and back,
   **without** the fake-handshake-then-`TransferPacket` trick that other friendconnects use.
   The `TransferPacket` function in the other FriendConnects makes it impossible for friends of friends to see the world, which is why I made nether2rak... to solve that. 

The original purpose of this relay is to be used for Geyser servers, and most of the setup/troubleshooting notes below
are written from that angle — the relay speaks both RakNet and NetherNet to whatever's on the
other end, so any RakNet- or NetherNet-speaking Bedrock dedicated server works the same way as
far as this code is concerned (a Dragonfly server, for example, speaks NetherNet directly) — see
`backend_transport` in Setup below.

Native BDS now works reliably as a backend too. One thing to know going in: native BDS doesn't
log the real XUID for a relayed, self-signed login, so you can't grant operator/permissions by
XUID directly from BDS itself — you'll need an Endstone or LeviLamina plugin on the BDS side to
resolve and assign permissions from the player's actual identity.

## Setup

All you need is nether2rak and your backend server

**1. Get a Bedrock-speaking backend server running first.** This is whatever players will
actually be playing on — Geyser (bridging a Java server) or a native Bedrock Dedicated Server.
nether2rak doesn't run a world itself; it only relays players into one that's already running.
   - Start the backend server (Geyser, or your BDS binary) locally, **on the same machine as
     nether2rak**, listening on loopback only (`127.0.0.1:<some port>`, e.g.
     `127.0.0.1:19132`), not `0.0.0.0`. Otherwise you will expose your backend to the public internet.
   - Turn off its login/encryption validation (Geyser: `advanced.bedrock.validate-bedrock-login:
     false` in `config.yml`) — required for nether2rak to work at all.

**2. Build nether2rak itself:**
   ```
   go build .
   ```
   Run it on a terminal, it produces a `nether2rak` (or `nether2rak.exe` on Windows) binary in the current folder.
   If it still can't fetch something over the network, try `GOPROXY=direct go build .`.

**3. Edit `config.json`** to match your backend config:
   - `geyser_address` → the address/port your backend is listening on.
   - `protocol` / `version` → must exactly match your backend's Bedrock protocol number and
     version string, or Bedrock clients get an "outdated" error and can't join.
   - `host_name` / `world_name` → what shows up on the Friends tab.
   - `max_players` → just cosmetic, shown on the Friends tab; doesn't actually limit who can
     connect.
   - `fake_player_count` → controls what player count is shown on the Friends tab. Leave at
     `0` to show the real, live number of players currently connected through nether2rak
     (updated every `update_interval_seconds`). Set it above `0` to always show that fixed
     number instead, regardless of how many players are actually connected.
   - `backend_transport` → `"raknet"` (default) to reach the backend the original way, or
     `"nethernet"` to reach a backend that speaks NetherNet directly instead (a Dragonfly
     server, for example) - confirmed working end-to-end. When set to `"nethernet"`,
     `nethernet_backend_address` needs to point at the backend's NetherNet signaling endpoint.
   - `direct_ip_enabled` / `direct_ip_listen_address` → puts a second relay listener on this
     machine's address, so a player who types it in directly gets routed through the same
     identity-forwarding path as a Friends-tab join instead of reaching the backend natively.
     This only matters together with `fix_native_bds_persistence` below - if your backend
     already resolves identity from the real login chain (Geyser, Dragonfly, PNX), a direct
     connection to the backend itself behaves the same with or without this, so there's no
     reason to enable it.
   - `fix_native_bds_persistence` → only turn this on when the backend is a native Bedrock
     Dedicated Server, together with `direct_ip_enabled` above. BDS resolves player identity
     differently depending on whether a login arrives relayed or direct, so without this a
     player gets two separate saves depending on which door they used to join. This makes both
     doors derive the same `SelfSignedID` from the player's real XUID, so they always resolve
     to the same save.

   (Leave `config.go` alone - it is a fallback config)

**4. Run the binary** (`./nether2rak` on Linux/Mac, `nether2rak.exe` on Windows) or just double click the executable.

   The first time you run it, it prints a Microsoft device-code sign-in link and code
   (`microsoft.com/link`, code `XXXXXXXX`) — open that link in a browser, enter the code, and
   sign in with whichever Microsoft account you want the world to be broadcasted by. Once
   signed in, it stays running and the world becomes visible on that account's Friends tab.
   (The account runs a risk of getting banned, so use an alt)

Deeper implementation notes — known rough edges most likely to need real-world debugging, and troubleshooting
for specific errors — are split out into [NOTES.md](NOTES.md).

nether2rak can also invite real players from another server into your world under a
Friends-tab-hosting account, using a separate scraper process under its own dedicated account
for safety — see [INVITE_FEATURE.md](INVITE_FEATURE.md).

## License

This project's own code is MIT-licensed — see [LICENSE](LICENSE). It also includes real,
unmodified code from other MIT-licensed projects, each keeping its original license attached;
see Credit below.

## Credit

Built on real, unmodified logic from two MIT-licensed projects:
- `github.com/GameParrot/netherconnect` — NetherNet signaling/listener code and the core
  bidirectional Bedrock packet relay (`proxy/proxy_conn.go`), used here to relay third parties instead of only the
  operator's own account.
- The MPSD session/RTA/friend-request logic in `xbl/` was ported from the decompiled
  `MCXboxBroadcastStandalone.jar` (rtm516/MCXboxBroadcast, MIT)

Also built directly on top of these MIT-licensed Go libraries, pulled in normally via
`go.mod`
- `github.com/df-mc/go-nethernet` (Dragonfly Tech) — NetherNet/WebRTC signaling and transport.
  Vendored locally at `vendor-go-nethernet/` for an IPv4-only ICE candidate filter and an
  ICE-transport-state patch not yet upstream — see the code comments there for details. (An
  earlier `strings.ToUpper()` fingerprint patch that used to live here was re-verified live
  against a real NetherNet backend and found to make no difference, so it was removed.)
- `github.com/sandertv/gophertunnel` (Sander van Vliet) — Bedrock protocol types, used
  throughout `bridge/`, `proxy/`, and `session/`. Pulled in normally via `go.mod`, not vendored.
- `github.com/sandertv/go-raknet` (Sander van Vliet) — RakNet transport, vendored locally at
  `vendor-go-raknet/` to track a specific unreleased commit ahead of its last tagged version.
- `github.com/df-mc/go-xsapi` (Dragonfly Tech) — Xbox Live service/token types used in the
  sign-in flow.

