package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"time"
	_ "unsafe"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gameparrot/netherconnect/session"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func (conn *ProxyConn) Login(clientData login.ClientData, session *session.Session, protocol int32) (err error) {
	if err := conn.WritePacket(&packet.RequestNetworkSettings{ClientProtocol: protocol}); err != nil {
		return fmt.Errorf("send request network settings: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)

	xsts, err := session.LegacyMultiplayerXBL(context.Background())
	if err != nil {
		return err
	}

	var chainData string

	chainData, err = authChain(ctx, xsts, key)
	if err != nil {
		return &net.OpError{Op: "dial", Net: "minecraft", Err: err}
	}
	identityData, err := readChainIdentityData([]byte(chainData))
	if err != nil {
		return &net.OpError{Op: "dial", Net: "minecraft", Err: err}
	}

	conn.clientData = clientData
	conn.privateKey = key

	newTok, err := session.MultiplayerToken(context.Background(), &key.PublicKey)
	if err != nil {
		return err
	}

	oidcV, err := oidcVerifier(context.Background())
	if err != nil {
		return err
	}

	request := login.Encode(chainData, conn.clientData, key, newTok, false)
	identityData, _, _, _ = login.Parse(request, oidcV)
	// If we got the identity data from Minecraft auth, we need to make sure we set it in the Conn too, as
	// we are not aware of the identity data ourselves yet.
	conn.identityData = identityData
	conn.loginPk = &packet.Login{ConnectionRequest: request, ClientProtocol: protocol}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.conn.Close()
			err = ctx.Err()
		case <-done:
			return
		}
	}()
	err = conn.ReadLoop()
	done <- struct{}{}
	return err
}

// ForwardIdentity logs in to a downstream server on behalf of a player who has already been
// authenticated elsewhere (in our case: by real Xbox Live auth at the NetherNet front door).
//
// It does NOT forward the player's original signed login chain. That approach cannot work,
// because the downstream server derives its encryption key from the public key embedded in
// whatever chain it receives - and a relay does not hold the original client's private key,
// so it could never complete the resulting ServerToClientHandshake. (Geyser performs this
// handshake unconditionally; there is no config option to disable it.)
//
// Instead this generates a fresh keypair owned by the relay and builds a self-signed
// ("offline") login carrying the already-verified identityData and clientData. The downstream
// server encrypts to OUR public key, which we can complete, while still reading the real
// player's XUID/UUID/display name out of the chain for its own player data and Floodgate
// linkage.
//
// The downstream server must therefore accept unsigned chains - on Geyser that is
// advanced.bedrock.validate-bedrock-login: false. Because that setting makes the downstream
// listener trust whatever identity it is told, the listener MUST be bound to loopback and
// firewalled from the public internet; the NetherNet/Xbox Live front door remains the real
// authentication boundary.
func (c *ProxyConn) ForwardIdentity(protocolID int32, identityData login.IdentityData, clientData login.ClientData) error {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate relay keypair: %w", err)
	}
	c.privateKey = key
	c.identityData = identityData
	c.clientData = clientData
	c.protocolId = protocolID

	request := login.EncodeOffline(identityData, clientData, key, false)
	c.loginPk = &packet.Login{ConnectionRequest: request, ClientProtocol: protocolID}

	if err := c.WritePacket(&packet.RequestNetworkSettings{ClientProtocol: protocolID}); err != nil {
		return fmt.Errorf("send request network settings: %w", err)
	}

	// ReadLoop drives the rest of the handshake: on NetworkSettings it sends loginPk (see
	// handleNetworkSettings), then completes encryption via ServerToClientHandshake using the
	// key we just generated, and returns once login is done.
	return c.ReadLoop()
}

// authChain requests the Minecraft auth JWT chain using the credentials passed. If successful, an encoded
// chain ready to be put in a login request is returned.
func authChain(ctx context.Context, xsts *auth.XBLToken, key *ecdsa.PrivateKey) (string, error) {
	// Obtain the raw chain data using the
	chain, err := auth.RequestMinecraftChain(ctx, xsts, key)
	if err != nil {
		return "", fmt.Errorf("request Minecraft auth chain: %w", err)
	}
	return chain, nil
}

// readChainIdentityData reads a login.IdentityData from the Mojang chain
// obtained through authentication.
func readChainIdentityData(chainData []byte) (login.IdentityData, error) {
	chain := struct{ Chain []string }{}
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return login.IdentityData{}, fmt.Errorf("read chain: read json: %w", err)
	}
	data := chain.Chain[1]
	claims := struct {
		ExtraData login.IdentityData `json:"extraData"`
	}{}
	tok, err := jwt.ParseSigned(data, []jose.SignatureAlgorithm{jose.ES384})
	if err != nil {
		return login.IdentityData{}, fmt.Errorf("read chain: parse jwt: %w", err)
	}
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return login.IdentityData{}, fmt.Errorf("read chain: read claims: %w", err)
	}
	if claims.ExtraData.Identity == "" {
		return login.IdentityData{}, fmt.Errorf("read chain: no extra data found")
	}
	return claims.ExtraData, nil
}

//go:linkname oidcVerifier github.com/sandertv/gophertunnel/minecraft.oidcVerifier
func oidcVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error)
