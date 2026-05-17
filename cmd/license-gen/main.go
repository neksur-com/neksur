// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// license-gen is a stand-alone CLI that generates signed Neksur license manifests.
//
// Usage:
//
//	license-gen [flags]
//
// Flags:
//
//	-customer-id       Customer identifier (e.g. "acme-corp")
//	-tenant-id         Tenant UUID (e.g. "00000000-0000-0000-0000-000000000001")
//	-tier              License tier: "commercial" or "enterprise"
//	-expiry-utc        Expiry timestamp in RFC3339 format (must be in the future)
//	-allowed-features  Comma-separated list of feature names (e.g. "schema_cache_broadcaster,write_conflict")
//	-private-key-path  Path to PKCS#8 PEM-encoded ECDSA P-256 private key
//	-out               Path to write the signed license manifest JSON
//
// Example:
//
//	go run ./cmd/license-gen \
//	  -customer-id acme-corp \
//	  -tenant-id 00000000-0000-0000-0000-000000000001 \
//	  -tier commercial \
//	  -expiry-utc 2027-05-17T00:00:00Z \
//	  -allowed-features schema_cache_broadcaster,write_conflict,continuous_verifier \
//	  -private-key-path /tmp/priv.pem \
//	  -out /tmp/license.json
//
// The output is a JSON manifest signed with ECDSA P-256 ASN.1-DER via ecdsa.SignASN1.
// Signer and verifier share the same canonicalJSON function from internal/license
// to prevent T-3-license-canon-drift.
//
// Production private key ceremony: see Plan 03-15 operator runbook.
// The private key MUST NEVER be committed to the repository.

package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/neksur-com/neksur/internal/license"
)

// validTiers is the set of allowed tier values per D-3.04.
var validTiers = map[string]bool{
	"commercial": true,
	"enterprise": true,
}

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run implements the license-gen command logic with injectable stdout/stderr.
// Returns 0 on success, 1 on any error.
//
// This shape (`Run(args, stdout, stderr) int`) is the preferred pattern for
// testable CLI implementations — it avoids os.Exit in tests.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("license-gen", flag.ContinueOnError)
	fs.SetOutput(stderr)

	customerID := fs.String("customer-id", "", "Customer identifier (required)")
	tenantID := fs.String("tenant-id", "", "Tenant UUID (required)")
	tier := fs.String("tier", "", "License tier: commercial or enterprise (required)")
	expiryUTC := fs.String("expiry-utc", "", "Expiry timestamp in RFC3339 (must be future, required)")
	allowedFeatures := fs.String("allowed-features", "", "Comma-separated feature names (required)")
	privateKeyPath := fs.String("private-key-path", "", "Path to PKCS#8 PEM ECDSA P-256 private key (required)")
	out := fs.String("out", "", "Output path for signed license manifest JSON (required)")

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError: fs.Parse writes usage + error to stderr automatically.
		return 1
	}

	// Validate required flags.
	missing := []string{}
	if *customerID == "" {
		missing = append(missing, "-customer-id")
	}
	if *tenantID == "" {
		missing = append(missing, "-tenant-id")
	}
	if *tier == "" {
		missing = append(missing, "-tier")
	}
	if *expiryUTC == "" {
		missing = append(missing, "-expiry-utc")
	}
	if *allowedFeatures == "" {
		missing = append(missing, "-allowed-features")
	}
	if *privateKeyPath == "" {
		missing = append(missing, "-private-key-path")
	}
	if *out == "" {
		missing = append(missing, "-out")
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "license-gen: missing required flags: %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}

	// Validate tier.
	if !validTiers[*tier] {
		fmt.Fprintf(stderr, "license-gen: tier must be one of: commercial, enterprise (got %q)\n", *tier)
		return 1
	}

	// Parse expiry.
	expiry, err := time.Parse(time.RFC3339, *expiryUTC)
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: invalid -expiry-utc %q: %v (use RFC3339, e.g. 2027-05-17T00:00:00Z)\n", *expiryUTC, err)
		return 1
	}
	if !expiry.After(time.Now().UTC()) {
		fmt.Fprintf(stderr, "license-gen: expiry must be in future (got %s)\n", expiry.Format(time.RFC3339))
		return 1
	}

	// Parse allowed features.
	rawFeatures := strings.Split(*allowedFeatures, ",")
	features := make([]string, 0, len(rawFeatures))
	for _, f := range rawFeatures {
		f = strings.TrimSpace(f)
		if f == "" {
			fmt.Fprintf(stderr, "license-gen: -allowed-features contains empty feature name (use non-empty, comma-separated names)\n")
			return 1
		}
		features = append(features, f)
	}
	if len(features) == 0 {
		fmt.Fprintf(stderr, "license-gen: -allowed-features must contain at least one feature name\n")
		return 1
	}

	// Load private key.
	privKeyBytes, err := os.ReadFile(*privateKeyPath)
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: read private key %q: %v\n", *privateKeyPath, err)
		return 1
	}
	privKey, err := loadPrivateKey(privKeyBytes)
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: load private key: %v\n", err)
		return 1
	}

	// Build the manifest.
	m := license.Manifest{
		LicenseID:       fmt.Sprintf("lic-%s-%s", *customerID, expiry.Format("20060102")),
		CustomerID:      *customerID,
		TenantID:        *tenantID,
		Tier:            *tier,
		ExpiryUTC:       expiry.UTC(),
		AllowedFeatures: features,
	}

	// Canonicalize and sign.
	canonical, err := license.CanonicalJSON(&m)
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: canonicalize manifest: %v\n", err)
		return 1
	}
	digest := sha256.Sum256(canonical)
	sig, err := ecdsa.SignASN1(rand.Reader, privKey, digest[:])
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: sign manifest: %v\n", err)
		return 1
	}
	m.Signature = base64.StdEncoding.EncodeToString(sig)

	// Marshal the signed manifest to JSON.
	manifestJSON, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "license-gen: marshal manifest: %v\n", err)
		return 1
	}

	// Write output file.
	if err := os.WriteFile(*out, manifestJSON, 0644); err != nil {
		fmt.Fprintf(stderr, "license-gen: write output %q: %v\n", *out, err)
		return 1
	}

	fmt.Fprintf(stdout, "license-gen: wrote signed manifest to %s\n", *out)
	return 0
}

// loadPrivateKey parses a PKCS#8 or PKCS#1 PEM-encoded ECDSA private key.
func loadPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key file")
	}

	// Try PKCS#8 first (standard for x509.MarshalPKCS8PrivateKey output).
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is not ECDSA (got %T)", key)
		}
		return ecKey, nil
	}

	// Fallback: try SEC1/PKCS#1 EC key format.
	ecKey, err2 := x509.ParseECPrivateKey(block.Bytes)
	if err2 != nil {
		return nil, fmt.Errorf("parse private key (tried PKCS8: %v; tried EC: %v)", err, err2)
	}
	return ecKey, nil
}
