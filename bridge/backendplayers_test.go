package bridge

import "testing"

func TestParsePongPlayers(t *testing.T) {
	// A real Geyser reply's shape: MCPE;motd;protocol;version;players;max;guid;sub-motd;mode;...
	got, err := parsePongPlayers([]byte("MCPE;ZID SMP;2193;1.26.50;7;30;123456789;Geyser;Survival;1;19132;19133;"))
	if err != nil || got != 7 {
		t.Errorf("got %d, %v; want 7", got, err)
	}
	for _, bad := range []string{"", "MCPE;x;1;1", "MCPE;x;1;1;many;30;"} {
		if _, err := parsePongPlayers([]byte(bad)); err == nil {
			t.Errorf("%q parsed without error", bad)
		}
	}
}
