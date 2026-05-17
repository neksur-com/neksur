// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.
//
// genkey is a ONE-TIME helper that generates the TEST/STAGING ECDSA P-256 keypair
// used in internal/license/. Run this to regenerate:
//
//	go run ./internal/license/genkey/ -pubout ./internal/license/neksur-license-pubkey.pem -privout ./internal/license/testdata/test-priv.pem
//
// WARNING: This is a TEST/STAGING key. Production key generation ceremony is in Plan 03-15.

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
)

func main() {
	pubOut := flag.String("pubout", "neksur-license-pubkey.pem", "path to write public key PEM")
	privOut := flag.String("privout", "testdata/test-priv.pem", "path to write private key PEM (TEST/STAGING only)")
	flag.Parse()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate key: %v\n", err)
		os.Exit(1)
	}

	// Write public key (PKIX PEM).
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal public key: %v\n", err)
		os.Exit(1)
	}
	pubFile, err := os.Create(*pubOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *pubOut, err)
		os.Exit(1)
	}
	defer pubFile.Close()
	if err := pem.Encode(pubFile, &pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}); err != nil {
		fmt.Fprintf(os.Stderr, "encode public key PEM: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Public key written to %s\n", *pubOut)

	// Write private key (PKCS8 PEM) — test/staging only.
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal private key: %v\n", err)
		os.Exit(1)
	}
	privFile, err := os.Create(*privOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *privOut, err)
		os.Exit(1)
	}
	defer privFile.Close()
	if err := pem.Encode(privFile, &pem.Block{Type: "PRIVATE KEY", Bytes: privDER}); err != nil {
		fmt.Fprintf(os.Stderr, "encode private key PEM: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Private key written to %s (TEST/STAGING only — see Plan 03-15 for production ceremony)\n", *privOut)
}
