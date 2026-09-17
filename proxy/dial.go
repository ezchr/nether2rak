package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	_ "unsafe"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gameparrot/netherconnect/session"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	mcprotocol "github.com/sandertv/gophertunnel/minecraft/protocol"
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
// so it could never complete the resulting ServerToClientHandshake. Confirmed 2026-09-15 against
// a live BDS 1.26.50.5 NetherNet backend too: it completes a full Minecraft-level encryption
// handshake regardless of transport, so this restriction is not specific to Geyser/RakNet.
//
// Instead this generates a fresh keypair owned by the relay and builds a self-signed
// ("offline") login carrying the already-verified identityData and clientData. The downstream
// server encrypts to OUR public key, which we can complete, while still reading the real
// player's XUID/UUID/display name out of the chain for its own player data and Floodgate
// linkage.
//
// The downstream server must therefore accept unsigned chains - on Geyser that is
// advanced.bedrock.validate-bedrock-login: false; on BDS that is online-mode=false. Because that
// setting makes the downstream listener trust whatever identity it is told, the listener MUST be
// bound to loopback and firewalled from the public internet; the NetherNet/Xbox Live front door
// remains the real authentication boundary.
//
// fixNativeBDSPersistence controls whether ClientData.SelfSignedID is
// overridden with a value deterministically derived from the player's real XUID - see
// proxy/self_signed_id.go for why that is needed at all.
//
// This must be opt-in, not automatic, because it is a workaround for one specific backend
// behaviour: native BDS discards XUID for a self-signed (AuthenticationType 2) login and mints a
// fresh, unrelated identity-linked record every reconnect unless SelfSignedID happens to be
// stable across connections. Geyser, Dragonfly and PNX do not have this problem - they resolve
// player identity from the login chain's real XUID regardless of SelfSignedID, so overriding it
// for them is unnecessary at best. Forcing it on unconditionally would also be actively wrong for
// any backend that DOES treat SelfSignedID as meaningful client-supplied data for some other
// purpose, since this silently replaces whatever the real client sent.
func (c *ProxyConn) ForwardIdentity(protocolID int32, identityData login.IdentityData, clientData login.ClientData, fixNativeBDSPersistence bool, transportKey *ecdsa.PrivateKey) error {
	key := transportKey
	if key == nil {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return fmt.Errorf("generate relay keypair: %w", err)
		}
	}
	c.privateKey = key
	c.identityData = identityData
	if fixNativeBDSPersistence {
		clientData.SelfSignedID = selfSignedIDFromXUID(identityData.XUID)
	}
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
	chain, err := requestMinecraftChain(ctx, xsts, key)
	if err != nil {
		return "", fmt.Errorf("request Minecraft auth chain: %w", err)
	}
	return chain, nil
}

// minecraftAuthURL is the endpoint that issues the Minecraft JWT claim chain.
const minecraftAuthURL = "https://multiplayer.minecraft.net/authentication"

// requestMinecraftChain requests the Minecraft JWT claim chain for an Xbox Live
// token, replacing auth.RequestMinecraftChain.
//
// gophertunnel v1.62.0 changed auth.RequestMinecraftChain to take a
// *xsapi.Client (go-xsapi/v2) rather than the *auth.XBLToken this relay
// already holds. Constructing that client runs a full Xbox device-auth flow
// (an xsts.TokenSource plus an xasd device-token source) and, in its default
// RTAEager mode, also dials an RTA websocket - none of which this endpoint
// needs. It authenticates on the XBL3.0 Authorization header alone:
// gophertunnel's own v1.62.0 implementation wraps the request in
// xsapi.WithoutAuthHeaders(request, "Signature") precisely because the vanilla
// client does not sign this one, so the request sent here is equivalent.
//
// Keeping this local avoids rebuilding the relay's working token flow on
// go-xsapi/v2 as part of a library bump. If the relay ever needs a real
// xsapi.Client for other endpoints, this should collapse back into a direct
// auth.RequestMinecraftChain call.
func requestMinecraftChain(ctx context.Context, token *auth.XBLToken, key *ecdsa.PrivateKey) (string, error) {
	data, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	body := `{"identityPublicKey":"` + base64.StdEncoding.EncodeToString(data) + `"}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, minecraftAuthURL, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST %v: %w", minecraftAuthURL, err)
	}
	token.SetAuthHeader(request)
	request.Header.Set("User-Agent", "MCPE/Android")
	request.Header.Set("Client-Version", mcprotocol.CurrentVersion)
	request.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("POST %v: %w", minecraftAuthURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %v: %v", minecraftAuthURL, resp.Status)
	}
	data, err = io.ReadAll(resp.Body)
	return string(data), err
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
