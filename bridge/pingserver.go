// Package bridge - pingserver.go exposes each connected player's real NetherNet round-trip
// time over a tiny loopback-only HTTP API, so the Folia-side PingDisplay plugin can show a
// Bedrock player's actual device-to-VPS latency instead of the ~1-4ms Geyser reports (which
// only measures the local relay hop between nether2rak and Geyser, both on the same machine -
// see the conversation this was built from for the full explanation).
package bridge

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
)

var (
	pingRegistryMu sync.Mutex
	pingRegistry   = map[string]*nethernet.Conn{}
)

// registerPing makes conn's live latency queryable by xuid until unregisterPing is called.
func registerPing(xuid string, conn *nethernet.Conn) {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	pingRegistry[xuid] = conn
}

// unregisterPing removes xuid from the ping registry. Call this when a connection ends.
func unregisterPing(xuid string) {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	delete(pingRegistry, xuid)
}

// ConnectedPlayerCount returns the number of players currently relayed through this process.
// It's a byproduct of the ping registry above (populated/cleared in the exact same
// register/unregisterPing calls that bracket each connection's lifetime in HandleConn), reused
// here instead of tracking a second, separate counter that could drift out of sync with it.
func ConnectedPlayerCount() int {
	pingRegistryMu.Lock()
	defer pingRegistryMu.Unlock()
	return len(pingRegistry)
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
		conn, ok := pingRegistry[xuid]
		pingRegistryMu.Unlock()
		if !ok {
			http.Error(w, "not connected", http.StatusNotFound)
			return
		}
		// Latency() returns half the SCTP SRTT; double it back to approximate a full
		// round trip, comparable to what Java's keepalive-based ping measures.
		rttMs := conn.Latency().Milliseconds() * 2
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"ms": rttMs})
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
