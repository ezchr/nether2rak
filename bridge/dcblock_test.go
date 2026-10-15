package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeAddr string

func (a fakeAddr) Network() string { return "nethernet" }
func (a fakeAddr) String() string  { return string(a) }

func TestDcVerdict(t *testing.T) {
	dir := t.TempDir()
	old := dcBlockDir
	dcBlockDir = dir
	defer func() { dcBlockDir = old; dc = dcLists{} }()
	dc = dcLists{}
	os.WriteFile(filepath.Join(dir, "block.txt"), []byte("20.0.0.0/8\n5.6.0.0/16\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "allow.txt"), []byte("20.42.0.0/16\n"), 0o644)
	for addr, want := range map[string]bool{
		"1 (2) (udp4 prflx 5.6.7.8:1 related :0 (resolved: 5.6.7.8:1))":     true,
		"1 (2) (udp4 relay 20.42.1.1:1 related :0 (resolved: 20.42.1.1:1))": false,
		"1 (2) (udp4 relay 20.43.1.1:1 related :0 (resolved: 20.43.1.1:1))": true,
		"1 (2) (udp4 prflx 9.9.9.9:1 related :0 (resolved: 9.9.9.9:1))":     false,
		"1 (2) (udp4 host 10.0.0.5:1 (resolved: 127.0.0.1:57207))":          false,
	} {
		if got, verdict, _, _ := dcVerdict(fakeAddr(addr)); got != want {
			t.Errorf("%s: blocked=%v (%s), want %v", addr, got, verdict, want)
		}
	}
}
