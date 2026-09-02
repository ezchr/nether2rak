// Standalone third-party verifier for a NetherNet SDP identity assertion.
//
// It deliberately does NOT reuse go-nethernet's generateFingerprints(), because that
// would be circular: signer and verifier sharing a canonicalization bug still agree.
// Instead it reconstructs the signed payload the way a spec-compliant verifier does
// (and the way Pumpkin's fingerprint_payload() does): straight from the SDP text.
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-jose/go-jose/v4"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: verify_identity <logfile> <offer|answer>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	which := os.Args[2]

	// Grab the last SDPDUMP raw <which> line and unquote it.
	re := regexp.MustCompile(`msg="SDPDUMP raw ` + which + `" data=("(?:[^"\\]|\\.)*")`)
	all := re.FindAllStringSubmatch(string(raw), -1)
	if len(all) == 0 {
		fmt.Printf("no SDPDUMP raw %s found in log\n", which)
		os.Exit(1)
	}
	sdp, err := strconv.Unquote(all[len(all)-1][1])
	if err != nil {
		panic(err)
	}

	// --- extract a=identity and a=fingerprint straight from the SDP text
	var identityB64, fpAlgorithm, fpDigest string
	for _, line := range strings.Split(sdp, "\r\n") {
		if v, ok := strings.CutPrefix(line, "a=identity:"); ok {
			identityB64 = v
		}
		if v, ok := strings.CutPrefix(line, "a=fingerprint:"); ok {
			if alg, dig, ok := strings.Cut(v, " "); ok {
				fpAlgorithm, fpDigest = alg, dig
			}
		}
	}
	if identityB64 == "" {
		fmt.Println("FAIL: no a=identity in SDP")
		os.Exit(1)
	}
	fmt.Printf("SDP fingerprint line : %s %s\n", fpAlgorithm, fpDigest)

	identityJSON, err := base64.StdEncoding.DecodeString(identityB64)
	if err != nil {
		fmt.Println("FAIL: identity not valid base64:", err)
		os.Exit(1)
	}
	var identity struct {
		Assertion string `json:"assertion"`
		IDP       struct {
			Domain   string `json:"domain"`
			Protocol string `json:"protocol"`
		} `json:"idp"`
	}
	if err := json.Unmarshal(identityJSON, &identity); err != nil {
		fmt.Println("FAIL: identity JSON:", err)
		os.Exit(1)
	}
	var assertion struct {
		Fingerprints string `json:"fingerprints"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal([]byte(identity.Assertion), &assertion); err != nil {
		fmt.Println("FAIL: assertion JSON:", err)
		os.Exit(1)
	}
	fmt.Printf("idp                  : domain=%q protocol=%q\n", identity.IDP.Domain, identity.IDP.Protocol)

	// --- pull cpk out of the token payload (no verification, just extraction)
	parts := strings.Split(assertion.Token, ".")
	if len(parts) != 3 {
		fmt.Println("FAIL: token is not a compact JWS")
		os.Exit(1)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		fmt.Println("FAIL: token payload base64:", err)
		os.Exit(1)
	}
	var claims struct {
		CPK string `json:"cpk"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
	}
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		fmt.Println("FAIL: token claims JSON:", err)
		os.Exit(1)
	}
	der, err := base64.StdEncoding.DecodeString(claims.CPK)
	if err != nil {
		fmt.Println("FAIL: cpk base64:", err)
		os.Exit(1)
	}
	pubAny, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		fmt.Println("FAIL: cpk is not a PKIX/SPKI public key:", err)
		os.Exit(1)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		fmt.Printf("FAIL: cpk is %T, not *ecdsa.PublicKey\n", pubAny)
		os.Exit(1)
	}
	fmt.Printf("cpk curve            : %s\n", pub.Curve.Params().Name)

	// --- reconstruct the canonical payload FROM THE SDP, as a third party would.
	// Try the exact SDP casing first, then the case variants, to pinpoint any mismatch.
	variants := []struct {
		name   string
		digest string
	}{
		{"as-in-SDP", fpDigest},
		{"uppercased", strings.ToUpper(fpDigest)},
		{"lowercased", strings.ToLower(fpDigest)},
	}

	sig, err := jose.ParseDetached(assertion.Fingerprints, []byte("placeholder"), []jose.SignatureAlgorithm{jose.ES384})
	_ = sig
	if err != nil {
		// ParseDetached validates structure against a payload; re-parse per variant below.
		fmt.Println("note: detached JWS structure parse warning:", err)
	}

	anyOK := false
	for _, v := range variants {
		payload := fmt.Sprintf(`{"fingerprint":[{"algorithm":%s,"digest":%s}]}`,
			strconv.Quote(fpAlgorithm), strconv.Quote(v.digest))
		s, err := jose.ParseDetached(assertion.Fingerprints, []byte(payload), []jose.SignatureAlgorithm{jose.ES384})
		if err != nil {
			fmt.Printf("  %-11s : parse error: %v\n", v.name, err)
			continue
		}
		if _, err := s.Verify(pub); err != nil {
			fmt.Printf("  %-11s : SIGNATURE MISMATCH\n", v.name)
		} else {
			fmt.Printf("  %-11s : *** SIGNATURE VALID ***\n", v.name)
			anyOK = true
		}
	}

	fmt.Println()
	if anyOK {
		fmt.Println("RESULT: assertion verifies against a payload rebuilt from the SDP.")
	} else {
		fmt.Println("RESULT: assertion does NOT verify under any digest casing -> the signed")
		fmt.Println("        payload differs from what the SDP implies (real bug).")
	}
}
