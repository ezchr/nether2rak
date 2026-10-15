package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
)

// Checks for bedrocktool, a proxy that saves every chunk a server sends into a world file. They
// catch the tool as people download it today, not world copying in general: a server has to send
// its chunks, and the tool is open source.
//
//   - radius: a very large view distance (the tool forces 76).
//   - contradiction: a view distance above the maximum the same packet states.
//   - maxzero: no maximum at all. The tool's own request leaves it out; every real client sends one.
//   - client: the login's client data is gophertunnel's fill-in rather than the game's: no device
//     model, or gophertunnel's all-black default skin. Tools built on gophertunnel send these unless
//     they copy a real client's data. It replaces the old check on the Xbox title, which current
//     logins no longer carry: the multiplayer token names only the PlayFab title, the same for
//     every copy of the game, and the old certificate chain with the Xbox title is not sent.
//
// Every rule a player trips is logged and flags the player for good, in toolcheck_flags.json in
// the working directory (by XUID: name, which rules, how often, first and last time). A rule set
// to kick also disconnects them. N2R_TOOLCHECK sets what each rule does, for example
// "contradiction=kick,maxzero=flag,notitle=off"; a value is off, flag or kick ("log" means flag),
// and a bare value sets all of them. Unset rules keep their default. radius and notitle never
// kick. Players whose XUID is a line of toolcheck_allow.txt (read at every check, "#" starts a
// comment) are never checked.

const (
	toolOff = iota
	toolFlag
	toolKick
)

const (
	ruleRadius        = "radius"
	ruleContradiction = "contradiction"
	ruleMaxZero       = "maxzero"
	ruleClient        = "client"
)

const (
	// toolAllowFile lists the XUIDs that are never checked, one a line.
	toolAllowFile = "toolcheck_allow.txt"
	// toolFlagFile holds the flagged players.
	toolFlagFile = "toolcheck_flags.json"
	// toolKickMessage does not say which check it was.
	toolKickMessage = "Flagged by anticheat"
	// suspiciousRadius is the view distance, in chunks, from which a request is flagged.
	suspiciousRadius = 64
)

// toolRules is what each rule does. The contradiction and a missing maximum kick by default: no real
// client was seen doing either (measured 2026-10-15 over the relays' logs).
var toolRules = parseToolRules(os.Getenv("N2R_TOOLCHECK"))

func parseToolRules(v string) map[string]int {
	rules := map[string]int{ruleRadius: toolFlag, ruleContradiction: toolKick, ruleMaxZero: toolKick, ruleClient: toolFlag}
	mode := func(s string) (int, bool) {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "off":
			return toolOff, true
		case "flag", "log":
			return toolFlag, true
		case "kick":
			return toolKick, true
		}
		return 0, false
	}
	for _, part := range strings.Split(v, ",") {
		name, value, pair := strings.Cut(part, "=")
		if !pair {
			if m, ok := mode(part); ok {
				for rule := range rules {
					rules[rule] = m
				}
			}
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if _, known := rules[name]; !known {
			continue
		}
		if m, ok := mode(value); ok {
			rules[name] = m
		}
	}
	// A large view distance alone is never a reason to kick, and the client rule only flags until
	// real clients were measured against it.
	for _, rule := range []string{ruleRadius, ruleClient} {
		if rules[rule] == toolKick {
			rules[rule] = toolFlag
		}
	}
	return rules
}

// toolVerdict drops the signals whose rule is off, and reports whether one of the rest kicks.
func toolVerdict(signals []string) (active []string, kick bool) {
	for _, rule := range signals {
		switch toolRules[rule] {
		case toolKick:
			kick = true
			active = append(active, rule)
		case toolFlag:
			active = append(active, rule)
		}
	}
	return active, kick
}

// radiusSignals are the rules a view-distance request trips.
func radiusSignals(radius int32, max uint8) []string {
	var out []string
	switch {
	case max == 0:
		out = append(out, ruleMaxZero)
	case radius > int32(max):
		out = append(out, ruleContradiction)
	}
	if radius >= suspiciousRadius {
		out = append(out, ruleRadius)
	}
	return out
}

// defaultSkinData is the skin gophertunnel's dialer fills in when a client sets none: 64x32 pixels,
// all black.
var defaultSkinData = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0, 0, 0, 255}, 32*64))

// clientSignals are the rules a login's client data trips, with what was seen.
func clientSignals(cd login.ClientData) (signals []string, seen string) {
	var why []string
	if strings.TrimSpace(cd.DeviceModel) == "" {
		why = append(why, "no device model")
	}
	if cd.SkinData == defaultSkinData {
		why = append(why, "default skin")
	}
	if len(why) == 0 {
		return nil, ""
	}
	return []string{ruleClient}, strings.Join(why, ", ")
}

// toolAllowed reports whether the XUID is listed in toolAllowFile.
func toolAllowed(xuid string) bool {
	if xuid == "" {
		return false
	}
	b, err := os.ReadFile(toolAllowFile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if strings.TrimSpace(line) == xuid {
			return true
		}
	}
	return false
}

// flaggedPlayer is one player of toolFlagFile.
type flaggedPlayer struct {
	Name string `json:"name"`
	// Rules counts how often the player tripped each rule.
	Rules  map[string]int `json:"rules"`
	First  string         `json:"first"`
	Last   string         `json:"last"`
	Detail string         `json:"detail"` // what was seen the last time
}

var toolFlagMu sync.Mutex

// flagPlayer records, for good, that the player tripped the rules passed. detail says what was
// seen. An error is returned for the caller to log; the check itself goes on.
func flagPlayer(xuid, name string, rules []string, detail string) error {
	if xuid == "" || len(rules) == 0 {
		return nil
	}
	toolFlagMu.Lock()
	defer toolFlagMu.Unlock()
	flagged := map[string]*flaggedPlayer{}
	if b, err := os.ReadFile(toolFlagFile); err == nil {
		if err := json.Unmarshal(b, &flagged); err != nil {
			// Never overwrite a file that could not be read: keep it aside.
			_ = os.Rename(toolFlagFile, toolFlagFile+".unreadable")
			flagged = map[string]*flaggedPlayer{}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	p := flagged[xuid]
	if p == nil {
		p = &flaggedPlayer{Rules: map[string]int{}, First: now}
		flagged[xuid] = p
	}
	if p.Rules == nil {
		p.Rules = map[string]int{}
	}
	p.Name, p.Last, p.Detail = name, now, detail
	for _, rule := range rules {
		p.Rules[rule]++
	}
	b, err := json.MarshalIndent(flagged, "", "  ")
	if err != nil {
		return err
	}
	tmp := toolFlagFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, toolFlagFile)
}

// addrIP matches the IP addresses in the text of a connection address.
var addrIP = regexp.MustCompile(`\[([0-9a-fA-F:.]+)\]|(\d{1,3}(?:\.\d{1,3}){3})`)

// toolExempt reports whether a connection from addr is one of ours: this machine's own services
// (the relay, bots, the editor), which connect from loopback or from one of its own addresses. A
// NetherNet address is text with the resolved address of the peer last, so the last IP counts.
func toolExempt(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	found := addrIP.FindAllStringSubmatch(addr.String(), -1)
	if len(found) == 0 {
		return false
	}
	last := found[len(found)-1]
	ip := net.ParseIP(last[1] + last[2])
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	own, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range own {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
