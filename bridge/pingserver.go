// Package bridge - pingserver.go exposes each connected player's real end-to-end round-trip
// time over a tiny loopback-only HTTP API, so the Folia-side PingDisplay plugin can show a
// Bedrock player's actual latency instead of Geyser's own Player#getPing(), which for a
// relayed/Floodgate player only measures the local loopback hop between nether2rak and the
// backend, not anything the player's real device experiences.
package bridge

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// latencyReporter is satisfied by both *nethernet.Conn and *raknet.Conn - each backend transport
// this relay supports exposes its own rolling RTT this same way, so a single registry/reporting
// path can cover the client leg and either kind of backend leg without needing to know which
// concrete type it's holding.
type latencyReporter interface {
	Latency() time.Duration
}

// connLatencies is what's tracked per connected player: the client-facing leg is always present
// once ReadLoop succeeds; the backend leg is filled in once dialBackend succeeds, a moment later
// in HandleConn - see registerBackendPing's doc comment for why these are two separate calls
// rather than one.
type connLatencies struct {
	client  latencyReporter
	backend latencyReporter // nil until registerBackendPing is called, absent entirely for a
	// connection that fails before reaching the backend dial
}

var (
	pingRegistryMu sync.Mutex
	pingRegistry   = map[string]*connLatencies{}
)

// registerPing makes conn's live client-leg latency queryable by xuid until unregisterPing is
// called with the returned entry. Call this as soon as the client leg is established - the
// backend hasn't necessarily been dialed yet at that point, which is why the backend leg is
// filled in separately by registerBackendPing rather than requiring both up front.
//
// The returned *connLatencies is this call's own entry, not just xuid, so a caller's deferred
// unregisterPing removes only the entry it created - see unregisterPing's doc comment for why
// that distinction matters.
func registerPing(xuid string, conn latencyReporter) *connLatencies {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	l := &connLatencies{client: conn}
	pingRegistry[xuid] = l
	return l
}

// registerBackendPing records the backend leg's connection once it's been dialed, so /ping can
// report the real end-to-end round trip (client-to-relay plus relay-to-backend) instead of just
// the client leg. A no-op if xuid was never registered via registerPing first (shouldn't happen
// in practice - HandleConn always calls registerPing before dialing the backend - but this must
// not panic if it somehow does).
func registerBackendPing(xuid string, conn latencyReporter) {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	if l, ok := pingRegistry[xuid]; ok {
		l.backend = conn
	}
}

// unregisterPing removes xuid from the ping registry, but only if entry is still the one
// currently registered under it. Call this when a connection ends.
//
// Deleting by bare xuid unconditionally previously let a player reconnecting before their OLD
// connection finished cleaning up have their NEW entry deleted by the OLD connection's deferred
// call - the registry would go blind for an actively connected player until their next
// reconnect. Comparing against the specific entry registerPing returned closes that race: an
// unregisterPing call only ever removes the entry it is actually responsible for.
func unregisterPing(xuid string, entry *connLatencies) {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	if pingRegistry[xuid] == entry {
		delete(pingRegistry, xuid)
	}
}

// StartPingServer starts the loopback-only HTTP ping API in the background. addr should be a
// 127.0.0.1 address - this is intentionally not reachable off the VPS.
func StartPingServer(addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		xuid := r.URL.Query().Get("xuid")
		if xuid == "" {
			http.Error(w, "missing xuid", http.StatusBadRequest)
			return
		}
		pingRegistryMu.Lock()
		l, ok := pingRegistry[xuid]
		pingRegistryMu.Unlock()
		if !ok {
			http.Error(w, "not connected", http.StatusNotFound)
			return
		}
		// Both go-nethernet's Conn.Latency() and go-raknet's Conn.Latency() return half the
		// rolling-average RTT (confirmed directly in both libraries' own doc comments/source,
		// not assumed) - double each back to a full round trip before summing, rather than
		// summing the halves and doubling once, since that would be wrong the moment only one
		// leg is present (see below).
		clientMs := l.client.Latency().Milliseconds() * 2
		totalMs := clientMs
		var backendMs int64
		if l.backend != nil {
			// The backend leg may still be nil very briefly after registerPing but before
			// dialBackend succeeds (or forever, if the connection fails before reaching the
			// backend at all) - report just the client leg in that case rather than blocking or
			// returning an error, since that's still a real, useful number on its own.
			backendMs = l.backend.Latency().Milliseconds() * 2
			totalMs += backendMs
		}
		w.Header().Set("Content-Type", "application/json")
		// client_ms / backend_ms are ms's two legs, for telling where a high reading comes from.
		_ = json.NewEncoder(w).Encode(map[string]int64{"ms": totalMs, "client_ms": clientMs, "backend_ms": backendMs})
	})

	server := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("could not start ping server", "addr", addr, "err", err)
		return
	}
	log.Info("ping server listening", "addr", addr)
	go func() {
		if err := server.Serve(ln); err != nil {
			log.Warn("ping server stopped", "err", err)
		}
	}()
}

// ConnectedXUIDs returns the XUID of every player currently connected through this relay's
// client-facing listener (Friends-tab or direct-IP - whichever door they came through, both call
// registerPing the same way in HandleConn/directip.go). Exported for the invite features
// (invitewatcher.go, friendsinviter.go) to skip anyone already in the world rather than send
// them a pointless invite to somewhere they already are.
//
// This does NOT see a player who reached the backend directly, bypassing the relay entirely -
// only the relay's own connections are tracked here. That's the right scope for the invite
// features, since their whole job is getting people INTO the relay in the first place.
func ConnectedXUIDs() map[string]bool {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	xuids := make(map[string]bool, len(pingRegistry))
	for xuid := range pingRegistry {
		xuids[xuid] = true
	}
	return xuids
}
