package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/pion/webrtc/v4"
)

// This file adds a second front door: players who type this machine's address into the Bedrock
// server list, instead of joining through the Friends tab.
//
// The point of it is identity, not convenience. Without it, a direct IP join goes straight to the
// backend server, which resolves that player by their real Xbox identity - while a relayed join
// arrives re-signed by the relay and resolves to a different record. The same person then has two
// separate sets of player data depending on how they joined.
//
// Routing IP joins through here closes that fork: an accepted connection is handed to the very
// same HandleConn the Friends-tab listener uses, so both paths authenticate the real player, then
// enter the backend through ForwardIdentity with a SelfSignedID derived from that player's real
// XUID (see proxy/self_signed_id.go). Same XUID in, same SelfSignedID out, one player record.
//
// The front door speaks NetherNet rather than RakNet. That is deliberate:
//
//   - The backend is a NetherNet server already, so RakNet would mean translating transports for
//     no reason.
//   - It reuses the client-connection path that is known to work. A RakNet client leg has to run
//     Minecraft's own encryption handshake (NetherNet connections skip it, being encrypted by
//     DTLS already), and the RakNet direct-IP build in n2r-selfsignedfix stalls after login in
//     exactly that area - the client falls silent and never spawns.

// DirectIPConfig configures the direct-IP front door.
type DirectIPConfig struct {
	// ListenAddress is the host:port to serve NetherNet HTTP signaling on, e.g. ":19132".
	// This is the address players type, and it is TCP - a Bedrock client probes it with
	// GET /v1/join before it will consider a UDP/RakNet connection.
	ListenAddress string

	// Server-list fields, shown for this entry in the player's server list.
	WorldName  string
	MaxPlayers int
	Protocol   int32
	Version    string

	// Relay is the configuration used for every accepted connection - the same value the
	// Friends-tab listener is served with, so both doors relay identically.
	Relay Config

	Log *slog.Logger
}

// StartDirectIP brings up the direct-IP front door and returns once it is listening. Connections
// are served in the background until ctx is cancelled.
func StartDirectIP(ctx context.Context, cfg DirectIPConfig) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("src", "direct-ip")

	if cfg.ListenAddress == "" {
		return fmt.Errorf("direct-ip listen address is empty")
	}

	motd := &serverListProvider{cfg: cfg, log: log}
	signaling := newHTTPSignalingServer(log, motd.body)

	// Both of these mirror the Friends-tab listener in listener.go, where they are load-bearing
	// workarounds rather than preferences - see the comments there. The 30s connection context
	// replaces go-nethernet's 5s default, which real ICE negotiation routinely overruns; the
	// 4-character ICE ufrag matches what Bedrock's own implementation emits, since pion's
	// 16-character default may exceed what the client's STUN parser accepts.
	connCtx := func(parent context.Context, _ *nethernet.Conn) context.Context {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		// go-nethernet gives no completion callback for a Conn's lifetime, so release the
		// timer when ctx itself ends (success, timeout, or parent cancellation alike) rather
		// than discarding cancel - go vet: lostcancel flagged a live 30s timer per accepted
		// connection that was never being released otherwise.
		context.AfterFunc(ctx, cancel)
		return ctx
	}
	settings := webrtc.SettingEngine{}
	settings.SetICECredentials(randomICEString(4), randomICEString(24))
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))

	listener, err := nethernet.ListenConfig{
		Log:         log,
		ConnContext: connCtx,
		API:         api,
		// Required for HTTP signaling: the entire offer/answer exchange is one request/response,
		// so all local candidates must be gathered before the answer is written.
		DisableTrickleICE: true,
		// AllowAnonymous is left false: a real Bedrock client always presents an identity
		// assertion, and accepting peers without one would remove the binding between an
		// account and its connection.
	}.Listen(signaling)
	if err != nil {
		return fmt.Errorf("start direct-ip nethernet listener: %w", err)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           signaling,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind before returning so a port conflict is a startup error rather than a silent
	// background failure that only shows up as players being unable to connect.
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		listener.Close()
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}

	go func() {
		<-ctx.Done()
		_ = server.Close()
		_ = listener.Close()
	}()

	go func() {
		if err := server.Serve(ln); err != nil && ctx.Err() == nil {
			log.Error("direct-ip signaling server stopped", "err", err)
		}
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() == nil {
					log.Error("direct-ip listener stopped", "err", err)
				}
				return
			}
			nc, ok := conn.(*nethernet.Conn)
			if !ok {
				_ = conn.Close()
				continue
			}
			log.Info("direct-ip connection accepted", "remote", nc.RemoteAddr())
			// Same handler as the Friends-tab path - this is what makes the two doors produce
			// one identity. See this file's header comment.
			go HandleConn(ctx, nc, cfg.Relay)
		}
	}()

	log.Info("direct-ip front door listening (players can join by address)",
		"address", cfg.ListenAddress, "transport", "nethernet")
	return nil
}

// serverListProvider builds the JSON served for GET /v1/join, which is what a player's server
// list shows for this entry.
//
// The live fields (player count especially) are taken from the backend's own GET /v1/join rather
// than guessed, so the entry reflects reality; the name and capacity are overridden from config
// so the list entry matches what the Friends tab advertises for the same server. If the backend
// cannot be reached the entry still renders from config alone, showing an empty server rather
// than vanishing from the list.
type serverListProvider struct {
	cfg DirectIPConfig
	log *slog.Logger

	mu       sync.Mutex
	cached   []byte
	cachedAt time.Time
}

// serverListEntry is the shape real BDS serves, confirmed against a live 1.26.50.5 instance:
//
//	{"name":"...","protocol":2193,"version":"1.26.50","level":"...","players":2,"maxPlayers":30,"gameType":0}
type serverListEntry struct {
	Name       string `json:"name"`
	Protocol   int32  `json:"protocol"`
	Version    string `json:"version"`
	Level      string `json:"level"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"maxPlayers"`
	GameType   int    `json:"gameType"`
}

// serverListCacheTTL keeps a burst of server-list refreshes from turning into a burst of backend
// requests. Short enough that the player count stays believable.
const serverListCacheTTL = 3 * time.Second

func (p *serverListProvider) body(ctx context.Context) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.cachedAt) < serverListCacheTTL {
		return p.cached
	}

	entry := serverListEntry{
		Name:       p.cfg.WorldName,
		Protocol:   p.cfg.Protocol,
		Version:    p.cfg.Version,
		Level:      p.cfg.WorldName,
		MaxPlayers: p.cfg.MaxPlayers,
	}
	if live, err := p.backendEntry(ctx); err != nil {
		p.log.Debug("could not read backend server-list info, serving config values", "err", err)
	} else {
		entry.Players = live.Players
		entry.GameType = live.GameType
		// Trust the backend for protocol/version over config, since a mismatch there is what
		// makes a client refuse to connect and config is the likelier of the two to be stale.
		if live.Protocol != 0 {
			entry.Protocol = live.Protocol
		}
		if live.Version != "" {
			entry.Version = live.Version
		}
	}

	b, err := json.Marshal(entry)
	if err != nil {
		// Marshalling a fixed struct cannot realistically fail, but never serve a nil body.
		b = []byte(`{}`)
	}
	p.cached, p.cachedAt = b, time.Now()
	return b
}

// backendEntry reads the backend's own server-list JSON. Only meaningful when the backend is
// itself a NetherNet server; a RakNet backend has no such endpoint and this simply fails.
func (p *serverListProvider) backendEntry(ctx context.Context) (*serverListEntry, error) {
	if backendTransportName(p.cfg.Relay) != TransportNetherNet {
		return nil, fmt.Errorf("backend is not nethernet")
	}
	address, err := NormalizeNetherNetAddress(p.cfg.Relay.NetherNetBackendAddress)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/v1/join", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "libhttpclient/1.0.0.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %s", resp.Status)
	}
	entry := new(serverListEntry)
	if err := json.NewDecoder(resp.Body).Decode(entry); err != nil {
		return nil, fmt.Errorf("decode backend server-list json: %w", err)
	}
	return entry, nil
}
