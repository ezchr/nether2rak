package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFrom(t *testing.T, body string) FileConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hasNote(cfg FileConfig, part string) bool {
	for _, n := range cfg.Notes {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func TestServerAddressUsedForEveryTransport(t *testing.T) {
	cfg := loadFrom(t, `{"backend_transport":"nethernet-norelay","server_address":"1.2.3.4:19134"}`)
	if cfg.NetherNetBackendAddress != "1.2.3.4:19134" {
		t.Fatalf("nethernet address = %q", cfg.NetherNetBackendAddress)
	}
	cfg = loadFrom(t, `{"backend_transport":"raknet","server_address":"1.2.3.4:19132"}`)
	if cfg.GeyserAddress != "1.2.3.4:19132" {
		t.Fatalf("raknet address = %q", cfg.GeyserAddress)
	}
	if len(cfg.Notes) != 0 {
		t.Fatalf("a clean config should give no notes, got %v", cfg.Notes)
	}
}

func TestOldKeysStillWork(t *testing.T) {
	// The live ZID config before server_address existed.
	cfg := loadFrom(t, `{"backend_transport":"nethernet-norelay","geyser_address":"127.0.0.1:19142",
		"nethernet_backend_address":"http://127.0.0.1:19134"}`)
	if cfg.ServerAddress != "http://127.0.0.1:19134" || cfg.NetherNetBackendAddress != "http://127.0.0.1:19134" {
		t.Fatalf("server %q nethernet %q", cfg.ServerAddress, cfg.NetherNetBackendAddress)
	}
	if !hasNote(cfg, `"nethernet_backend_address"`) || !hasNote(cfg, `"geyser_address" is ignored`) {
		t.Fatalf("expected a rename note and an ignored note, got %v", cfg.Notes)
	}

	cfg = loadFrom(t, `{"geyser_address":"10.0.0.5:19142"}`)
	if cfg.GeyserAddress != "10.0.0.5:19142" || cfg.ServerAddress != "10.0.0.5:19142" {
		t.Fatalf("raknet via old key: server %q geyser %q", cfg.ServerAddress, cfg.GeyserAddress)
	}
}

func TestServerAddressWinsOverOldKey(t *testing.T) {
	cfg := loadFrom(t, `{"backend_transport":"nethernet","server_address":"5.5.5.5:1",
		"nethernet_backend_address":"http://9.9.9.9:2"}`)
	if cfg.NetherNetBackendAddress != "5.5.5.5:1" {
		t.Fatalf("server_address should win, got %q", cfg.NetherNetBackendAddress)
	}
	if !hasNote(cfg, `"nethernet_backend_address" is ignored because "server_address" is set`) {
		t.Fatalf("notes %v", cfg.Notes)
	}
}

func TestDefaultsFollowTransport(t *testing.T) {
	if cfg := loadFrom(t, `{"backend_transport":"nethernet-norelay"}`); cfg.NetherNetBackendAddress != defaultNetherNetAddress {
		t.Fatalf("nethernet default = %q", cfg.NetherNetBackendAddress)
	}
	if cfg := loadFrom(t, `{}`); cfg.GeyserAddress != defaultRakNetAddress {
		t.Fatalf("raknet default = %q", cfg.GeyserAddress)
	}
}

func TestRakNetDropsURLScheme(t *testing.T) {
	cfg := loadFrom(t, `{"backend_transport":"raknet","server_address":"http://1.2.3.4:19132/"}`)
	if cfg.GeyserAddress != "1.2.3.4:19132" {
		t.Fatalf("got %q", cfg.GeyserAddress)
	}
}

func TestNoRelayNotesUnusedRelaySettings(t *testing.T) {
	cfg := loadFrom(t, `{"backend_transport":"nethernet-norelay","server_address":"a:1",
		"compression":"snappy","fix_native_bds_persistence":true,"relay_direct_ip":false,
		"relay_direct_ip_listen_address":"127.0.0.1:19135"}`)
	for _, key := range []string{`"compression"`, `"fix_native_bds_persistence"`, `"relay_direct_ip_listen_address"`} {
		if !hasNote(cfg, key) {
			t.Fatalf("expected a note for %s, got %v", key, cfg.Notes)
		}
	}
	// In a relaying mode the same settings matter, so they get no note.
	cfg = loadFrom(t, `{"backend_transport":"nethernet","server_address":"a:1","compression":"snappy"}`)
	if hasNote(cfg, `"compression"`) {
		t.Fatalf("compression is used in relay mode, got %v", cfg.Notes)
	}
}

func TestFreshConfigWritesServerAddressOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeyserAddress != defaultRakNetAddress {
		t.Fatalf("runtime address = %q", cfg.GeyserAddress)
	}
	b, _ := os.ReadFile(path)
	body := string(b)
	if !strings.Contains(body, `"server_address": "127.0.0.1:19132"`) ||
		strings.Contains(body, "geyser_address") || strings.Contains(body, "nethernet_backend_address") {
		t.Fatalf("written default:\n%s", body)
	}
}
