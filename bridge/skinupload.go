package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Skin uploads to GeyserMC's global API, as Geyser does them. In relay mode a backend only gets the
// relay's re-signed copy of a player's login, which the API refuses, so the relay uploads the
// player's own Xbox-signed login (chain or token, plus client data, which holds the skin) itself.
// The API turns the skin into a Mojang-signed Java texture and keeps it by XUID, where backends with
// Java players (Dragonfly's native Java support, Floodgate) look it up.

const (
	skinUploadURL    = "wss://api.geysermc.org/ws"
	skinUploadAltURL = "wss://ws.geysermc.org/ws"
	skinQueueMax     = 500 // as Geyser: keep the newest uploads while the API is unreachable

	skinEventSkinUploaded = 3
	skinEventLogMessage   = 5
)

type skinUploader struct {
	mu    sync.Mutex
	queue [][]byte
	wake  chan struct{}
	once  sync.Once
	log   *slog.Logger
}

var skins = &skinUploader{wake: make(chan struct{}, 1)}

// uploadSkin queues a player's raw login packet (header included) for upload.
//
// N2R_NO_SKIN_UPLOAD=1 turns uploads off, for test clients whose made-up skin must not replace an
// account's real one in the API.
func uploadSkin(rawLogin []byte, log *slog.Logger) {
	if len(rawLogin) == 0 || os.Getenv("N2R_NO_SKIN_UPLOAD") != "" {
		return
	}
	id, payload, err := splitPacket(rawLogin)
	if err != nil || id != packet.IDLogin {
		return
	}
	var pk packet.Login
	if decodeInto(&pk, payload) != nil {
		return
	}
	msg, ok := skinUploadMessage(pk.ConnectionRequest)
	if !ok {
		return
	}
	skins.once.Do(func() {
		skins.log = log
		go skins.run()
	})
	skins.mu.Lock()
	if len(skins.queue) >= skinQueueMax {
		skins.queue = skins.queue[1:]
	}
	skins.queue = append(skins.queue, msg)
	skins.mu.Unlock()
	select {
	case skins.wake <- struct{}{}:
	default:
	}
}

// skinUploadMessage builds the upload message from a login request: {"chain_data": [...] or
// "token": "...", "client_data": "..."}, as Geyser sends it.
func skinUploadMessage(req []byte) ([]byte, bool) {
	buf := bytes.NewReader(req)
	var n int32
	if binary.Read(buf, binary.LittleEndian, &n) != nil || n <= 0 || int(n) > buf.Len() {
		return nil, false
	}
	head := make([]byte, n)
	_, _ = buf.Read(head)
	var outer struct {
		Certificate string   `json:"Certificate"`
		Chain       []string `json:"chain"`
		Token       string   `json:"Token"`
	}
	if json.Unmarshal(head, &outer) != nil {
		return nil, false
	}
	chain := outer.Chain
	if outer.Certificate != "" {
		var cert struct {
			Chain []string `json:"chain"`
		}
		if json.Unmarshal([]byte(outer.Certificate), &cert) == nil {
			chain = cert.Chain
		}
	}
	if binary.Read(buf, binary.LittleEndian, &n) != nil || n <= 0 || int(n) > buf.Len() {
		return nil, false
	}
	clientData := make([]byte, n)
	_, _ = buf.Read(clientData)
	msg := map[string]any{"client_data": string(clientData)}
	switch {
	case len(chain) > 0:
		msg["chain_data"] = chain
	case outer.Token != "":
		msg["token"] = outer.Token
	default:
		return nil, false
	}
	b, err := json.Marshal(msg)
	return b, err == nil
}

// run keeps a websocket to the API open and sends the queued uploads, reconnecting after failures.
func (u *skinUploader) run() {
	fails := 0
	for {
		url := skinUploadURL
		if fails >= 5 && fails%2 == 1 {
			url = skinUploadAltURL
		}
		if err := u.session(url); err != nil {
			fails++
			u.log.Debug("skin upload: disconnected", "url", url, "err", err)
		}
		time.Sleep(min(time.Duration(fails+1)*5*time.Second, time.Minute))
	}
}

func (u *skinUploader) session(url string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dialCtx, url, nil)
	dialCancel()
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	errc := make(chan error, 2)
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			u.handleMessage(data)
		}
	}()
	go func() { // uploads. No websocket pings: the API closes the connection ("invalid action") on a
		// ping that carries a payload, and every coder/websocket ping does; the reader notices a drop.
		for {
			u.mu.Lock()
			q := u.queue
			u.queue = nil
			u.mu.Unlock()
			for i, msg := range q {
				if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
					u.mu.Lock()
					u.queue = append(q[i:], u.queue...)
					u.mu.Unlock()
					errc <- err
					return
				}
			}
			select {
			case <-u.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return <-errc
}

func (u *skinUploader) handleMessage(data []byte) {
	var m struct {
		EventID int    `json:"event_id"`
		XUID    string `json:"xuid"`
		Success bool   `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch {
	case m.Error != "":
		u.log.Debug("skin upload: error from the global API", "err", m.Error)
	case m.EventID == skinEventSkinUploaded:
		u.log.Debug("skin upload: done", "xuid", m.XUID, "success", m.Success)
	case m.EventID == skinEventLogMessage:
		u.log.Debug("skin upload: message from the global API", "message", m.Message)
	}
}
