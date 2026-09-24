package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// pipeConn is one end of an in-memory, packet-framed connection - what a nethernet.Conn looks
// like to ProxyConn.
type pipeConn struct {
	in, out chan []byte
	closed  chan struct{}
	once    sync.Once
}

func newPipe() (*pipeConn, *pipeConn) {
	ab, ba := make(chan []byte, 64), make(chan []byte, 64)
	return &pipeConn{in: ba, out: ab, closed: make(chan struct{})},
		&pipeConn{in: ab, out: ba, closed: make(chan struct{})}
}

func (p *pipeConn) ReadPacket() ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, net.ErrClosed
	case <-time.After(5 * time.Second):
		return nil, errors.New("pipe read timed out")
	}
}

func (p *pipeConn) Read([]byte) (int, error) { return 0, errors.New("use ReadPacket") }
func (p *pipeConn) Write(b []byte) (int, error) {
	p.out <- append([]byte(nil), b...)
	return len(b), nil
}
func (p *pipeConn) Close() error                     { p.once.Do(func() { close(p.closed) }); return nil }
func (p *pipeConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (p *pipeConn) SetDeadline(time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error { return nil }

const testXUID = "2535440792904888"

func testLogin(key *ecdsa.PrivateKey) []byte {
	identity := login.IdentityData{XUID: testXUID, Identity: uuid.NewString(), DisplayName: "Tester"}
	client := login.ClientData{
		DeviceOS:          1,
		GameVersion:       "1.26.50",
		LanguageCode:      "en_GB",
		ServerAddress:     "127.0.0.1:19132",
		SkinID:            "skin",
		SkinResourcePatch: base64.StdEncoding.EncodeToString([]byte("{}")),
		ThirdPartyName:    "Tester",
	}
	return login.EncodeOffline(identity, client, key, false)
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func readOne(c *ProxyConn) (packet.Packet, error) {
	pks, err := c.ReadPackets()
	if err != nil {
		return nil, err
	}
	data, err := ParseData(pks[0])
	if err != nil {
		return nil, err
	}
	newPk, ok := c.pool[data.h.PacketID]
	if !ok {
		return nil, fmt.Errorf("unknown packet id %d", data.h.PacketID)
	}
	pk := newPk()
	return pk, decodePacket(pk, data.payload)
}

// startRelay runs the relay's client-facing side of a login in the background.
func startRelay(conn *pipeConn, encrypt bool) (*ProxyConn, chan error) {
	relay := NewProxyConn(conn, true)
	relay.SetAuthEnabled(false) // offline test logins; Xbox verification is not what is under test
	relay.SetClientEncryption(encrypt)
	done := make(chan error, 1)
	go func() { done <- relay.ReadLoop() }()
	return relay, done
}

// join plays the joining client: it logs in with a login signed by loginKey, then answers the
// encryption handshake with handshakeKey. An honest client uses one key for both; a replayer has
// a copy of someone's login but not the key behind it.
func join(conn *pipeConn, loginKey, handshakeKey *ecdsa.PrivateKey) (*ProxyConn, error) {
	client := NewProxyConn(conn, false)
	if err := client.WritePacket(&packet.RequestNetworkSettings{ClientProtocol: 2193}); err != nil {
		return nil, err
	}
	pk, err := readOne(client)
	if err != nil {
		return nil, fmt.Errorf("read network settings: %w", err)
	}
	settings, ok := pk.(*packet.NetworkSettings)
	if !ok {
		return nil, fmt.Errorf("want NetworkSettings, got %T", pk)
	}
	if err := client.handleNetworkSettings(settings); err != nil {
		return nil, err
	}
	if err := client.WritePacket(&packet.Login{ClientProtocol: 2193, ConnectionRequest: testLogin(loginKey)}); err != nil {
		return nil, err
	}
	pk, err = readOne(client)
	if err != nil {
		return nil, fmt.Errorf("read handshake: %w", err)
	}
	handshake, ok := pk.(*packet.ServerToClientHandshake)
	if !ok {
		return nil, fmt.Errorf("want ServerToClientHandshake, got %T", pk)
	}
	client.privateKey = handshakeKey
	return client, client.handleServerToClientHandshake(handshake)
}

func waitLogin(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("relay login never finished")
		return nil
	}
}

func TestHandshakeCompletesForKeyHolder(t *testing.T) {
	relayEnd, clientEnd := newPipe()
	relay, done := startRelay(relayEnd, true)
	key := newKey(t)

	client, err := join(clientEnd, key, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitLogin(t, done); err != nil {
		t.Fatalf("honest client rejected: %v", err)
	}
	if relay.IdentityData().XUID != testXUID || !relay.EncryptionEnabled() {
		t.Fatalf("xuid %q encrypted %v", relay.IdentityData().XUID, relay.EncryptionEnabled())
	}
	// Both ends derived the same key: an encrypted packet gets through intact.
	if err := relay.WritePacket(&packet.PlayStatus{Status: packet.PlayStatusLoginSuccess}); err != nil {
		t.Fatal(err)
	}
	pk, err := readOne(client)
	if err != nil {
		t.Fatalf("client could not read an encrypted packet: %v", err)
	}
	if status, ok := pk.(*packet.PlayStatus); !ok || status.Status != packet.PlayStatusLoginSuccess {
		t.Fatalf("got %#v", pk)
	}
}

func TestReplayedLoginIsRejected(t *testing.T) {
	relayEnd, clientEnd := newPipe()
	_, done := startRelay(relayEnd, true)

	// The replayer sends a genuinely signed login it copied, but holds a different key.
	if _, err := join(clientEnd, newKey(t), newKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := waitLogin(t, done); err == nil {
		t.Fatal("a replayed login without its private key was accepted")
	}
}

func TestServerRolePacketsFromClientAreRejected(t *testing.T) {
	for _, pk := range []packet.Packet{
		&packet.PlayStatus{Status: packet.PlayStatusLoginSuccess},
		&packet.ServerToClientHandshake{JWT: []byte("x.y.z")},
		&packet.NetworkSettings{},
		&packet.ClientToServerHandshake{},
	} {
		relayEnd, clientEnd := newPipe()
		_, done := startRelay(relayEnd, true)
		client := NewProxyConn(clientEnd, false)
		if err := client.WritePacket(pk); err != nil {
			t.Fatal(err)
		}
		if err := waitLogin(t, done); err == nil {
			t.Errorf("%T before login ended the login without authentication", pk)
		}
	}
}

func TestClientEncryptionCanBeTurnedOff(t *testing.T) {
	relayEnd, clientEnd := newPipe()
	relay, done := startRelay(relayEnd, false)
	client := NewProxyConn(clientEnd, false)
	if err := client.WritePacket(&packet.RequestNetworkSettings{ClientProtocol: 2193}); err != nil {
		t.Fatal(err)
	}
	pk, err := readOne(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.handleNetworkSettings(pk.(*packet.NetworkSettings)); err != nil {
		t.Fatal(err)
	}
	if err := client.WritePacket(&packet.Login{ClientProtocol: 2193, ConnectionRequest: testLogin(newKey(t))}); err != nil {
		t.Fatal(err)
	}
	if err := waitLogin(t, done); err != nil {
		t.Fatal(err)
	}
	if relay.EncryptionEnabled() {
		t.Fatal("encryption ran although it was turned off")
	}
}
