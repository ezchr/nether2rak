# Notes

Deeper technical detail split out of the main README — design rationale, known rough edges,
and troubleshooting for specific errors.

## What's most likely to need real debugging

Being upfront about this rather than overselling it:

- **`protocol`/`version` drift.** Confirmed as **2168** against a live Geyser 2.11.1-b1217
  log (`Player connected with username ... (2168)`) as a concrete example, which is what
  `config.json` defaults to — the actual value depends on whatever your backend (Geyser or
  otherwise) is running. This still changes with nearly every Bedrock release - re-check your
  backend's own log after any update, or clients get an outdated-client/server disconnect.
- **SDP identity assertion (the 26.40 change) — now handled.** `go-nethernet` is pinned to
  its `feature/identity-assertion` branch (commit `7015d7d8c940`), which implements the
  `a=identity` SDP attribute modern clients require: a JWT plus a detached JWS signature over
  the DTLS fingerprints, binding the identity to the WebRTC connection. `ListenConfig`
  auto-generates a server identity when one isn't supplied, so this needed no code change on
  our side beyond the dependency pin.

  Worth noting: that library's `generateFingerprints()` builds a canonical JSON payload
  containing each fingerprint's hex `digest`. If signer and verifier disagree on the *casing*
  of that digest, the signature fails to verify while everything else looks correct — a real,
  easy-to-lose-a-night-to bug. If you hit handshake failures, check there first.

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

## Why the backend needs encryption off (Geyser-specific details)

This section documents the reasoning as it applies to Geyser specifically, since that was the
original target backend. The same *general* problem (a relay can't forward a player's original
signed login chain and still be able to decrypt what comes back) applies to any Bedrock server
that unconditionally validates/encrypts against the login chain it receives — check your
specific backend's docs for its equivalent setting if you're not using Geyser.

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
door. The backend then encrypts to the relay's key (which works), while still reading the real
player's XUID out of the chain for its own auth/linkage, so per-player identity, stats and bans
behave normally.

Geyser accepts an unsigned chain only when `advanced.bedrock.validate-bedrock-login` is
`false`. **That setting makes the listener trust whatever identity it is handed**, so it is
only safe when the listener cannot be reached directly:

- Set `bedrock.address` to `127.0.0.1` (the shipped default of `0.0.0.0` is public - change it).
- Firewall UDP 19132 from the outside world regardless, as defence in depth.

The Xbox Live/NetherNet front door remains the real authentication boundary; the backend
becomes a trusted internal server, the same trust model WaterdogPE/ProxyPass use (which is
exactly what this Geyser option was added for).

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
