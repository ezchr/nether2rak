package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/df-mc/go-nethernet"
	"github.com/ezchr/nether2rak/bridge"
	"github.com/ezchr/nether2rak/session"
)

// network is the relay's server network (config "servers"), or nil for a relay into one server.
var network *bridge.Network

// setupNetwork builds the network from the config. Relay mode only: the relay has to stay in the
// player's connection to move them.
func setupNetwork(cfg FileConfig, authSession *session.Session, log *slog.Logger) (*bridge.Network, error) {
	if len(cfg.Servers) == 0 {
		return nil, nil
	}
	if strings.EqualFold(strings.TrimSpace(cfg.BackendTransport), bridge.TransportNetherNetNoRelay) {
		return nil, errors.New(`"servers" needs backend_transport "nethernet" (relay mode): in no-relay mode the relay is not in the connection, so it can't move players`)
	}
	seen := map[string]bool{}
	for i, s := range cfg.Servers {
		if s.Name == "" || s.Address == "" {
			return nil, fmt.Errorf("servers[%d] needs a name and an address", i)
		}
		if seen[strings.ToLower(s.Name)] {
			return nil, fmt.Errorf("two servers are called %q", s.Name)
		}
		seen[strings.ToLower(s.Name)] = true
		if !strings.EqualFold(s.Transport, bridge.TransportRakNet) {
			addr, err := bridge.NormalizeNetherNetAddress(s.Address)
			if err != nil {
				return nil, fmt.Errorf("server %s: %w", s.Name, err)
			}
			cfg.Servers[i].Address = addr
		}
	}
	def := cfg.DefaultServer
	if def == "" {
		def = cfg.Servers[0].Name
	}
	dir := cfg.NetworkDir
	if dir == "" {
		dir = "network"
	}
	admins := map[string]bool{}
	for _, a := range cfg.Admins {
		admins[strings.ToLower(strings.TrimSpace(a))] = true
	}
	n := &bridge.Network{
		Servers: cfg.Servers,
		Default: def,
		Dir:     dir,
		Identity: func(ctx context.Context) (*nethernet.Identity, error) {
			return bridge.NewClientIdentity(ctx, authSession)
		},
		Admins:        admins,
		Log:           log,
		RejoinAddress: cfg.RejoinAddress,
	}
	if listing != nil {
		n.Listing = listing
	}
	if err := n.Init(); err != nil {
		return nil, err
	}
	n.FetchAllPacks()
	return n, nil
}

// listing switches the Friends-tab listing (every broadcast of this relay) on and off: config
// "listing_toggle". Off, the relay keeps no Xbox Live session, so the world is not in anyone's
// Friends tab; players already in stay, and direct IP joins keep working. The state survives
// restarts (listing_state.json). nil when the toggle is not configured: always listed.
var listing *listingGate

var errListingOff = errors.New("the Friends-tab listing was switched off")

type listingGate struct {
	mu   sync.Mutex
	on   bool
	path string
	// onCh is closed when the listing goes on, offCh when it goes off; each is replaced once used.
	onCh, offCh chan struct{}
	log         *slog.Logger
}

func newListingGate(path string, log *slog.Logger) *listingGate {
	g := &listingGate{path: path, onCh: make(chan struct{}), offCh: make(chan struct{}), log: log}
	var st struct {
		On bool `json:"on"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &st) == nil {
		g.on = st.On
	}
	if g.on {
		close(g.onCh)
	}
	return g
}

func (g *listingGate) On() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.on
}

func (g *listingGate) Set(on bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.on == on {
		return nil
	}
	b, _ := json.Marshal(map[string]bool{"on": on})
	if err := os.WriteFile(g.path, b, 0o644); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	g.on = on
	if on {
		close(g.onCh)
		g.offCh = make(chan struct{})
	} else {
		close(g.offCh)
		g.onCh = make(chan struct{})
	}
	g.log.Info("friends-tab listing switched", "on", on)
	return nil
}

// waitOn blocks until the listing is on.
func (g *listingGate) waitOn(ctx context.Context) error {
	g.mu.Lock()
	ch := g.onCh
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// offSignal is closed when the listing next goes off.
func (g *listingGate) offSignal() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.offCh
}

// serveNetworkControl answers the control port's network and listing routes. It reports whether
// the path was one of them.
//
//	/listing[/status] /listing/on /listing/off   the Friends-tab listing
//	/servers                                     the servers and who is on which
//	/switch?xuid=X&server=NAME                   move a player
func serveNetworkControl(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch {
	case path == "/listing" || path == "/listing/status" || path == "/listing/on" || path == "/listing/off":
		if listing == nil {
			http.Error(w, "the Friends-tab listing toggle is off for this relay (config listing_toggle)", http.StatusNotFound)
			return true
		}
		switch path {
		case "/listing/on":
			if err := listing.Set(true); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return true
			}
		case "/listing/off":
			if err := listing.Set(false); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return true
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"on": listing.On()})
		return true
	case path == "/servers":
		if network == nil {
			http.Error(w, "this relay has no servers list", http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": network.ServerNames(), "default": network.Default, "players": network.Sessions()})
		return true
	case path == "/switch":
		if network == nil {
			http.Error(w, "this relay has no servers list", http.StatusNotFound)
			return true
		}
		if err := network.Switch(r.URL.Query().Get("xuid"), r.URL.Query().Get("server")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		_, _ = w.Write([]byte("moving\n"))
		return true
	}
	return false
}

// directIPExtra is the control server when it shares the direct-IP door (control_on_direct_ip).
func directIPExtra(cfg FileConfig, control *controlServer) http.Handler {
	if !cfg.ControlOnDirectIP {
		return nil
	}
	return control
}
