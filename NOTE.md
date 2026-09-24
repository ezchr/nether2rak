# n2r-unified — one player record for both join methods

A copy of `n2r-nethernet` with two additions:

1. a **direct-IP front door**, so players can join by typing this machine's address, and
2. the **self-signed persistence fix**, so a player's backend save data survives reconnects.

Together these close the identity fork: joining by IP and joining through the Friends tab now
produce **the same backend player record**.

Built 2026-09-15 from `n2r-nethernet`. `n2r/nether2rak` (Paper/Velocity) remains untouched.

## Why both were needed

The persistence fix alone was not enough. It makes *relayed* players resolve to a stable record,
but a direct-IP player who reaches the backend server directly is resolved by the backend's own
native identity handling instead — a different record. Same person, two saves, depending on how
they joined.

The fix is that **both doors now run the same code**. `HandleConn` authenticates the real player,
then enters the backend through `ForwardIdentity`, which sets

```go
clientData.SelfSignedID = selfSignedIDFromXUID(identityData.XUID)
```

a UUID v5 derived from the player's real XUID (`proxy/self_signed_id.go`). Same XUID in → same
SelfSignedID out → same record, every time, by either route. `main.go`'s `relayConfig` builds one
config value handed to both listeners, deliberately, so they cannot drift apart.

## Config

```jsonc
{
  "direct_ip_enabled": true,
  "direct_ip_listen_address": ":19132",
  "fix_native_bds_persistence": true
}
```

Both default to `false` — leaving them out gives plain `n2r-nethernet` behaviour.

`fix_native_bds_persistence` (added 2026-09-15) controls the SelfSignedID override on its own,
separately from direct-IP support. Turn it on **only** when the backend is native Bedrock
Dedicated Server. It exists to work around one specific BDS bug - BDS discards a self-signed
login's real XUID and mints a fresh, empty player-data record every reconnect unless
SelfSignedID happens to be stable across connections (see `proxy/self_signed_id.go`). Geyser,
Dragonfly and PNX backends don't have this problem: they resolve identity from the login chain's
real XUID regardless of SelfSignedID, so forcing the override on for them replaces real
client-supplied data for no benefit. It was previously hardcoded unconditionally in
`ForwardIdentity` - now `ForwardIdentity` takes it as an explicit parameter, threaded through
`bridge.Config.FixNativeBDSPersistence` from `config.json`, so the same relay binary works
correctly against any backend by just flipping this one setting.

`direct_ip_listen_address` is **TCP**, not UDP. A Bedrock client joining an address first probes
`GET /v1/join` over HTTP at that address and connects over NetherNet if it answers, only falling
back to RakNet if nothing does. So this is the port players type.

## The front door speaks NetherNet, not RakNet

Deliberate, for two reasons:

- The backend is already a NetherNet server, so RakNet would mean translating transports for
  nothing.
- It reuses the client path that is known to work. A RakNet client leg has to run Minecraft's own
  encryption handshake (NetherNet skips it — DTLS already encrypts), and the RakNet direct-IP
  build in `n2r-selfsignedfix` stalls right around there: login completes, the backend streams
  chunks, and the client falls silent without ever spawning. That bug is still unsolved. This
  design sidesteps it rather than inheriting it.

`bridge/nnserver.go` is a port of upstream's `go-nethernet/endpoint.Handler`, adapted to the
pinned `Signaling` interface for the same reasons `nnendpoint.go` ports the client half.

## Server-list entry

`GET /v1/join` returns the JSON a Bedrock client shows in the server list. Real BDS answers this
with:

```json
{"name":"...","protocol":2193,"version":"1.26.50","level":"...","players":2,"maxPlayers":30,"gameType":0}
```

(Upstream's own `Handler` answers a bare `200`, which is enough to connect but leaves the entry
looking dead.) This build proxies the backend's live response for player count / protocol /
version and overrides name and capacity from config, so the entry matches the Friends-tab world
and still shows a real player count. If the backend is unreachable the entry renders from config
alone rather than disappearing.

Note `fake_player_count` applies to the **Friends tab only**. The server list shows the real
count. Say so if you want them made consistent — it's a one-line change.

## Verified

- Direct-IP front door accepts a full NetherNet connection (HTTP signaling → identity → ICE →
  DTLS → SCTP) in **10ms**, tested via `cmd/nntest` against `http://127.0.0.1:19132`.
- Server-list endpoint returns correct live JSON.
- Friends-tab join relays a real player into the backend end to end (`ninj5587`, backend player
  count went to 1).

**Not yet verified:** a real Minecraft client joining by typed IP, and persistence across a
rejoin. The thing to check is: join by IP, place something / move, leave, rejoin through the
Friends tab, and confirm the same inventory and position.

## Caveat

A client that does not support NetherNet address joins will fall back to RakNet on UDP 19132,
where nothing is listening, and fail. There is no RakNet listener in this build by design (see
above).

## Running

```
cd /root/mcjava/n2r-unified/nether2rak
tmux new-session -d -s n2r-uni './nether2rak-unified 2>&1 | tee -a run.log'
```

Ping port 7779, pprof 6062 — distinct from `n2r` (7777/6060) and `n2r-nethernet` (7778/6061), so
they can run side by side. Token is the `ezchr` account.

Test the front door without a game client:

```
./cmd/nntest/nntest http://127.0.0.1:19132   # our own direct-IP door
./cmd/nntest/nntest                          # the backend, from config
```

## gophertunnel upgrade (2026-09-17): no longer vendored

Bumped from a vendored fork of `gophertunnel v1.56.2` to stock upstream `v1.62.0` — same
version Dragonfly itself uses. `vendor-gophertunnel/` is dead weight left on disk from before
the upgrade; `go.mod` has no `replace` for gophertunnel anymore, confirmed via
`go list -m github.com/sandertv/gophertunnel` resolving straight to `v1.62.0` with no local
path. Safe to delete the directory once nothing else references it.

The old fork existed for three patches to `ClientData.Validate()` (see the deleted
`vendor-gophertunnel/PATCHES.md` for the originals, still in backups if needed), relaxing
checks that assumed a RakNet client for fields a NetherNet client legitimately populates
differently: an empty `SelfSignedID`, an empty `ServerAddress`, and a NetherNet signaling URL
in `ServerAddress`. All three were confirmed obsolete before upgrading, not assumed:

- Empty `SelfSignedID`/`ServerAddress` — verified empirically that
  `net.ResolveUDPAddr(udp, )` returns no error, so upstream's unconditional check already
  passes an empty string on its own.
- NetherNet URL `ServerAddress` — upstream v1.62.0 added a
  `!strings.Contains(data.ServerAddress, "://")` guard before the IPv6-heuristic mangling that
  broke this, plus a real URL-parsing branch in `Validate`.

Three call-site breaks came with the bump, all fixed in the relay's own code (not gophertunnel):

- `session/session.go`: `mcTokenSource.Token()` renamed to `ServiceToken(context.Context)` to
  match the new `service.TokenSource` interface.
- `cmd/scraper/main.go`: `packet.PlayerList.ActionType` moved from the packet onto each
  `protocol.PlayerListEntry` — filtering is now per-entry.
- `proxy/dial.go`: `auth.RequestMinecraftChain` now wants a `*xsapi.Client` (go-xsapi/v2)
  instead of the `*auth.XBLToken` this relay already holds. Building a real `xsapi.Client` runs
  a full Xbox device-auth flow and, by default, dials an RTA websocket — overkill for an endpoint
  that only needs an authenticated HTTP client. Added a local `requestMinecraftChain` in
  `dial.go` that sends the same request gophertunnel's own v1.62.0 implementation does
  (XBL3.0 header only, Signature header excluded — vanilla doesn't sign this endpoint either),
  keeping the existing `XBLToken` flow. Collapse this back into a direct `auth.RequestMinecraftChain`
  call if the relay ever needs a real `xsapi.Client` for something else.

`vendor-go-nethernet/` is untouched and still pinned to the exact pseudo-version
(`v1.0.20-0.20260818142457-6fc1eb6f907c`) gophertunnel v1.62.0 itself requires — its IPv4-only
candidate filtering and ICE-state patches are unaffected. Only its `endpoint/` subpackage was
added (copied from the real v1.0.20 module), needed because `go mod tidy` pulled it in as a test
dependency of gophertunnel.

## NetherNet-into-Dragonfly resource-pack stall (2026-09-17): real bug, fixed

Joining Dragonfly over NetherNet through this relay hung forever on loading resource packs —
looked identical to earlier NetherNet failures, but this one was proven to be a genuine bug, not
a client-side cancel. Isolated with two throwaway Go programs (not kept in this repo): one dialed
Dragonfly's NetherNet listener directly with the real `go-nethernet` package (connected in
11ms), the other did a full `gophertunnel` login + `DoSpawn` straight into Dragonfly, bypassing
this relay entirely (spawned in 45ms). Dragonfly's NetherNet path is not the problem.

Root cause: `proxy/proxy_conn.go`'s `ProxyConn.ReadLoop`, reading the backend leg during login.
gophertunnel's `handleClientToServerHandshake` writes `packet.PlayStatus{Status: LoginSuccess}`
then `packet.ResourcePacksInfo` back to back with no flush in between, so a fast, local backend
(Dragonfly, on the same box) reliably delivers both in **one batch**. `ReadLoop` iterated that
batch, matched `PlayStatus`, set `loginDone = true`, and returned immediately — discarding every
packet still left in the same batch, silently, with nothing logged on either side. The client
was left waiting on a `ResourcePacksInfo` that had already been thrown away.

This is why it only showed up against Dragonfly: BDS and Geyser are slower/remote enough that
`PlayStatus` and `ResourcePacksInfo` usually land in separate reads, so the second packet got
picked up normally by the pump loop afterward. It was a batching race, not a NetherNet or
protocol-version problem — the earlier gophertunnel upgrade was unrelated to this bug and didn't
fix it on its own.

Fix: `ReadLoop` now calls `c.deferRemaining(pks[i+1:])` at all four points where it returns on
login completion (raw NetherNet `Login` passthrough, `ServerToClientHandshake`,
`ClientToServerHandshake`, and `PlayStatus` LoginSuccess/PlayerSpawn), queueing any leftover
packets in the same batch into the existing `deferredPackets` slice instead of dropping them.
They get replayed to the caller on the next `ReadPackets()` call, once the relay starts pumping.

Same-class latent bug spotted but **not** fixed: `ReadLoop`'s inner `if pkFunc, ok := c.pool[...]; ok`
has no `else` — a packet whose ID isn't in the server pool is silently dropped mid-login with no
log line, same failure shape as this bug. Hasn't caused a reported issue yet; worth fixing
proactively if another client hangs during login, nothing in the logs report comes in.
