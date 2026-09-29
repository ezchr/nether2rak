package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/messaging"
	"github.com/gameparrot/netherconnect/session"
	"github.com/gameparrot/netherconnect/signaling"
	"github.com/pion/webrtc/v4"
)

// Listener wraps one or both NetherNet signaling transports.
//
// NetherNet has two distinct signaling paths, and a joining client may use either:
//
//   - WebSocket signaling (signaling.Dialer): we pick our own uint64 network ID and the
//     client dials wss://.../ws/v1.0/signaling/<id>. This ID is what goes in the Xbox Live
//     session's NetherNetId / WebRTCNetworkId fields. The nethernet-spec describes this path.
//   - JsonRPC "messaging" signaling (messaging.Dialer): the service assigns us a UUID. That
//     UUID is the session's PmsgId. Newer clients prefer this path.
//
// These are different values from different services - an earlier version of this file dialed
// only messaging and then tried to parse its UUID as the uint64 NetherNetId, which simply
// fails. bedrock-portal advertises both, so we listen on both and advertise both, letting the
// client choose.
type Listener struct {
	log *slog.Logger

	wsListener  *nethernet.Listener
	msgListener *nethernet.Listener

	// brokers is populated instead of the listeners above when the backend transport is
	// TransportNetherNetNoRelay. A broker answers no offer itself - it forwards each one to the
	// backend so the client negotiates with it directly - so there is no nethernet.Listener to
	// build and no connection to accept. See norelay.go.
	brokers []*signalBroker
}

// Listen starts both signaling transports. It returns the listener, the uint64 network ID for
// NetherNetId/WebRTCNetworkId, and the UUID for PmsgId. If one transport fails to start the
// other is still used, and the corresponding ID is returned zero/empty so the caller can omit
// it from the session document; if both fail, an error is returned.
// The netherNetID parameter is the ID a PREVIOUS Listen call returned; pass 0 only on the very
// first call of the process. It must be reused for the entire process lifetime, never
// regenerated per rebuild. Xbox Live's session document advertises this ID, and a Bedrock client
// that already has the world in its Friends tab routes its join signaling to whatever ID it
// cached. Regenerating it on rebuild leaves those clients signaling an ID nothing listens on any
// more: the join never reaches our signaling server at all (no CONNECTREQUEST is ever logged) and
// the client sits on "loading" until it gives up. MCXboxBroadcast - which does not exhibit this
// failure - generates its NetherNet ID exactly once in ExpandedSessionInfo's constructor (there is
// no setter for it) and its setupNetherNet() rebinds new signaling onto that same
// sessionInfo.getNetherNetId() every time it tears the transport down and back up.
// Confirmed 2026-08-22 against MCXboxBroadcast source at /root/mcjava/n2r-java/MCXboxBroadcast.
// cfg selects what each signaling transport is wired to: a nethernet.Listener that answers offers
// here (the relay modes) or a signalBroker that forwards them to the backend
// (TransportNetherNetNoRelay). Everything else about the two paths - which transports are dialed,
// which IDs are advertised - is identical, because the Xbox Live side cannot tell the difference.
func Listen(ctx context.Context, authSession *session.Session, netherNetID uint64, cfg Config, log *slog.Logger) (*Listener, uint64, string, error) {
	l := &Listener{log: log}
	noRelay := BackendBypassesRelay(cfg)

	// go-nethernet defaults to a hardcoded 5-second context for starting the ICE/DTLS/SCTP
	// transports of every accepted Conn when ConnContext is left nil. Real-world ICE
	// negotiation over the public internet - especially involving a TURN relay allocation -
	// routinely takes longer than that, and every live join attempt during testing
	// (2026-08-14) failed with "start ICE: context deadline exceeded" at exactly the 5-second
	// mark. 30 seconds matches what most WebRTC stacks use as an ICE connect timeout.
	connCtx := func(parent context.Context, _ *nethernet.Conn) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, 30*time.Second)
	}

	// Bedrock's own ICE implementation uses 4-character ufrags - real client offers
	// carry "a=ice-ufrag:na6b" - and netherboat's working server does the same (its
	// STUN USERNAME was observed as "rdLY:RzAv", i.e. a 4-character remote ufrag).
	// pion defaults to 16 characters, which is legal per RFC 5245 but may exceed
	// what the Bedrock client's STUN parser accepts, causing it to silently drop
	// our connectivity checks instead of reporting an error.
	settings := webrtc.SettingEngine{}
	settings.SetICECredentials(randomICEString(4), randomICEString(24))
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))

	// attach wires one dialed signaling transport to whichever handler the mode calls for. In
	// no-relay mode nothing local terminates the connection, so no WebRTC settings apply - the
	// ICE credentials and connection timeouts above belong to peer connections this process
	// builds, and it builds none.
	attach := func(name string, sig nethernet.Signaling, dst **nethernet.Listener) error {
		if noRelay {
			broker, err := newSignalBroker(name, sig, cfg, log)
			if err != nil {
				return err
			}
			l.brokers = append(l.brokers, broker)
			return nil
		}
		ln, err := nethernet.ListenConfig{Log: log, ConnContext: connCtx, API: api}.Listen(sig)
		if err != nil {
			return err
		}
		*dst = ln
		return nil
	}

	var pmsgID string

	// --- WebSocket signaling: we choose the numeric network ID ourselves.
	// Only mint a new one if the caller has none yet (first call of the process) - see this
	// function's doc comment for why reusing it across rebuilds is load-bearing.
	// Masked to 63 bits to stay inside signed-int64 range, matching MCXboxBroadcast's
	// Math.abs(RANDOM.nextLong()); rand.Uint64 alone can exceed int64 max, which nothing has been
	// observed to reject but which no reference implementation ever emits.
	if netherNetID == 0 {
		netherNetID = rand.Uint64() &^ (1 << 63)
	}
	// Keep the generated value even if websocket signaling later fails and zeroes
	// netherNetID for the session document; the messaging transport still needs
	// a stable numeric ID to identify us in signaling payloads.
	numericNetherNetID := netherNetID
	if mcTok, err := authSession.MCToken(ctx); err != nil {
		log.Warn("could not obtain mc token for websocket signaling", "err", err)
		netherNetID = 0
	} else {
		wsDialer := signaling.Dialer{NetworkID: strconv.FormatUint(netherNetID, 10), Log: log}
		wsSig, err := wsDialer.DialContext(ctx, mcTok)
		if err != nil {
			log.Warn("websocket signaling unavailable, continuing without it", "err", err)
			netherNetID = 0
		} else if err := attach("websocket", wsSig, &l.wsListener); err != nil {
			log.Warn("could not listen on websocket signaling", "err", err)
			netherNetID = 0
		} else {
			log.Info("websocket signaling listener started", "netherNetID", netherNetID, "noRelay", noRelay)
		}
	}

	// --- JsonRPC messaging signaling: the service assigns a UUID.
	if mcTok, err := authSession.MCToken(ctx); err != nil {
		log.Warn("could not obtain mc token for messaging signaling", "err", err)
	} else {
		msgDialer := messaging.Dialer{Log: log}
		msgSig, err := msgDialer.DialContext(ctx, mcTok)
		if err != nil {
			log.Warn("messaging signaling unavailable, continuing without it", "err", err)
		} else if err := attach("messaging", msgSig, &l.msgListener); err != nil {
			log.Warn("could not listen on messaging signaling", "err", err)
		} else {
			pmsgID = msgSig.NetworkID()
			// Identify ourselves to the client by our numeric NetherNetId,
			// matching what real hosts put in the signaling payload.
			msgSig.SetNetherNetID(strconv.FormatUint(numericNetherNetID, 10))
			log.Info("messaging signaling listener started", "pmsgID", pmsgID, "noRelay", noRelay)
		}
	}

	if l.wsListener == nil && l.msgListener == nil && len(l.brokers) == 0 {
		return nil, 0, "", fmt.Errorf("no nethernet signaling transport could be started")
	}
	return l, netherNetID, pmsgID, nil
}

// Serve accepts connections from every active transport until ctx is cancelled. ctx governs
// the signaling transports and the accept loop only; connCtx is used for already-accepted
// connections handed to HandleConn (see the call site in main.go's runSession for why these
// are deliberately different contexts - a signaling-side session cycle must not kill players
// who are already mid-game on a healthy, independent WebRTC transport).
func (l *Listener) Serve(ctx, connCtx context.Context, cfg Config) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// No-relay mode: each broker only needs its signaling subscription to stay up, since every
	// connection it negotiates belongs to the client and the backend and never reaches this
	// process. connCtx has nothing to govern here - there are no accepted connections to outlive
	// a session cycle - and because a broker holds no player's transport, losing signaling costs
	// nothing beyond new joins until the session is rebuilt.
	if len(l.brokers) > 0 {
		for _, broker := range l.brokers {
			wg.Add(1)
			go func(broker *signalBroker) {
				defer wg.Done()
				errCh <- broker.Run(ctx)
			}(broker)
		}
		return <-errCh
	}

	accept := func(name string, ln *nethernet.Listener) {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				errCh <- fmt.Errorf("%s listener stopped: %w", name, err)
				return
			}
			nc, ok := conn.(*nethernet.Conn)
			if !ok {
				continue
			}
			// remote shows the ICE route picked: "relay" means through a TURN server, not straight here.
			l.log.Info("friends-tab connection accepted", "transport", name, "remote", nc.RemoteAddr())
			go HandleConn(connCtx, nc, cfg)
		}
	}

	go func() {
		<-ctx.Done()
		if l.wsListener != nil {
			l.wsListener.Close()
		}
		if l.msgListener != nil {
			l.msgListener.Close()
		}
	}()

	if l.wsListener != nil {
		wg.Add(1)
		go accept("websocket", l.wsListener)
	}
	if l.msgListener != nil {
		wg.Add(1)
		go accept("messaging", l.msgListener)
	}

	// Return as soon as one transport dies so the operator sees it; the other keeps running
	// until ctx is cancelled.
	return <-errCh
}

// randomICEString returns a random ICE ufrag/password of n characters, using only
// characters permitted in the SDP ice-ufrag/ice-pwd attributes.
func randomICEString(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}
