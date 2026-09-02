// Package bridge implements the actual "no transfer, connect and stay" relay: it takes an
// inbound NetherNet (WebRTC) connection from a real player who joined via the Xbox Live
// session/Friends tab, and relays it directly into a locally-running RakNet backend server
// (Geyser, native BDS, or any other RakNet-speaking server) over RakNet, forwarding the
// player's own originally-signed identity chain. This is the piece
// MCXboxBroadcast's RedirectPacketHandler deliberately does NOT do (it fakes a placeholder
// world and sends a TransferPacket elsewhere instead).
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/proxy"
	"github.com/sandertv/go-raknet"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Config holds everything the relay needs to know about the local backend server and access
// policy for incoming connections.
type Config struct {
	// ServerAddress is host:port of the backend RakNet listener (Geyser, native BDS, or any
	// other RakNet-speaking server), e.g. "127.0.0.1:19132". It MUST have encryption disabled
	// - see the ForwardLogin doc comment in proxy/dial.go for why.
	ServerAddress string

	// AllowXUID, if non-nil, is consulted for every connecting player; return false to reject.
	// Leave nil to allow any XUID that passed real Xbox Live authentication to join, which is
	// the whole point of this bridge versus a self-only tool like NetherConnect.
	AllowXUID func(xuid string) bool

	Log *slog.Logger
}

// HandleConn processes a single inbound NetherNet connection end-to-end: reads and validates
// the real player's login, dials the backend server, forwards the real identity, then pumps
// packets both directions until either side disconnects. Call this in its own goroutine per
// accepted connection - see Listener.Accept in listener.go.
func HandleConn(ctx context.Context, conn *nethernet.Conn, cfg Config) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	defer conn.Close()

	clientConn := proxy.NewProxyConn(conn, true)
	clientConn.SetAuthEnabled(true) // reject anyone not genuinely Xbox Live authenticated

	if err := clientConn.ReadLoop(); err != nil {
		log.Warn("client failed to log in", "err", err)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "Error: " + err.Error()})
		time.Sleep(time.Second)
		return
	}

	identity := clientConn.IdentityData()
	if cfg.AllowXUID != nil && !cfg.AllowXUID(identity.XUID) {
		log.Warn("rejected connection from disallowed xuid", "xuid", identity.XUID, "displayName", identity.DisplayName)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "You are not allowed to join this server."})
		time.Sleep(time.Second)
		return
	}
	log.Info("player authenticated via xbox live", "displayName", identity.DisplayName, "xuid", identity.XUID)

	// Make this connection's real NetherNet round-trip time queryable by XUID over the
	// loopback ping API for the whole lifetime of the relay, not just from here to the
	// backend server.
	registerPing(identity.XUID, conn)
	defer unregisterPing(identity.XUID)

	rkConn, err := raknet.Dial(cfg.ServerAddress)
	if err != nil {
		log.Error("failed to dial backend server", "addr", cfg.ServerAddress, "err", err)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "Error: could not reach the server, try again shortly."})
		time.Sleep(time.Second)
		return
	}
	defer rkConn.Close()

	// Forward the player's already-verified identity to the backend server under a
	// relay-owned keypair. See ForwardIdentity's doc comment for why the original signed
	// chain can't be forwarded.
	serverConn := proxy.NewProxyConn(rkConn, false)
	if err := serverConn.ForwardIdentity(clientConn.Protocol(), identity, clientConn.ClientData()); err != nil {
		log.Error("failed to log in to backend server", "err", err)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "Error: failed to join world: " + err.Error()})
		time.Sleep(time.Second)
		return
	}
	log.Info("relaying player into backend server", "displayName", identity.DisplayName, "xuid", identity.XUID)

	errCh := make(chan error, 2)
	go pump("client->server", clientConn, serverConn, errCh)
	go pump("server->client", serverConn, clientConn, errCh)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Info("relay ended", "displayName", identity.DisplayName, "reason", err)
		}
	case <-ctx.Done():
	}
}

type packetReadWriter interface {
	ReadPackets() ([][]byte, error)
	WritePackets([][]byte) error
}

func pump(direction string, from, to packetReadWriter, errCh chan<- error) {
	for {
		pks, err := from.ReadPackets()
		if err != nil {
			errCh <- fmt.Errorf("%s: read: %w", direction, err)
			return
		}
		if err := to.WritePackets(pks); err != nil {
			errCh <- fmt.Errorf("%s: write: %w", direction, err)
			return
		}
	}
}
