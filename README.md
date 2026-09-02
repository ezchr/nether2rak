# nether2rak does two distinct jobs in one process:

1. **FriendConnect** — broadcasts a locally running RakNet dedicated server over Xbox Live as
   a joinable Friends-tab world (the Xbox Live session/MPSD/Friends-tab-visibility side).
2. **Relay** — actually moves a joining player's traffic into that backend server and back,
   **without** the fake-handshake-then-`TransferPacket` trick that MCXboxBroadcast uses. Real
   players connect over NetherNet and stay connected — the same NetherNet connection *is*
   their game session, relayed straight into the backend server over RakNet.

The original intent behind this project was specifically Geyser (a Java-server backend
speaking Bedrock protocol via translation), and most of the setup/troubleshooting notes below
are written from that angle — but the relay itself only speaks RakNet to whatever's on the
other end, so any RakNet-speaking Bedrock dedicated server (native BDS included, not just
Geyser) works the same way as far as this code is concerned.

## Setup

You need three things running/set up, in this order: your backend server, this program built,
and a config file pointed at that server.

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
   This produces a `nether2rak` (or `nether2rak.exe` on Windows) binary in the current folder.
   It should just work — `go.mod` already points the tricky dependencies at working mirrors.
   If it still can't fetch something over the network, try `GOPROXY=direct go build .`.

**3. Edit `config.json`** (already included in this repo, no need to create it) to describe
   your setup from step 1:
   - `server_address` → the address/port your backend is listening on.
   - `protocol` / `version` → must exactly match your backend's Bedrock protocol number and
     version string, or Bedrock clients get an "outdated" error and can't join.
   - `host_name` / `world_name` → what shows up on the Friends tab.
   - `max_players` → just cosmetic, shown on the Friends tab; doesn't actually limit who can
     connect.
   - `fake_player_count` → controls what player count is shown on the Friends tab. Leave at
     `0` to show the real, live number of players currently connected through nether2rak
     (updated every `update_interval_seconds`). Set it above `0` to always show that fixed
     number instead, regardless of how many players are actually connected.

   (Leave `config.go` alone - it is a fallback config)

**4. Run the binary** (`./nether2rak` on Linux/Mac, `nether2rak.exe` on Windows) from a
   terminal, rather than double-clicking it. It's not strictly required — double-clicking it
   still opens a console window on Windows — but if it ever exits or crashes, that window
   closes immediately with it, so you'd lose whatever it just printed (including the sign-in
   code below, or an error). Running it from a terminal you already have open keeps that
   output visible and scrollable no matter what happens.

   The first time you run it, it prints a Microsoft device-code sign-in link and code
   (`microsoft.com/link`, code `XXXXXXXX`) — open that link in a browser, enter the code, and
   sign in with whichever Microsoft account you want the world to appear to be hosted by. Once
   signed in, it stays running and the world becomes visible on that account's Friends tab.

Deeper implementation notes — known rough edges most likely to need real-world debugging, the
full reasoning behind requiring the backend's login validation to be off, and troubleshooting
for specific errors — are split out into [NOTES.md](NOTES.md).

## License

This project's own code is MIT-licensed — see [LICENSE](LICENSE). It also includes real,
unmodified code from other MIT-licensed projects, each keeping its original license attached;
see Credit below.

## Credit

Built on real, unmodified logic from two MIT-licensed projects:
- `github.com/GameParrot/netherconnect` — NetherNet signaling/listener code and the core
  bidirectional Bedrock packet relay (`proxy/proxy_conn.go`), lightly extended here (see the
  `ForwardLogin`/`RawLoginPacket` additions) to relay third parties instead of only the
  operator's own account. `LICENSE-netherconnect` preserved as required.
- The MPSD session/RTA/friend-request logic in `xbl/` was ported from the decompiled
  `MCXboxBroadcastStandalone.jar` (rtm516/MCXboxBroadcast, MIT) — same JSON field names, same
  endpoints, rewritten in Go.

Also built directly on top of these MIT-licensed Go libraries, pulled in normally via
`go.mod` (not copied in, so no separate license file needed here — each is licensed under its
own repo):
- `github.com/df-mc/go-nethernet` (Dragonfly Tech) — NetherNet/WebRTC signaling and transport.
  Vendored locally at `vendor-go-nethernet/` (its own `LICENSE` travels with it) with one
  patch: `strings.ToUpper()` on the SDP fingerprint digest in `generateFingerprints()`, fixing
  an identity-assertion signature mismatch against modern Bedrock clients — see the code
  comment there for details.
- `github.com/sandertv/gophertunnel` and `github.com/sandertv/go-raknet` (Sander van Vliet) —
  Bedrock protocol types and RakNet, used throughout `bridge/`, `proxy/`, and `session/`.
- `github.com/df-mc/go-xsapi` (Dragonfly Tech) — Xbox Live service/token types used in the
  sign-in flow.

