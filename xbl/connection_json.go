package xbl

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// UnmarshalJSON reads a Connection from another host's session document, where NetherNetId is
// not always the number this project writes: some clients store it as a JSON string. Decoding
// the field strictly failed the whole activity-handles response over a single such world, so
// the scraper could not see any friend's world at all (seen 2026-09-29, result #10 of a friends'
// worlds listing). A string that does not parse, or null, reads as 0 - "no NetherNet ID" - which
// callers already handle by falling back to PmsgId.
//
// Marshalling is unchanged: the relay's own session keeps writing NetherNetId as a number.
func (c *Connection) UnmarshalJSON(b []byte) error {
	type plain Connection
	var raw struct {
		plain
		NetherNetId json.RawMessage `json:"NetherNetId"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*c = Connection(raw.plain)
	c.NetherNetId = 0
	id := bytes.TrimSpace(raw.NetherNetId)
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return nil
	}
	s := string(id)
	if id[0] == '"' {
		if err := json.Unmarshal(id, &s); err != nil {
			return nil
		}
	}
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		c.NetherNetId = n
	}
	return nil
}
