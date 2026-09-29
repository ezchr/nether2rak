package bridge

// Who is joining, as far as no-relay mode can tell before the backend does.
//
// In the relay modes HandleConn learned the player's XUID from the Minecraft login it relayed, and
// used it for allowed_xuids and for friend_activity.txt. In no-relay mode that login goes straight
// to the backend and never passes through here, so the only identity available is the one in the
// SDP offer: the 'a=identity' attribute a Bedrock client attaches to every NetherNet connection.
// It holds two things (see go-nethernet's identity.go, whose layout this mirrors):
//
//   - a multiplayer token: an RS256 JWT issued by Minecraft's authorization service, carrying
//     the player's XUID (xid), gamertag (xname) and a public key (cpk);
//   - a detached ES384 JWS over the offer's own DTLS fingerprints, signed with that key.
//
// Both are checked here. The token's signature, issuer, audience and expiry prove Microsoft issued
// it to that XUID. The fingerprint signature proves the offer came from whoever holds the token's
// private key - without it a token lifted from another player's offer could be replayed onto an
// attacker's own connection. go-nethernet's default verifier does only the second of these, which
// is why a backend's own Minecraft-login check is what normally authenticates a player.

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gameparrot/netherconnect/session"
	"github.com/go-jose/go-jose/v4"
)

// PlayerToken is what a verified client identity token says about the player.
type PlayerToken struct {
	XUID string
	Name string
	// PublicKey is the token's cpk claim: the key the connection's fingerprints must be signed with.
	PublicKey *ecdsa.PublicKey
}

// SessionTokenVerifier verifies client identity tokens against the authorization service s
// authenticated with, for Config.VerifyPlayerToken.
func SessionTokenVerifier(s *session.Session) func(ctx context.Context, token string) (PlayerToken, error) {
	return func(ctx context.Context, token string) (PlayerToken, error) {
		var claims struct {
			XUID string          `json:"xid"`
			Name string          `json:"xname"`
			CPK  json.RawMessage `json:"cpk"`
		}
		if err := s.VerifyMultiplayerToken(ctx, token, &claims); err != nil {
			return PlayerToken{}, err
		}
		if claims.XUID == "" {
			return PlayerToken{}, errors.New("token has no xid claim")
		}
		key, err := parseCPK(claims.CPK)
		if err != nil {
			return PlayerToken{}, fmt.Errorf("cpk claim: %w", err)
		}
		return PlayerToken{XUID: claims.XUID, Name: claims.Name, PublicKey: key}, nil
	}
}

// parseCPK decodes a cpk claim. Minecraft's authorization service writes it as a base64 DER
// public key; go-nethernet also accepts a JWK there, so both are handled.
func parseCPK(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		der, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if der, err = base64.RawURLEncoding.DecodeString(s); err != nil {
				return nil, fmt.Errorf("decode base64: %w", err)
			}
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		key, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is %T, not ECDSA", pub)
		}
		return key, nil
	}
	var jwk jose.JSONWebKey
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, errors.New("neither a base64 DER key nor a JWK")
	}
	key, ok := jwk.Key.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("JWK key is %T, not ECDSA", jwk.Key)
	}
	return key, nil
}

// errNoIdentity reports an offer without an 'a=identity' attribute - an anonymous peer.
var errNoIdentity = errors.New("offer carries no identity")

// offerIdentity is the part of an 'a=identity' attribute that has to be checked.
type offerIdentity struct {
	token        string
	fingerprints string // detached JWS over the DTLS fingerprints
}

// parseOfferIdentity extracts the identity assertion from an SDP offer.
func parseOfferIdentity(sdp string) (offerIdentity, error) {
	value, ok := sdpAttribute(sdp, "identity", false)
	if !ok {
		return offerIdentity{}, errNoIdentity
	}
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return offerIdentity{}, fmt.Errorf("decode identity attribute: %w", err)
	}
	var data struct {
		Assertion json.RawMessage `json:"assertion"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return offerIdentity{}, fmt.Errorf("decode identity attribute: %w", err)
	}
	// go-nethernet writes the assertion as a JSON string holding the object (identityAssertion's
	// MarshalJSON); accept the plain object as well rather than depend on which client wrote it.
	assertion := []byte(data.Assertion)
	var nested string
	if json.Unmarshal(assertion, &nested) == nil {
		assertion = []byte(nested)
	}
	var a struct {
		Fingerprints string `json:"fingerprints"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal(assertion, &a); err != nil {
		return offerIdentity{}, fmt.Errorf("decode identity assertion: %w", err)
	}
	if strings.Count(a.Token, ".") != 2 || strings.Count(a.Fingerprints, ".") != 2 {
		return offerIdentity{}, errors.New("identity assertion is incomplete")
	}
	return offerIdentity{token: a.Token, fingerprints: a.Fingerprints}, nil
}

// verifyOfferIdentity authenticates the player behind an offer: the token must be genuinely issued
// (verify), and the offer's DTLS fingerprints must be signed by the token's own key.
func verifyOfferIdentity(ctx context.Context, sdp string, verify func(context.Context, string) (PlayerToken, error)) (PlayerToken, error) {
	id, err := parseOfferIdentity(sdp)
	if err != nil {
		return PlayerToken{}, err
	}
	player, err := verify(ctx, id.token)
	if err != nil {
		return PlayerToken{}, fmt.Errorf("verify token: %w", err)
	}
	if player.PublicKey == nil {
		return PlayerToken{}, errors.New("verified token has no public key")
	}
	payload, err := fingerprintPayload(sdp)
	if err != nil {
		return PlayerToken{}, err
	}
	sig, err := jose.ParseDetached(id.fingerprints, payload, []jose.SignatureAlgorithm{jose.ES384})
	if err != nil {
		return PlayerToken{}, fmt.Errorf("parse fingerprint assertion: %w", err)
	}
	if _, err := sig.Verify(player.PublicKey); err != nil {
		return PlayerToken{}, fmt.Errorf("fingerprint assertion not signed by the token's key: %w", err)
	}
	return player, nil
}

// fingerprintPayload rebuilds the exact bytes the fingerprint assertion signs, the way
// go-nethernet's generateFingerprints does, from the offer's fingerprint attribute. As in
// parseDescription, a media-level fingerprint wins over a session-level one.
func fingerprintPayload(sdp string) ([]byte, error) {
	value, ok := sdpAttribute(sdp, "fingerprint", true)
	if !ok {
		return nil, errors.New("offer has no fingerprint attribute")
	}
	algorithm, digest, ok := strings.Cut(value, " ")
	if !ok || strings.Contains(digest, " ") {
		return nil, fmt.Errorf("invalid fingerprint: %s", value)
	}
	return []byte(`{"fingerprint":[{"algorithm":` + strconv.Quote(algorithm) +
		`,"digest":` + strconv.Quote(digest) + `}]}`), nil
}

// sdpAttribute returns the value of the first "a=<key>:" attribute in an SDP. With preferMedia,
// one in the media section is returned ahead of a session-level one.
func sdpAttribute(sdp, key string, preferMedia bool) (string, bool) {
	prefix := "a=" + key + ":"
	var session string
	var sessionOK, inMedia bool
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "m=") {
			inMedia = true
			continue
		}
		value, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		if !preferMedia || inMedia {
			return value, true
		}
		if !sessionOK {
			session, sessionOK = value, true
		}
	}
	return session, sessionOK
}
