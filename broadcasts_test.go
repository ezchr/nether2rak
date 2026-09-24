package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextTokenFileSkipsExistingAndPrimary(t *testing.T) {
	dir := t.TempDir()
	if got := nextTokenFile(dir); got != "token2.json" {
		t.Fatalf("empty dir: want token2.json, got %q", got)
	}
	// token.json (the primary) existing must not affect the result - it's never a candidate.
	os.WriteFile(filepath.Join(dir, "token.json"), []byte("{}"), 0600)
	if got := nextTokenFile(dir); got != "token2.json" {
		t.Fatalf("with only token.json present: want token2.json, got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "token2.json"), []byte("{}"), 0600)
	if got := nextTokenFile(dir); got != "token3.json" {
		t.Fatalf("with token2.json taken: want token3.json, got %q", got)
	}
	// gap: token3 present, token4 free after removing an intermediate file's sibling check
	os.WriteFile(filepath.Join(dir, "token3.json"), []byte("{}"), 0600)
	os.WriteFile(filepath.Join(dir, "token5.json"), []byte("{}"), 0600) // out-of-order, must not confuse it
	if got := nextTokenFile(dir); got != "token4.json" {
		t.Fatalf("with token2,3,5 taken: want the first gap token4.json, got %q", got)
	}
}

func TestResolveBroadcastsDefaultsAndValidation(t *testing.T) {
	cfg := defaultConfig()
	cfg.HostName, cfg.WorldName, cfg.InvitePort = "chrisgg", "Dragonfly Server", 7783
	cfg.ExtraBroadcasts = []BroadcastConfig{
		{TokenFile: "token2.json"}, // defaults filled in
		{Name: "alt", TokenFile: "token3.json", WorldName: "Alt World", InvitePort: 7790},
		{TokenFile: ""}, // no token file
		{Name: "dupefile", TokenFile: "token2.json"},                    // reuses extra-1's file
		{Name: "usesprimary", TokenFile: "token.json"},                  // reuses the primary's file
		{Name: "alt", TokenFile: "token9.json"},                         // name taken
		{Name: "portclash", TokenFile: "token4.json", InvitePort: 7783}, // primary's port
	}
	got, problems := resolveBroadcasts(cfg)

	if len(got) != 3 {
		t.Fatalf("want 3 usable broadcasts, got %d: %+v", len(got), got)
	}
	if got[0].Name != "extra-1" || got[0].HostName != "chrisgg" || got[0].WorldName != "Dragonfly Server" || got[0].InvitePort != 0 {
		t.Errorf("defaults not applied: %+v", got[0])
	}
	if got[1].Name != "alt" || got[1].WorldName != "Alt World" || got[1].HostName != "chrisgg" || got[1].InvitePort != 7790 {
		t.Errorf("explicit values not kept: %+v", got[1])
	}
	if got[2].Name != "portclash" || got[2].InvitePort != 0 {
		t.Errorf("a clashing invite_port must be turned off, not kept: %+v", got[2])
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"token_file is empty", `"token2.json" is already used by extra-1`, `"token.json" is already used by primary`, `name "alt" is already used`, "invite_port 7783 is already used by primary"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func TestResolveBroadcastsNoneConfigured(t *testing.T) {
	got, problems := resolveBroadcasts(defaultConfig())
	if len(got) != 0 || len(problems) != 0 {
		t.Fatalf("no extra_broadcasts must mean nothing extra: %v %v", got, problems)
	}
}

func TestForBroadcastOverridesOnlyItsOwnFields(t *testing.T) {
	cfg := defaultConfig()
	cfg.HostName, cfg.WorldName, cfg.InvitePort, cfg.MaxPlayers = "h", "w", 7783, 30
	cfg.ExtraBroadcasts = []BroadcastConfig{{TokenFile: "x"}}
	b := BroadcastConfig{Name: "b", TokenFile: "t2.json", HostName: "h2", WorldName: "w2"}
	c := b.forBroadcast(cfg)
	if c.HostName != "h2" || c.WorldName != "w2" || c.InvitePort != 0 {
		t.Errorf("broadcast fields not applied: %+v", c)
	}
	if c.MaxPlayers != 30 || c.BackendTransport != cfg.BackendTransport || c.Protocol != cfg.Protocol {
		t.Errorf("shared fields must carry over unchanged")
	}
	if c.ExtraBroadcasts != nil {
		t.Errorf("a broadcast's own config must not list extra broadcasts")
	}
	if cfg.HostName != "h" {
		t.Errorf("forBroadcast must not modify the shared config")
	}
}

func TestClaimXUIDOneBroadcastPerAccount(t *testing.T) {
	if _, ok := claimXUID("test-xuid-1", "primary"); !ok {
		t.Fatal("first claim must succeed")
	}
	if owner, ok := claimXUID("test-xuid-1", "extra-1"); ok || owner != "primary" {
		t.Fatalf("second claim of the same account must fail and name the owner, got %q %v", owner, ok)
	}
	if _, ok := claimXUID("test-xuid-2", "extra-1"); !ok {
		t.Fatal("a different account must be claimable")
	}
}
