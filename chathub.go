package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The network's shared chat: each server of the network sends its chat lines here (already
// formatted, with rank and name) and long-polls for the lines of the other servers, which it shows
// with a tag for the server they came from. Served on the control routes (loopback only).
//
//	POST /chat/send   {"server": "pvp", "text": "<formatted line>"}
//	GET  /chat/poll?server=pvp&after=<id>   lines from the other servers after id, waiting up to
//	                                        25 s for one; after=-1 returns only the current id.

const (
	chatKeep    = 200
	chatMaxLen  = 1024
	chatPollFor = 25 * time.Second
)

type chatLine struct {
	ID     int64  `json:"id"`
	Server string `json:"server"`
	Text   string `json:"text"`
}

type chatHub struct {
	mu    sync.Mutex
	lines []chatLine
	next  int64
	wake  chan struct{} // closed and replaced on every new line
}

var chat = &chatHub{wake: make(chan struct{})}

func (h *chatHub) add(server, text string) {
	h.mu.Lock()
	h.next++
	h.lines = append(h.lines, chatLine{ID: h.next, Server: server, Text: text})
	if len(h.lines) > chatKeep {
		h.lines = h.lines[len(h.lines)-chatKeep:]
	}
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// since returns the other servers' lines after id, the newest id, and a channel closed on the next line.
func (h *chatHub) since(server string, after int64) ([]chatLine, int64, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []chatLine
	for _, l := range h.lines {
		if l.ID > after && l.Server != server {
			out = append(out, l)
		}
	}
	return out, h.next, h.wake
}

// serveChat handles the /chat routes, reporting whether the path was one.
func serveChat(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/chat/send":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return true
		}
		var in struct {
			Server string `json:"server"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil || in.Server == "" || in.Text == "" {
			http.Error(w, "want {server, text}", http.StatusBadRequest)
			return true
		}
		if len(in.Text) > chatMaxLen {
			in.Text = in.Text[:chatMaxLen]
		}
		chat.add(in.Server, in.Text)
		w.WriteHeader(http.StatusNoContent)
		return true
	case "/chat/poll":
		server := r.URL.Query().Get("server")
		after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if server == "" || err != nil {
			http.Error(w, "want server and after", http.StatusBadRequest)
			return true
		}
		lines, last, wake := chat.since(server, after)
		if after >= 0 && len(lines) == 0 {
			select {
			case <-wake:
				lines, last, _ = chat.since(server, after)
			case <-time.After(chatPollFor):
			case <-r.Context().Done():
				return true
			}
		}
		if after < 0 {
			lines = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Last  int64      `json:"last"`
			Lines []chatLine `json:"lines"`
		}{last, lines})
		return true
	}
	return false
}
