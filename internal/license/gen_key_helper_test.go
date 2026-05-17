// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.
//
// This file is a build helper — it generates the test/staging ECDSA P-256 keypair
// that is embedded into pubkey.go. It is a _test.go file so it is never included in
// production binary builds.
//
// To regenerate the key pair (e.g. after key rotation test ceremony):
//
//	go generate ./internal/license/
//
// WARNING: The private key written to testdata/test-priv.pem is a TEST/STAGING key.
// Production private key generation and ceremony are documented in Plan 03-15 runbook.
// NEVER commit production private keys to this repository.

package license
