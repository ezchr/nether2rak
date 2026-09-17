# Invite-everyone-from-another-server feature

Two pieces, deliberately split across two processes and two different Xbox accounts. Neither
piece is enabled unless explicitly started.

## `cmd/scraper` - what it's for

A **standalone binary**, not part of the relay. Its only job: join a Bedrock server you point it
at, as a real, live Xbox Live-authenticated client, and record every real player it sees on that
server's player list, for as long as it stays connected.

```
cd /root/mcjava/n2r-unified/run/scraper-ezchr
./scraper play.cubecraft.net:19132
```

- Requires its own `token.json` in the run directory (Microsoft device-code sign-in on first run,
  or copy an existing cached token file in). **Must be a different account from whichever one is
  hosting a nether2rak world** - see "why two accounts" below.
- Writes discovered players to `invite_queue.txt` in its own working directory, one line per
  player: `xuid,username` (e.g. `2535440792904888,EzCrazy4395`). The username is there purely so
  a human can read the file - only the xuid half is ever used programmatically.
- Runs until interrupted (Ctrl+C) or the connection drops, in which case it reconnects
  automatically after 10 seconds and keeps going. It is **not** a one-time snapshot: a player who
  joins the target server an hour after the scraper started still gets recorded, and a restart of
  the scraper picks up where the file left off (it dedupes against what's already written, so
  restarting doesn't produce duplicate lines).
- A stray `xuid="0"` can appear for some connections (a known minor gap, not yet filtered) - it
  is not a real XUID and any invite attempt against it will simply fail harmlessly on the
  receiving end.

### Why two accounts, two processes

Sending a real Xbox Live invite into a session can only be done by the account that **owns**
that session - Xbox Live checks this. That part has no way around it.

But *discovering who to invite* means joining some arbitrary third-party server, on a loop, as a
live game client - repeatedly connecting to servers that have nothing to do with yours. That's a
materially different (and riskier) thing for an Xbox account to be doing than quietly hosting a
Friends-tab world. Running both from the same account would mean one identity simultaneously
hosting a public world *and* constantly joining strangers' servers to scrape their player lists -
if either behavior ever causes a problem (a ban, a flag, a crash), it would take the other down
with it.

So: the scraper runs under its own dedicated account (tonight, `ezchr` - `token.json.previous-account`
copied into its own run directory), the relay's invite-sending stays on whichever account is
actually hosting the world, and the only thing connecting them is the shared `invite_queue.txt`
file.

## The relay side (`invitewatcher.go`, bundled into `nether2rak-unified`)

This part **is** bundled into the main relay binary - no separate process needed for it. It reads
whatever `invite_queue.txt` sits next to that specific run's `config.json`/`token.json`, and sends
real invites through that instance's own Xbox Live session.

**Off by default.** Starting the relay does not start inviting anyone, regardless of whether
`invite_queue.txt` exists. It only starts after an explicit command, issued as a loopback-only
HTTP call (same trust model as the existing ping API in `bridge/pingserver.go` - not reachable
off the VPS):

```
curl http://127.0.0.1:<invite_port>/invites/start
curl http://127.0.0.1:<invite_port>/invites/stop
curl http://127.0.0.1:<invite_port>/invites/status
```

`invite_port` is set per run directory in `config.json` (`"invite_port"`) - currently `7782` for
`run/geyser-nethernet`, `7783` for `run/dragonfly-raknet`, `7784` for `run/bds-nethernet`, so
multiple instances don't collide.

Once started, it repeats indefinitely until stopped: send an invite to everyone currently in the
queue file, one lap, then immediately start the next lap (no pause between laps), continuing
until `/invites/stop` is called or the relay's session ends. A person who's already been invited
gets invited again on the next lap - this is deliberate, so someone who missed or dismissed an
earlier invite gets another chance, not so it can spam any one person constantly (see rate limit
below).

**Rate limit: one invite sent every 10 seconds**, across the whole lap - not per person, but
between *any* two sends. Chosen after confirming delivery works but discovering that anything
much faster (3 seconds, tried first) gets folded by Xbox/the recipient's device into a single
notification's incrementing count rather than anything more useful, and running that fast
indefinitely risks looking like an abuse pattern to Xbox Live over a long-running loop.

## Invite my real Xbox friends (no scraping)

A separate, simpler source of invites, sharing the same underlying `SendInvite` mechanism as the
queue-based inviter above. Reads the host account's own real Xbox Live friends list directly
(`xbl.ListFriends`, `social.xboxlive.com/users/me/people`) - no scraper, no second account, just
this account's existing friends.

```
curl http://127.0.0.1:<invite_port>/invites/friends/start
curl http://127.0.0.1:<invite_port>/invites/friends/stop
curl http://127.0.0.1:<invite_port>/invites/friends/status
```

Same port as the queue-based inviter, different path prefix - both share one HTTP control server.
Off by default, same repeating-lap-until-stopped shape, same 10-second rate limit. Re-fetches the
friends list every lap rather than once at start, so someone added as a friend mid-run gets
included on the very next lap.

Confirmed working live 2026-09-16 immediately after building it: a real lap fetched the actual
friends list and sent real invites (same handle-response shape as the queue-based path, same
`titleId: "896928775"` fix applied identically here - both paths call the same `SendInvite`).

## Invites skip anyone already connected

Both inviters (queue-based and friends-based) check `bridge.ConnectedXUIDs()` right before every
individual send - not once per lap, since a rate-limited lap over a long list can take a while and
someone can join partway through it. Anyone currently connected through the relay's client-facing
listener (Friends-tab or direct-IP, whichever door they used) is skipped rather than sent a
pointless invite to somewhere they already are.

`ConnectedXUIDs()` (added to `bridge/pingserver.go`) reads the same `pingRegistry` the real-latency
ping API already maintains - every `HandleConn` call already registers/unregisters a player's
XUID there for the ping feature, so this reuses that bookkeeping rather than adding a second,
parallel tracking mechanism.

Scope note: this only sees players who came in through the relay itself. A player who reached the
backend directly, bypassing the relay entirely, is invisible to it - which is the correct scope
here, since the whole point of these invite features is getting people INTO the relay.

## Changes made to nether2rak itself tonight

All in `/root/mcjava/n2r-unified/nether2rak/`, the one shared source tree/binary used by every
`run/<target>` config.

### New files

- **`xbl/friendslist.go`** - `xbl.ListFriends`, the real friends-list fetch backing the "invite my friends" feature above.
- **`friendsinviter.go`** - `friendsInviteController`, its start/stop/status routes (registered onto the same mux `startInviteControlServer` builds).
- **`xbl/invite.go`** - `Session.SendInvite(ctx, xuid)`. Builds and POSTs the real Xbox Live
  invite request to `sessiondirectory.xboxlive.com/handles` (`type: "invite"`, a completely
  different handle type from the `"activity"` handle `Create()` already sent for session
  listing). Two real bugs were found and fixed while building this, both confirmed against a
  live device:
  1. The request must **not** include an `"id"` field - an early version copied Microsoft's own
     GDK REST doc example literally, which includes one, and got a hard `400 Invalid handle
     provided. The request body must not specify the handle 'id' field.` The `id` in that
     example is what the *response* contains, not something the caller sends.
  2. `inviteAttributes.titleId` must be Minecraft Bedrock's real, registered Xbox Live title ID
     - **`896928775`** (`InviteTitleId` constant) - sent as a **string** on the wire
     (`"896928775"`, not the bare int `896928775`). This project's existing `TitleId = 0`
     constant (used correctly for session/world *listing*) does NOT work for invites: Xbox Live
     still accepts a title-0 invite request and returns what looks like a completely normal
     invite handle, but nothing ever surfaces as a notification on the recipient's device,
     because there's no real game "0" for the invite UI to resolve into anything displayable.
     Confirmed by cross-checking `github.com/HashimTheArab/go-mcxboxbroadcast` (a real,
     independently-working Java-equivalent reimplementation using the same `AndroidConfig`-style
     Xbox Live auth this project uses) - its own source explicitly keeps these as two separate
     constants for exactly this reason.
- **`invitewatcher.go`** - the invite-lap loop and its HTTP start/stop/status control server, see
  above. Everything in this file is `package main`, alongside `main.go` itself.
- **`cmd/scraper/main.go`** - the standalone scraper binary described above.

### Changed files

- **`main.go`** - `runSession` now constructs an `inviteController` right after
  `xblSession.Create()` succeeds and starts `startInviteControlServer` on it
  (`127.0.0.1:<cfg.InvitePort>`). No other behavior in `main.go` changed - this is purely
  additive, gated entirely behind the controller's own stopped-by-default state.
- **`config.go`** - added `InvitePort int` to `FileConfig` (`json:"invite_port"`), defaulting to
  `7782`.
- **Every `run/<target>/config.json`** - given its own `invite_port` value so multiple instances
  can run `/invites/start` independently without colliding.

### Nothing else in the relay's existing behavior changed

The client-facing NetherNet listener, the backend dial logic (RakNet or NetherNet), identity
forwarding, the direct-IP door, and the native-BDS persistence fix are all untouched by tonight's
invite work - this was purely additive on top of the already-working relay.
