package bridge

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Datacenter block, shared with Velocity's DcBlock plugin: joins from hosting/datacenter addresses
// (scanners on rented servers) are turned away, Microsoft's ranges always get through (the Xbox
// relays a player behind a strict NAT comes through are in Azure), and every join attempt is logged.
// The lists are in dcBlockDir (update.sh refreshes them daily): block.txt, allow.txt. Allow wins.

var dcBlockDir = func() string {
	if d := os.Getenv("N2R_DCBLOCK_DIR"); d != "" {
		return d
	}
	return "/root/mcjava/dcblock"
}()

const dcBlockMessage = "Connections from hosting/datacenter networks aren't allowed. Join from your own internet connection."

type dcLists struct {
	mu           sync.Mutex
	block, allow []*net.IPNet
	stamp        [2]time.Time
	checked      time.Time
}

var dc dcLists

func readNets(path string) ([]*net.IPNet, time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	var out []*net.IPNet
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(strings.SplitN(sc.Text(), "#", 2)[0])
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out, st.ModTime(), sc.Err()
}

// refresh re-reads a list whose file changed, at most every 10 minutes. dc.mu held.
func (d *dcLists) refresh() {
	if !d.checked.IsZero() && time.Since(d.checked) < 10*time.Minute {
		return
	}
	d.checked = time.Now()
	for i, name := range []string{"block.txt", "allow.txt"} {
		p := filepath.Join(dcBlockDir, name)
		st, err := os.Stat(p)
		if err != nil || st.ModTime().Equal(d.stamp[i]) {
			continue
		}
		nets, mod, err := readNets(p)
		if err != nil {
			continue
		}
		if i == 0 {
			d.block = nets
		} else {
			d.allow = nets
		}
		d.stamp[i] = mod
	}
}

func firstMatch(nets []*net.IPNet, ip net.IP) *net.IPNet {
	for _, n := range nets {
		if n.Contains(ip) {
			return n
		}
	}
	return nil
}

// remoteIP reads the address a NetherNet connection came from (the last "ip:port" in its
// description, as toolExempt does) and the ICE candidate type (host, srflx, prflx, relay).
func remoteIP(addr net.Addr) (net.IP, string) {
	if addr == nil {
		return nil, ""
	}
	s := addr.String()
	kind := ""
	for _, k := range []string{" host ", " srflx ", " prflx ", " relay "} {
		if strings.Contains(s, k) {
			kind = strings.TrimSpace(k)
		}
	}
	found := addrIP.FindAllStringSubmatch(s, -1)
	if len(found) == 0 {
		return nil, kind
	}
	last := found[len(found)-1]
	return net.ParseIP(last[1] + last[2]), kind
}

// dcVerdict says whether a join from addr is turned away, and why, in words for the log.
func dcVerdict(addr net.Addr) (blocked bool, verdict string, ip net.IP, kind string) {
	ip, kind = remoteIP(addr)
	if ip == nil {
		return false, "allowed (address unknown)", nil, kind
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return false, "allowed (local)", ip, kind
	}
	dc.mu.Lock()
	dc.refresh()
	allow, block := dc.allow, dc.block
	dc.mu.Unlock()
	if n := firstMatch(allow, ip); n != nil {
		return false, "allowed (allow list " + n.String() + ")", ip, kind
	}
	if n := firstMatch(block, ip); n != nil {
		return true, "BLOCKED (datacenter " + n.String() + ")", ip, kind
	}
	return false, "allowed", ip, kind
}

var joinsLogMu sync.Mutex

// logJoin appends one join attempt to joins.log, next to Velocity's.
func logJoin(line string) {
	joinsLogMu.Lock()
	defer joinsLogMu.Unlock()
	f, err := os.OpenFile(filepath.Join(dcBlockDir, "joins.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}
