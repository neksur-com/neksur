// Package glue is the AWS Glue Iceberg REST IcebergCatalogClient adapter
// (D-3.02) — AWS Glue Iceberg REST catalog with SigV4 signing via
// aws-sdk-go-v2/aws/signer/v4.
//
// Per D-1.03 the entire catalog-side surface is declarative struct fields
// on Config; there is NO functional-options pattern and NO runtime capability
// flags. New (in adapter.go) reads from the struct, validates, and constructs
// an iceberg-go REST catalog client with SigV4 transport.
package glue

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrInvalidConfig is the package sentinel returned when Validate
// finds a missing or empty required field. Callers branch on
// errors.Is(err, glue.ErrInvalidConfig) to distinguish a
// configuration problem from a runtime upstream-Glue error.
var ErrInvalidConfig = errors.New("glue: invalid config")

// Config is the entire surface for a Glue-backed catalog (D-1.03
// declarative). All fields are visible at construction time;
// missing-required-field bugs are caught by Validate before any
// network call.
//
// Field semantics:
//
//   - Region: the AWS region where the Glue catalog resides
//     (e.g., "us-east-1"). Used to build the endpoint URL
//     "https://glue.{region}.amazonaws.com/iceberg" and as the
//     SigV4 signing region. Required.
//
//   - IAMRoleARN: the IAM role ARN that grants Glue:GetTable +
//     Glue:UpdateTable + Glue:CreateTable access. Used only for
//     documentation — credentials are loaded from the AWS default
//     credential chain (config.LoadDefaultConfig) at New() time.
//     Empty string is permitted (the role ARN is optional metadata).
//
//   - CatalogID: the Glue catalog ID. This maps to the Iceberg
//     REST "warehouse" parameter. For AWS Glue's default catalog,
//     this is the AWS account ID. Required.
//
//   - CredentialMode: passed through to the upstream catalog as
//     the X-Iceberg-Access-Delegation header. Defaults to
//     "vended-credentials" (Glue's Iceberg REST supports STS
//     credential vending). Per the plan's Capabilities, Glue
//     SupportsCredVend=true.
type Config struct {
	Region         string
	IAMRoleARN     string
	CatalogID      string
	CredentialMode string

	// BaseTransportWrap is an OPTIONAL hook that wraps the underlying
	// http.Transport before the sigv4Transport composes over it.
	// The composition order at New() time is:
	//
	//   iceberg-go.sessionTransport
	//      → sigv4Transport (SigV4 signing for every request)
	//          → BaseTransportWrap(http.DefaultTransport.Clone())  ← THIS
	//
	// Production callers leave this nil — the adapter then uses a fresh
	// http.DefaultTransport.Clone() as the base. Integration tests use
	// this hook to inject a recording RoundTripper that captures the
	// outbound request headers reaching Glue, or to rewrite the endpoint
	// to point at LocalStack (override the request URL in the transport).
	//
	// The hook is on Config (declarative) rather than a functional-option
	// per D-1.03 — every field of the adapter surface is visible on the
	// struct at construction time. Production-runtime nil is the default;
	// no behavior change for non-test callers.
	BaseTransportWrap func(http.RoundTripper) http.RoundTripper
}

// Validate checks the required fields are non-empty. Defaults for
// CredentialMode are NOT applied here (callers may inspect a returned
// Config to see what they passed); New (in adapter.go) is responsible
// for applying the empty-string defaults before constructing the
// iceberg-go client.
//
// Returns an error wrapped around ErrInvalidConfig so callers can
// branch on errors.Is + still see the specific missing field in
// the message text.
func (c Config) Validate() error {
	switch {
	case c.Region == "":
		return fmt.Errorf("glue: config: Region required: %w", ErrInvalidConfig)
	case c.CatalogID == "":
		return fmt.Errorf("glue: config: CatalogID required: %w", ErrInvalidConfig)
	}
	return nil
}

// withDefaults returns a copy of c with CredentialMode set to its
// default when the caller left it empty.
// Internal helper used by New; not exported because callers should
// see Validate as the only normalization step at the surface.
func (c Config) withDefaults() Config {
	if c.CredentialMode == "" {
		c.CredentialMode = "vended-credentials"
	}
	return c
}
