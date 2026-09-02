package nethernet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCpkIsJWK(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := GenerateServerIdentity(key, "self")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(id.Token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("JWT payload: %s", payload)
	if !strings.Contains(string(payload), "\"cpk\":{\"crv\":\"P-384\",\"kty\":\"EC\"") {
		t.Fatalf("cpk is NOT a JWK object")
	}
	t.Logf("OK: cpk is a JWK object")
	pub, err := claimPublicKey(id.Token, true)
	if err != nil {
		t.Fatalf("self-signed verification failed: %v", err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("round-tripped cpk does not match original key")
	}
	t.Logf("OK: self-signed JWT verifies and cpk round-trips")
}
