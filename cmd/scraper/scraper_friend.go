package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/gameparrot/netherconnect/messaging"
	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/signaling"
	"github.com/gameparrot/netherconnect/xbl"

	"github.com/df-mc/go-nethernet"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/service"
	"golang.org/x/oauth2"
)

// closableSignaling is nethernet.Signaling plus Close, which both messaging.Conn and
// signaling.Conn implement concretely but which nethernet.Signaling itself does not declare -
// dialFriendSignaling returns this narrower-than-concrete, wider-than-interface type so
// scrapeFriendOnce can defer-close the connection without caring which of the two concrete
// types it got back.
type closableSignaling interface {
	nethernet.Signaling
	io.Closer
}

// scrapeFriendOnce resolves friendXUID's currently open Minecraft session via Xbox Live (no
// join required for the lookup itself - see xbl.ActivitiesForXUIDs) and, if one is open, joins
// it over NetherNet using whichever signaling path that session actually advertises, then
// records players the same way scrapeOnce does for a direct-IP target.
//
// Two separate credentials are in play here, deliberately not reused for each other:
//   - tokSrc (oauth2.TokenSource) is what minecraft.Dialer itself needs to run the normal
//     Bedrock login handshake with the target - the same token source scrapeOnce already uses
//     for a direct-IP target, completely unrelated to Xbox Live session lookup.
//   - authSession (*session.Session) is what talks to Xbox Live's own APIs: ActivitiesForXUIDs
//     (finding the session) and MCToken (authenticating the NetherNet signaling dial). This is
//     the same session type nether2rak's own main.go uses for hosting, reused here read-only.
//
// Returns an error (same as scrapeOnce) whenever nothing useful happened - including "friend has
// no open session right now", which the caller's reconnect loop treats identically to a dropped
// connection: wait reconnectDelay, try again. A friend starting a world after this process
// started is picked up on the very next retry, not only at process start.
func scrapeFriendOnce(ctx context.Context, tokSrc oauth2.TokenSource, authSession *session.Session, selfXUID, friendXUID string, seen map[string]bool, out *queueWriter, log *slog.Logger) error {
	activities, err := xbl.ActivitiesForXUIDs(ctx, authSession, []string{friendXUID})
	if err != nil {
		return fmt.Errorf("look up active session for xuid %s: %w", friendXUID, err)
	}
	if len(activities) == 0 {
		return fmt.Errorf("xuid %s has no open session right now", friendXUID)
	}
	// A player is normally in at most one world at a time; if Xbox Live ever returns more than
	// one, the first is used rather than treating it as an error - same "don't fail on more data
	// than expected" posture as the rest of this codebase.
	return scrapeWorld(ctx, tokSrc, authSession, selfXUID, activities[0], 0, seen, out, log)
}

// scrapeWorld joins the world behind activity and records its players. With stay > 0 it leaves
// after that long (returning nil); with 0 it stays until the connection drops or ctx ends.
func scrapeWorld(ctx context.Context, tokSrc oauth2.TokenSource, authSession *session.Session, selfXUID string, activity xbl.ActiveSession, stay time.Duration, seen map[string]bool, out *queueWriter, log *slog.Logger) error {
	host := activity.Custom.OwnerId

	// A vanilla host rejects a login without the nonce it issued for our XUID, and only issues one
	// once we are a member of its session - see xbl/join.go. Membership needs a live RTA
	// connection for the whole stay, so the RTA websocket lives as long as this attempt does.
	rtaCtx, cancelRTA := context.WithCancel(ctx)
	defer cancelRTA()
	rta := xbl.NewRTA(authSession, selfXUID, log)
	go func() {
		if err := rta.Connect(rtaCtx); err != nil && rtaCtx.Err() == nil {
			log.Warn("rta connection for friend join ended", "err", err)
		}
	}()
	connectionID, err := rta.ConnectionID(ctx)
	if err != nil {
		return fmt.Errorf("obtain rta connection id: %w", err)
	}
	joined, err := xbl.JoinSession(ctx, authSession, selfXUID, connectionID, activity.HandleID)
	if err != nil {
		return fmt.Errorf("join xuid %s's session: %w", host, err)
	}
	defer func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := joined.Leave(leaveCtx); err != nil {
			log.Warn("failed to leave friend's session", "err", err)
		}
	}()
	nonce, custom, err := joined.WaitNonce(ctx, selfXUID, 20*time.Second)
	if err != nil {
		return fmt.Errorf("wait for xuid %s's host to issue a nonce: %w", host, err)
	}
	if len(custom.SupportedConnections) == 0 {
		custom = activity.Custom
	}
	log.Info("joined friend's xbox live session, host issued a nonce", "hostXuid", host)

	mcToken, err := authSession.MCToken(ctx)
	if err != nil {
		return fmt.Errorf("obtain mc token: %w", err)
	}

	signalCtx, cancelSignal := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSignal()

	sig, networkID, err := dialFriendSignaling(signalCtx, mcToken, custom, log)
	if err != nil {
		return fmt.Errorf("dial signaling for xuid %s: %w", host, err)
	}
	defer sig.Close()

	// No client-side Identity is explicitly configured here, but one is still sent: gophertunnel's
	// own DialContextNetwork (minecraft/dial.go) obtains a real multiplayer token via TokenSource
	// and, since NetherNet implements identityDialer, calls DialContextIdentity itself - which
	// signs a real 'a=identity' assertion into the offer automatically. That happens regardless of
	// what's set here.
	//
	// What IS set explicitly: a custom webrtc.API with Bedrock-compatible ICE credentials (4-char
	// ufrag, 24-char password), matching what bridge/listener.go and bridge/directip.go already do
	// for THEIR NetherNet transports. Their doc comments note real Bedrock clients send 4-char
	// ufrags (observed "na6b", "rdLY:RzAv") and pion's own default is 16 characters - legal per
	// RFC 5245 but potentially rejected or silently dropped by Bedrock's own stricter STUN parser.
	// That mismatch was previously only fixed on the listening side; this dial is a new code path
	// offering into a real Bedrock/BDS-hosted session (not the earlier Dragonfly throwaway tests,
	// which apparently tolerated default pion settings), so it needs the same treatment.
	settings := webrtc.SettingEngine{}
	settings.SetICECredentials(randomICEString(4), randomICEString(24))
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))

	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	dialer := minecraft.Dialer{
		TokenSource: tokSrc,
		ErrorLog:    log,
		ClientData:  login.ClientData{Nonce: nonce},
		// The scraper only reads the player list, so it never needs a world's packs - and
		// downloading them can fail outright: gophertunnel v1.62.0 cannot parse a pack manifest
		// that starts with a UTF-8 BOM, which a vanilla client accepts (seen 2026-09-28 on a
		// world carrying "Simple Auction House RP"; the failed download stalled the whole join).
		// Declining every pack makes gophertunnel answer with "all packs downloaded".
		DownloadResourcePack: func(uuid.UUID, string, int, int) bool { return false },
	}
	netherNet := minecraft.NetherNet{
		Signaling: sig,
		Dialer:    nethernet.Dialer{API: api, Log: log},
		Log:       log,
	}
	conn, err := dialer.DialContextNetwork(dialCtx, netherNet, networkID)
	if err != nil {
		return fmt.Errorf("dial %s over nethernet: %w", host, err)
	}
	defer conn.Close()

	spawnCtx, cancelSpawn := context.WithTimeout(ctx, 120*time.Second)
	defer cancelSpawn()
	if err := conn.DoSpawnContext(spawnCtx); err != nil {
		return fmt.Errorf("spawn on %s's world: %w", host, err)
	}
	log.Info("joined friend's world, recording players", "hostXuid", host, "worldName", custom.WorldName)

	// Lines in the world file name the world and its host, so entries from different worlds can be
	// told apart.
	source := csvField(custom.WorldName) + "," + csvField(custom.HostName)
	if stay <= 0 {
		return recordPlayerList(ctx, conn, source, seen, out, log)
	}
	stayCtx, cancelStay := context.WithTimeout(ctx, stay)
	defer cancelStay()
	err = recordPlayerList(stayCtx, conn, source, seen, out, log)
	if ctx.Err() == nil && stayCtx.Err() != nil {
		return nil // stayed the planned time; the read error is just our own close
	}
	return err
}

// dialFriendSignaling picks the real signaling transport the target session actually
// advertises - WebSocket signaling keyed by NetherNetId, or JsonRPC "messaging" signaling keyed
// by PmsgId - and dials into it as a joining client, using the target's own ID rather than
// generating a new one (see bridge/listener.go's own doc comment on this codebase's hosting
// side of the same two paths for why these are two distinct, non-interchangeable IDs).
//
// Prefers messaging (PmsgId) when both are present, matching bridge/listener.go's own comment
// that newer clients prefer it - the choice doesn't have to match what the client itself would
// pick, since this scraper is not relaying to that client, just independently joining the same
// session, but preferring the modern path is safer than guessing whichever legacy field happens
// to still be populated.
func dialFriendSignaling(ctx context.Context, mcToken *service.Token, custom xbl.SessionCustomProperties, log *slog.Logger) (closableSignaling, string, error) {
	if len(custom.SupportedConnections) == 0 {
		return nil, "", fmt.Errorf("session advertises no connection info")
	}
	for _, c := range custom.SupportedConnections {
		if c.PmsgId != "" {
			conn, err := (messaging.Dialer{Log: log}).DialContext(ctx, mcToken)
			if err != nil {
				return nil, "", fmt.Errorf("dial messaging signaling: %w", err)
			}
			return conn, c.PmsgId, nil
		}
	}
	for _, c := range custom.SupportedConnections {
		if c.NetherNetId != 0 {
			networkID := fmt.Sprintf("%d", c.NetherNetId)
			conn, err := (signaling.Dialer{NetworkID: networkID, Log: log}).DialContext(ctx, mcToken)
			if err != nil {
				return nil, "", fmt.Errorf("dial websocket signaling: %w", err)
			}
			return conn, networkID, nil
		}
	}
	return nil, "", fmt.Errorf("session's SupportedConnections has neither a PmsgId nor a NetherNetId")
}

// randomICEString returns a random ICE ufrag/password of n characters, using only characters
// permitted in the SDP ice-ufrag/ice-pwd attributes. Duplicated from bridge/listener.go's
// unexported helper of the same name and behavior, rather than exporting it across packages for
// this one additional caller.
func randomICEString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}
