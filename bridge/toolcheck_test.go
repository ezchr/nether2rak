package bridge

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"
)

func TestRadiusSignals(t *testing.T) {
	for _, c := range []struct {
		radius int32
		max    uint8
		want   []string
	}{
		// Honest clients: a view distance at or below their own maximum (14/14, 7/12 and 7/13
		// were measured on real iOS and Android clients).
		{14, 14, nil},
		{7, 12, nil},
		{32, 32, nil},
		{96, 96, []string{ruleRadius}},
		// bedrocktool: its own request, and a rewritten one of the client (80/28 was seen live).
		{76, 0, []string{ruleMaxZero, ruleRadius}},
		{80, 28, []string{ruleContradiction, ruleRadius}},
		{8, 0, []string{ruleMaxZero}},
	} {
		if got := radiusSignals(c.radius, c.max); !reflect.DeepEqual(got, c.want) {
			t.Errorf("radius %d, max %d: %v, want %v", c.radius, c.max, got, c.want)
		}
	}
}

func TestParseToolRules(t *testing.T) {
	def := parseToolRules("")
	want := map[string]int{ruleContradiction: toolKick, ruleMaxZero: toolKick, ruleRadius: toolFlag}
	if !reflect.DeepEqual(def, want) {
		t.Errorf("defaults = %v, want %v", def, want)
	}
	for rule, m := range parseToolRules("off") {
		if m != toolOff {
			t.Errorf("off: %s = %d", rule, m)
		}
	}
	// title, notitle and notitle_maxzero were rules once; an old N2R_TOOLCHECK naming them must
	// not break anything.
	mixed := parseToolRules("maxzero=flag, title=off, radius=kick, notitle=kick, notitle_maxzero=kick, nonsense=kick, contradiction=what, MaxZero = FLAG")
	want = map[string]int{ruleContradiction: toolKick, ruleMaxZero: toolFlag, ruleRadius: toolFlag}
	if !reflect.DeepEqual(mixed, want) {
		t.Errorf("mixed = %v, want %v", mixed, want)
	}
	if all := parseToolRules("kick"); all[ruleMaxZero] != toolKick || all[ruleRadius] != toolFlag {
		t.Errorf("bare kick = %v", all)
	}
	if parseToolRules("log")[ruleContradiction] != toolFlag {
		t.Error("log should mean flag")
	}
}

func TestToolVerdict(t *testing.T) {
	old := toolRules
	defer func() { toolRules = old }()
	toolRules = parseToolRules("contradiction=kick,maxzero=flag,radius=off")
	if active, kick := toolVerdict([]string{ruleMaxZero, ruleRadius}); kick || !reflect.DeepEqual(active, []string{ruleMaxZero}) {
		t.Errorf("flag only: %v, kick %v", active, kick)
	}
	if active, kick := toolVerdict([]string{ruleContradiction, ruleRadius}); !kick || !reflect.DeepEqual(active, []string{ruleContradiction}) {
		t.Errorf("kick: %v, kick %v", active, kick)
	}
	if active, kick := toolVerdict(nil); kick || active != nil {
		t.Errorf("nothing: %v, kick %v", active, kick)
	}
}

func TestToolAllowed(t *testing.T) {
	t.Chdir(t.TempDir())
	if toolAllowed("2535400000000101") {
		t.Error("allowed without a file")
	}
	if err := os.WriteFile(toolAllowFile, []byte("# admins\n2535400000000101 # an admin\n\n  2535400000000102\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for xuid, want := range map[string]bool{"2535400000000101": true, "2535400000000102": true, "2535400000000000": false, "": false} {
		if got := toolAllowed(xuid); got != want {
			t.Errorf("%q allowed = %v, want %v", xuid, got, want)
		}
	}
}

func TestFlagPlayer(t *testing.T) {
	t.Chdir(t.TempDir())
	read := func() map[string]flaggedPlayer {
		b, err := os.ReadFile(toolFlagFile)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]flaggedPlayer
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := flagPlayer("1", "Alex", []string{ruleRadius}, "first"); err != nil {
		t.Fatal(err)
	}
	if err := flagPlayer("1", "Alex2", []string{ruleRadius, ruleMaxZero}, "second"); err != nil {
		t.Fatal(err)
	}
	if err := flagPlayer("2", "Sam", []string{ruleContradiction}, "other"); err != nil {
		t.Fatal(err)
	}
	if err := flagPlayer("", "nobody", []string{ruleContradiction}, ""); err != nil {
		t.Fatal(err)
	}
	if err := flagPlayer("3", "clean", nil, ""); err != nil {
		t.Fatal(err)
	}
	m := read()
	if len(m) != 2 {
		t.Fatalf("%d players flagged, want 2: %v", len(m), m)
	}
	a := m["1"]
	if a.Name != "Alex2" || a.Rules[ruleRadius] != 2 || a.Rules[ruleMaxZero] != 1 || a.Detail != "second" || a.First == "" || a.Last == "" {
		t.Errorf("player 1 = %+v", a)
	}
	// A file that cannot be read is kept aside, not overwritten.
	if err := os.WriteFile(toolFlagFile, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := flagPlayer("4", "New", []string{ruleContradiction}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(toolFlagFile + ".unreadable"); err != nil {
		t.Errorf("the unreadable file was not kept: %v", err)
	}
	if m := read(); len(m) != 1 || m["4"].Name != "New" {
		t.Errorf("after an unreadable file: %v", m)
	}
}

func TestToolExempt(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:19136": true,
		"[::1]:5000":      true,
		"8.8.8.8:1234":    false,
	} {
		a, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := toolExempt(a); got != want {
			t.Errorf("%s exempt = %v, want %v", addr, got, want)
		}
	}
	if toolExempt(nil) {
		t.Error("no address is not exempt")
	}
	// NetherNet addresses are text; the resolved peer address comes last.
	for text, want := range map[string]bool{
		"123 (456) (udp4 prflx 203.0.113.7:63264 related :0 (resolved: 203.0.113.7:63264))": false,
		"123 (456) (udp4 host 10.0.0.5:1 (resolved: 127.0.0.1:57207))":                            true,
		"no address here": false,
	} {
		if got := toolExempt(textAddr(text)); got != want {
			t.Errorf("%q exempt = %v, want %v", text, got, want)
		}
	}
}

type textAddr string

func (textAddr) Network() string  { return "nethernet" }
func (a textAddr) String() string { return string(a) }
