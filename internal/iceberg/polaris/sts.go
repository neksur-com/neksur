// sts.go — helper for parsing Polaris 1.4 vended-credentials response.
//
// Polaris 1.4 returns STS credentials in the loadTable response config
// block using Iceberg REST #11118-standardized key names:
//
//	s3.access-key-id        — AWS access key ID
//	s3.secret-access-key    — AWS secret access key
//	s3.session-token        — AWS session token
//	s3.session-expiration   — expiration time (RFC 3339 / ISO 8601)
//
// Per Pitfall 7 (RESEARCH lines 781-785), parsing fails closed if any
// required key is absent — the caller (IssueScopedSTSCredentials) must
// not return partial credentials.
package polaris

import (
	"fmt"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// Polaris 1.4 vended-credentials config-block key names per Iceberg
// REST OpenAPI standardization (apache/iceberg#11118). These constants
// are the only shape this parser accepts — reject any catalog that uses
// a different key convention (Pitfall 7).
const (
	polarisKeyAccessKeyID     = "s3.access-key-id"
	polarisKeySecretAccessKey = "s3.secret-access-key"
	polarisKeySessionToken    = "s3.session-token"
	polarisKeySessionExpiry   = "s3.session-expiration"
)

// expiryLayouts lists the time formats Polaris 1.4 uses for the
// s3.session-expiration field. ISO 8601 with nanoseconds is the
// first-class format; RFC 3339 without sub-seconds is the fallback
// (observed in some Polaris preview versions). Additional formats
// can be appended here as integration testing surfaces them.
var expiryLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05Z", // Polaris-observed variant
}

// parseVendedCreds extracts AWS STS credentials from the loadTable
// response config block returned by a Polaris 1.4 catalog.
//
// Fail-closed behaviour (Pitfall 7 mitigation): if any of the three
// required keys (access-key-id, secret-access-key, session-token) is
// absent or empty the function returns an error so the caller does not
// return partial credentials to Spark. An absent expiration key is
// tolerated but the returned Expiration field will be the zero value —
// callers should treat a zero Expiration as "already expired" (i.e.
// refresh immediately).
//
// The Region field on the returned *STSCredentials is intentionally
// left empty — the caller (IssueScopedSTSCredentials) sets it from
// the request's region parameter after parsing.
func parseVendedCreds(config map[string]string) (*iceberg.STSCredentials, error) {
	accessKeyID := config[polarisKeyAccessKeyID]
	secretAccessKey := config[polarisKeySecretAccessKey]
	sessionToken := config[polarisKeySessionToken]
	expirationStr := config[polarisKeySessionExpiry]

	// Fail-closed: missing any required key is a hard error.
	if accessKeyID == "" {
		return nil, fmt.Errorf("polaris: vended-credentials: missing required key %q in loadTable response config", polarisKeyAccessKeyID)
	}
	if secretAccessKey == "" {
		return nil, fmt.Errorf("polaris: vended-credentials: missing required key %q in loadTable response config", polarisKeySecretAccessKey)
	}
	if sessionToken == "" {
		return nil, fmt.Errorf("polaris: vended-credentials: missing required key %q in loadTable response config", polarisKeySessionToken)
	}

	// Parse the expiration timestamp — tolerate absence (zero value).
	var expiration time.Time
	if expirationStr != "" {
		parsed := false
		for _, layout := range expiryLayouts {
			t, err := time.Parse(layout, expirationStr)
			if err == nil {
				expiration = t.UTC()
				parsed = true
				break
			}
		}
		if !parsed {
			return nil, fmt.Errorf("polaris: vended-credentials: cannot parse %q=%q with any known layout",
				polarisKeySessionExpiry, expirationStr)
		}
	}

	return &iceberg.STSCredentials{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
		Expiration:      expiration,
		// Region is set by the caller from the request parameter.
	}, nil
}
