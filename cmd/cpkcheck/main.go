package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/df-mc/go-nethernet"
)

func main() {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		panic(err)
	}
	id, err := nethernet.GenerateServerIdentity(key, "self")
	if err != nil {
		panic(err)
	}
	parts := strings.Split(id.Token, ".")
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	fmt.Println("JWT header :", string(header))
	fmt.Println("JWT payload:", string(payload))
	if strings.Contains(string(payload), "\"cpk\":{") {
		fmt.Println("RESULT: cpk is a JWK object  <-- matches genuine vanilla host")
	} else {
		fmt.Println("RESULT: cpk is NOT a JWK object")
	}
}
