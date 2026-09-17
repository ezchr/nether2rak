// Package bridge implements the actual "no transfer, connect and stay" relay: it takes an
// inbound NetherNet (WebRTC) connection from a real player who joined via the Xbox Live
// session/Friends tab, and relays it directly into a locally-running backend server,
// forwarding the player's own originally-signed identity chain. This is the piece
// MCXboxBroadcast's RedirectPacketHandler deliberately does NOT do (it fakes a placeholder
// world and sends a TransferPacket elsewhere instead).
//
// The backend leg speaks one of two transports, chosen by Config.BackendTransport:
//
//   - "raknet" (default): a classic RakNet server such as Geyser. This is the original and
//     still the only configuration that has been running in production.
//   - "nethernet": a Bedrock Dedicated Server running with transport=nethernet, reached over
//     WebRTC via the HTTP signaling endpoints in nnendpoint.go. This makes the relay
//     NetherNet on both legs, with no RakNet anywhere in the path.
//
// The client-facing leg is always NetherNet either way; only the backend leg changes.
package bridge

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/gameparrot/netherconnect/proxy"
	"github.com/sandertv/go-raknet"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Backend transport names accepted in Config.BackendTransport and the config file.
const (
	TransportRakNet    = "raknet"
	TransportNetherNet = "nethernet"
)

// backendDialTimeout bounds a single backend dial attempt. RakNet's own Dial has an internal
// timeout, but a NetherNet dial can otherwise sit in ICE negotiation indefinitely - a player
// waiting on a dead backend should get a clear disconnect rather than an endless loading screen.
const backendDialTimeout = 30 * time.Second

// Config holds everything the relay needs to know about the local backend server and access
// policy for incoming connections.
type Config struct {
	// BackendTransport selects how the backend is reached: TransportRakNet (the default when
	// empty) or TransportNetherNet.
	BackendTransport string

	// GeyserAddress is host:port of the backend's RakNet listener, e.g. "127.0.0.1:19132".
	// Used only when BackendTransport is RakNet. That listener MUST have encryption disabled -
	// see the ForwardLogin doc comment in proxy/dial.go for why.
	GeyserAddress string

	// NetherNetBackendAddress is the base URL of the backend's NetherNet HTTP signaling
	// endpoint, e.g. "http://127.0.0.1:19134". Used only when BackendTransport is NetherNet.
	// This is the backend's TCP server-port, which is where a BDS running transport=nethernet
	// serves /v1/join - not the UDP LAN-discovery port, which BDS does not use for this.
	NetherNetBackendAddress string

	// NetherNetIdentity, when non-nil, is called to mint the client identity presented to the
	// backend on each NetherNet dial. Required when BackendTransport is NetherNet: a Bedrock
	// Dedicated Server rejects an anonymous peer. See NewClientIdentity.
	NetherNetIdentity func(ctx context.Context) (*nethernet.Identity, error)

	// FixNativeBDSPersistence enables the SelfSignedID override in ForwardIdentity, which works
	// around a native-BDS-only bug: BDS discards a self-signed login's real XUID and mints a
	// fresh player-data record every reconnect unless SelfSignedID is held stable across
	// connections. See proxy/self_signed_id.go.
	//
	// Leave this false for any other backend (Geyser, Dragonfly, PNX...) - none of them have
	// this problem, since they resolve identity from the login chain's real XUID regardless of
	// SelfSignedID, so forcing it on for them would only replace real client-supplied data with
	// no benefit.
	FixNativeBDSPersistence bool

	// AllowXUID, if non-nil, is consulted for every connecting player; return false to reject.
	// Leave nil to allow any XUID that passed real Xbox Live authentication to join, which is
	// the whole point of this bridge versus a self-only tool like NetherConnect.
	AllowXUID func(xuid string) bool

	Log *slog.Logger
}

// backendConn is what a dialed backend looks like to the relay, and is deliberately the same
// method set proxy.NewProxyConn requires. Both *raknet.Conn and *nethernet.Conn satisfy it, which
// is what lets the transport be swapped without the proxy layer knowing or caring.
type backendConn interface {
	net.Conn
	ReadPacket() ([]byte, error)
}

// HandleConn processes a single inbound NetherNet connection end-to-end: reads and validates
// the real player's login, dials the backend, forwards the real identity, then pumps packets both
// directions until either side disconnects. Call this in its own goroutine per accepted
// connection - see Listener.Accept in listener.go.
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
	// loopback ping API for the whole lifetime of the relay, not just from here to the backend.
	pingEntry := registerPing(identity.XUID, conn)
	defer unregisterPing(identity.XUID, pingEntry)

	backend, transportKey, err := dialBackend(ctx, cfg, log)
	if err != nil {
		log.Error("failed to dial backend", "transport", backendTransportName(cfg), "err", err)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "Error: could not reach the server, try again shortly."})
		time.Sleep(time.Second)
		return
	}
	defer backend.Close()

	// Fills in the second half of /ping's round trip (see bridge/pingserver.go) - until now the
	// ping API only ever measured the client leg (this VPS to the real player's device), quietly
	// leaving out the relay-to-backend leg entirely. Harmless to skip when backend doesn't
	// implement latencyReporter (shouldn't happen - both raknet.Conn and nethernet.Conn do), in
	// which case /ping just falls back to reporting the client leg alone, same as before this.
	if reporter, ok := backend.(latencyReporter); ok {
		registerBackendPing(identity.XUID, reporter)
	}

	// Forward the player's already-verified identity to the backend under a relay-owned keypair.
	// See ForwardIdentity's doc comment for why the original signed chain can't be forwarded, and
	// for why transportKey (non-nil only for a NetherNet backend) must be reused here rather than
	// letting ForwardIdentity mint an unrelated one.
	serverConn := proxy.NewProxyConn(backend, false)
	if err := serverConn.ForwardIdentity(clientConn.Protocol(), identity, clientConn.ClientData(), cfg.FixNativeBDSPersistence, transportKey); err != nil {
		log.Error("failed to log in to backend", "transport", backendTransportName(cfg), "err", err)
		_ = clientConn.WritePacket(&packet.Disconnect{Message: "Error: failed to join world: " + err.Error()})
		time.Sleep(time.Second)
		return
	}
	// backendEncrypted is logged because it decides whether this relay could forward players'
	// genuine signed login chains instead of re-signing them - see ProxyConn.EncryptionEnabled.
	// If it is false, the backend is not running a Minecraft-level encryption handshake, and
	// forwarding the real chain (giving BDS a real XUID, and with it working ops and bans)
	// becomes possible.
	log.Info("relaying player into backend",
		"transport", backendTransportName(cfg), "displayName", identity.DisplayName, "xuid", identity.XUID,
		"backendEncrypted", serverConn.EncryptionEnabled())

	errCh := make(chan error, 2)
	go pump("client->backend", clientConn, serverConn, errCh)
	go pump("backend->client", serverConn, clientConn, errCh)

	// A "client->backend: read: ... SCTP transport closed" reason here is not a fault - it is
	// simply what a normal client disconnect looks like at this transport layer. The player's own
	// NetherNet (WebRTC) connection closing its SCTP association cleanly (nil underlying error) is
	// exactly what happens when they leave the world or close the game, same as an ordinary RakNet
	// disconnect on any other relay. Confirmed 2026-09-16 by checking session durations across many
	// real players (ranging from ~11s to a few minutes, no fixed length, no ICE/DTLS failure logged
	// beforehand) - there is no accompanying backend-side or transport-fault signal, just a player
	// leaving. Investigate further only if this instead correlates with players being unable to
	// stay connected at all (e.g. every session capped at some suspiciously fixed duration).
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Info("relay ended", "displayName", identity.DisplayName, "reason", err)
		}
	case <-ctx.Done():
	}
}

// backendTransportName reports the effective transport for logging, resolving the empty default.
func backendTransportName(cfg Config) string {
	if t := strings.ToLower(strings.TrimSpace(cfg.BackendTransport)); t != "" {
		return t
	}
	return TransportRakNet
}

// dialBackend opens the backend leg using whichever transport is configured.
// dialBackend opens the backend leg and, for a NetherNet backend, also returns the private key
// used for that connection's transport identity assertion - the caller (HandleConn) must sign the
// Minecraft login with this same key, not a fresh one. See ForwardIdentity's doc comment for why.
// Always nil for a RakNet backend, which has no such binding.
func dialBackend(ctx context.Context, cfg Config, log *slog.Logger) (backendConn, *ecdsa.PrivateKey, error) {
	switch name := backendTransportName(cfg); name {
	case TransportRakNet:
		conn, err := raknet.Dial(cfg.GeyserAddress)
		if err != nil {
			return nil, nil, fmt.Errorf("dial raknet backend %s: %w", cfg.GeyserAddress, err)
		}
		return conn, nil, nil
	case TransportNetherNet:
		dialCtx, cancel := context.WithTimeout(ctx, backendDialTimeout)
		defer cancel()

		var identity *nethernet.Identity
		if cfg.NetherNetIdentity != nil {
			var err error
			if identity, err = cfg.NetherNetIdentity(dialCtx); err != nil {
				return nil, nil, fmt.Errorf("mint client identity for backend: %w", err)
			}
		}
		conn, err := DialNetherNetBackend(dialCtx, cfg.NetherNetBackendAddress, identity, log)
		if err != nil {
			return nil, nil, err
		}
		var transportKey *ecdsa.PrivateKey
		if identity != nil {
			transportKey = identity.PrivateKey
		}
		return conn, transportKey, nil
	default:
		return nil, nil, fmt.Errorf("unknown backend transport %q (want %q or %q)", name, TransportRakNet, TransportNetherNet)
	}
}

// NormalizeNetherNetAddress accepts either a full base URL ("http://127.0.0.1:19134") or a bare
// host:port ("127.0.0.1:19134") and returns a base URL suitable for HTTP signaling.
//
// A bare host:port is assumed to be plain HTTP. That is correct for the loopback backends this
// relay is built for; a remote backend behind TLS should be given an explicit https:// URL.
//
// The port is required and not defaulted, because the signaling client rejects a URL without one
// and a silent guess here would turn a config typo into an obscure dial failure later.
func NormalizeNetherNetAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("nethernet backend address is empty - set nethernet_backend_address in config.json")
	}
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	address = strings.TrimSuffix(address, "/")

	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("parse nethernet backend address %q: %w", address, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("nethernet backend address must be http or https, got %q", u.Scheme)
	}
	if u.Port() == "" {
		return "", fmt.Errorf("nethernet backend address %q must include a port (e.g. %s:19134)", address, address)
	}
	if u.Path != "" {
		return "", fmt.Errorf("nethernet backend address %q must not include a path", address)
	}
	return u.String(), nil
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
