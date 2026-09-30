package xbl

import (
	"encoding/json"
	"testing"
)

func TestConnectionNetherNetIdForms(t *testing.T) {
	for in, want := range map[string]uint64{
		`{"ConnectionType":7,"NetherNetId":1062397408398728133,"PmsgId":"p"}`:   1062397408398728133,
		`{"ConnectionType":7,"NetherNetId":"1062397408398728133","PmsgId":"p"}`: 1062397408398728133,
		`{"ConnectionType":7,"NetherNetId":"","PmsgId":"p"}`:                    0,
		`{"ConnectionType":7,"NetherNetId":"not-a-number","PmsgId":"p"}`:        0,
		`{"ConnectionType":7,"NetherNetId":null,"PmsgId":"p"}`:                  0,
		`{"ConnectionType":7,"PmsgId":"p"}`:                                     0,
	} {
		var c Connection
		if err := json.Unmarshal([]byte(in), &c); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if c.NetherNetId != want || c.ConnectionType != 7 || c.PmsgId != "p" {
			t.Errorf("%s: got %+v, want NetherNetId %d with the other fields kept", in, c, want)
		}
	}
}

// TestActivityListSurvivesOddEntry: one world with a string ID must not lose the others.
func TestActivityListSurvivesOddEntry(t *testing.T) {
	body := `{"results":[
		{"id":"a","customProperties":{"SupportedConnections":[{"ConnectionType":7,"NetherNetId":5}]}},
		{"id":"b","customProperties":{"SupportedConnections":[{"ConnectionType":7,"NetherNetId":"6"}]}}]}`
	var parsed struct {
		Results []activityHandleResponse `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Results) != 2 ||
		parsed.Results[0].CustomProperties.SupportedConnections[0].NetherNetId != 5 ||
		parsed.Results[1].CustomProperties.SupportedConnections[0].NetherNetId != 6 {
		t.Errorf("got %+v", parsed.Results)
	}
}

// TestConnectionStillMarshalsAsNumber: the relay's own session must keep writing a number.
func TestConnectionStillMarshalsAsNumber(t *testing.T) {
	b, _ := json.Marshal(Connection{ConnectionType: 7, NetherNetId: 42})
	if want := `{"ConnectionType":7,"HostIpAddress":"","HostPort":0,"NetherNetId":42,"PmsgId":""}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
