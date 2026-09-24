# nether2rak (DIY)

A from-scratch attempt at what netherboat.net's owner calls "Nether2Rak": broadcast a locally
running Geyser instance over Xbox Live as a joinable Friends-tab world, **without** the
fake-handshake-then-`TransferPacket` trick that MCXboxBroadcast uses. Real players connect
over NetherNet and stay connected — the same NetherNet connection *is* their game session,
relayed straight into Geyser over RakNet.

This is a genuine, compiling Go program (`go build ./...` and `go vet ./...` both pass, and
it links into a real binary), not pseudocode. It has **not** been run against live Xbox Live
infrastructure or a live Geyser instance — I don't have network access to either from where
this was built. Treat it as a strong starting point that needs real-world debugging, the same
way the actual Nether2Rak clearly needed months of it (per the devlogs you shared).

## What it actually does

1. Signs in with a Microsoft account (device-code flow, prints a `microsoft.com/link` code
   like MCXboxBroadcast does).
2. Opens an RTA (Real-Time Activity) websocket subscription — required before Xbox Live will
   accept a session with a member referencing your connection.
3. Opens a NetherNet (WebRTC) listener via Xbox's modern JsonRPC signaling channel.
4. Creates an Xbox Live multiplayer session (MPSD, `MinecraftLobby` template) with
   `joinRestriction`/`readRestriction` set to `"followed"` and binds an **activity handle** to
   it — both are required for it to actually show up in the Friends tab, not just exist.
5. When a real player joins via NetherNet: validates they're genuinely Xbox Live
   authenticated, then dials your local Geyser instance over RakNet and **forwards the
   player's own originally-signed login** — it does not re-authenticate as your own bot
   account. From then on it's a raw bidirectional packet relay. No transfer, one connection,
   same as what you observed on netherboat.net.
6. Reacts to RTA events in real time: session-membership changes trigger a nonce refresh (so
   joins don't fail), incoming friend requests get queued and accepted automatically.

## Setup

1. Run Geyser locally (as a Spigot/Paper plugin or standalone) with **encryption disabled** on
   its listener, bound to loopback only (e.g. `127.0.0.1:19132`, not exposed publicly). This
   is required — see "Why Geyser needs encryption off" below — and it's a normal, safe setup
   for a backend that's only ever talked to over loopback, the same trust model Velocity/Bungee
   use for their own backends.
2. `go build .` (needs the same `GOPROXY=direct`/`replace` workarounds as this sandbox if your
   network also can't reach `golang.org`'s vanity import redirector directly — see go.mod).
3. Run the binary once; it'll write a default `config.json` and exit-ready — edit
   `geyser_address`, `host_name`, `world_name`, `max_players`, and **`protocol`/`version`** to
   match whatever Geyser is currently running (check Geyser's own startup log).
4. Run it again; follow the sign-in prompt.

## What's most likely to need real debugging

Being upfront about this rather than overselling it:

- **`protocol`/`version` drift.** Confirmed as **2168** against a live Geyser 2.11.1-b1217
  log (`Player connected with username ... (2168)`), which is what `config.json` defaults to.
  This still changes with nearly every Bedrock release - re-check Geyser's log after any
  update, or clients get an outdated-client/server disconnect.
- **SDP identity assertion (the 26.40 change) — now handled.** `go-nethernet` is pinned to
  its `feature/identity-assertion` branch (commit `7015d7d8c940`), which implements the
  `a=identity` SDP attribute modern clients require: a JWT plus a detached JWS signature over
  the DTLS fingerprints, binding the identity to the WebRTC connection. `ListenConfig`
  auto-generates a server identity when one isn't supplied, so this needed no code change on
  our side beyond the dependency pin.

  Worth noting: that library's `generateFingerprints()` builds a canonical JSON payload
  containing each fingerprint's hex `digest`. If signer and verifier disagree on the *casing*
  of that digest, the signature fails to verify while everything else looks correct — which
  is almost certainly the `strings.toUpper()` bug the netherboat dev described losing a night
  to. If you hit handshake failures, check there first.

  Caveat: this is an unmerged feature branch, so it may change or be rebased. If it disappears
  upstream, the pinned commit hash in go.mod is what you need.
- **`NetherNetId`/`PmsgId` semantics.** I ported these fields exactly from the decompiled
  Java (`Connection` record: a random numeric ID + the `pmid` claim from your MC token,
  separately). The `pmid` claim is also what the Go NetherNet signaling layer independently
  derives as your `NetworkID()`, which lines up — but I can't confirm Xbox's server-side
  actually cross-checks these the way I've assumed without a live session to test against.
- **Session nonce timing.** `RefreshNonces` runs on every RTA event that includes an `ncid`
  key, mirroring the Java client. Whether that's sufficiently fast/reliable in practice is
  untested.

## Why Geyser needs `validate-bedrock-login: false`

Geyser performs the Bedrock encryption handshake **unconditionally** - `LoginEncryptionUtils
.encryptConnectionWithCert()` always calls `startEncryptionHandshake()`, with no config option
to skip it. It derives the encryption key from the public key embedded in whatever login chain
it receives.

That means a relay **cannot** forward a player's original signed login chain: Geyser would
encrypt to the player's own key, which only the player's client holds, so the relay could
never decrypt the resulting stream.

So instead, this relay generates its own keypair per connection and builds a self-signed
("offline") login carrying the player's already-verified identity - real XUID, UUID and
display name, taken from the genuine Xbox Live authentication performed at the NetherNet front
door. Geyser then encrypts to the relay's key (which works), while still reading the real
player's XUID out of the chain for its own `AuthData` and Floodgate linkage, so per-player
identity, stats and bans behave normally.

Geyser accepts an unsigned chain only when `advanced.bedrock.validate-bedrock-login` is
`false`. **That setting makes the listener trust whatever identity it is handed**, so it is
only safe when the listener cannot be reached directly:

- Set `bedrock.address` to `127.0.0.1` (the shipped default of `0.0.0.0` is public - change it).
- Firewall UDP 19132 from the outside world regardless, as defence in depth.

The Xbox Live/NetherNet front door remains the real authentication boundary; Geyser becomes a
trusted internal backend, the same trust model WaterdogPE/ProxyPass use (which is exactly what
this Geyser option was added for).

## Cross-checked against bedrock-portal 2.5.0

The Xbox Live half of this was independently verified against lucienhh's `bedrock-portal`
(a separate, actively-maintained Node implementation). That cross-check **confirmed**:
protocol 2168, the SCID, the `MinecraftLobby` template, `ConnectionType: 7`,
`TransportLayer: 2`, `readRestriction: followed`, and that **`BroadcastSetting: 3` is
specifically its "friends of friends" joinability tier** (1 = invite only, 2 = friends only).

It also caught four real bugs, since fixed:

1. **`NetherNetId` was random.** It must be the ID the NetherNet listener is actually bound
   to - go-nethernet derives a listener's identity by `strconv.ParseUint`-ing the signaling
   service's NetworkID, and clients route to exactly that value. A random ID would have made
   the world appear in the Friends tab and then silently fail to join. This was very likely
   fatal on its own.
2. **`WebRTCNetworkId` was missing entirely** from the custom properties, and must carry the
   same value as `NetherNetId`.
3. **The member subscription ID was regenerated on every session update.** bedrock-portal
   keeps one stable ID for the host's lifetime; churning it can break join notifications.
4. **`TitleId` and `LanGame` were wrong** (`896928775`/`false`, should be `0`/`true`), and
   `levelId` isn't part of the request body at all.

Note bedrock-portal is redirect-based (it transfers players elsewhere after they join), so it
was only useful for the Xbox Live session/friends layer - not for the stay-connected relay,
which remains this project's own work built on NetherConnect's proxy code.

## Credit

Built on real, unmodified logic from two MIT-licensed projects:
- `github.com/GameParrot/netherconnect` — NetherNet signaling/listener code and the core
  bidirectional Bedrock packet relay (`proxy/proxy_conn.go`), lightly extended here (see the
  `ForwardLogin`/`RawLoginPacket` additions) to relay third parties instead of only the
  operator's own account. `LICENSE-netherconnect` preserved as required.
- The MPSD session/RTA/friend-request logic in `xbl/` was ported from the decompiled
  `MCXboxBroadcastStandalone.jar` (rtm516/MCXboxBroadcast, MIT) you uploaded earlier in this
  conversation — same JSON field names, same endpoints, rewritten in Go.

## Deployment (single VPS, Geyser as a plugin)

Intended layout — everything on one box, Geyser reachable only over loopback:

```
Bedrock player ──NetherNet/WebRTC──► nether2rak ──RakNet 127.0.0.1:19132──► Geyser ──► Java server
```

1. Install Geyser as a plugin on the Java server (drop `Geyser-Spigot.jar` into `plugins/`),
   plus Floodgate so Bedrock players don't need Java accounts.
2. In Geyser's `config.yml`:
   - set `bedrock.address` to `127.0.0.1` (default is `0.0.0.0`, which is public)
   - set `advanced.bedrock.validate-bedrock-login` to `false`
   - keep `java.auth-type: floodgate` if you want Bedrock players joining without Java accounts
   Then firewall UDP 19132 from the outside. See the section above for why both are required.
3. Copy `geyser_address` in nether2rak's `config.json` to match Geyser's bedrock `address`/`port`.
4. Set `protocol`/`version` in `config.json` to whatever Geyser reports at startup.
5. Run nether2rak on the same VPS, sign in with the account the world should appear to be
   hosted by, and it should appear on that account's friends' Friends tabs.

Note the Java server does NOT need to be on the same VPS — Geyser connects outward to it like
any other client, so pointing Geyser's `remote` address at an external Java server works fine.

## Troubleshooting: `403 The requested service config cannot be accessed`

The OAuth client ID must be a genuine **Minecraft Bedrock title client ID**. Xbox Live's SISU
flow (`sisu.xboxlive.com/authorize`) issues an XSTS token carrying a *title* claim, and
Minecraft's session directory service config (SCID `4fc10100-...`) only accepts tokens bearing
Minecraft's own title.

We use `auth.AndroidConfig` (client ID `0000000048183522`), the same Bedrock Android title ID
MCXboxBroadcast uses. If you swap in a different client ID and the Microsoft sign-in page says
anything other than Minecraft - "Minecraft beta", for instance - every session directory call
will 403 even though authentication itself succeeds.

If you previously signed in under a different client ID, **delete `token.json`** before
re-running, or the cached token from the wrong title will be reused.
