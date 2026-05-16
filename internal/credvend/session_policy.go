// session_policy.go — BuildSessionPolicy for the L4 credential vending path.
//
// This is the single source of truth for the AWS inline session policy that
// Neksur attaches to every Polaris STS issuance request. The Polaris adapter
// (internal/iceberg/polaris/adapter.go) imports this function via the
// credvend package to keep session policy logic single-sourced.
//
// CRITICAL — Pitfall 1 + rustfs#1337:
//
//	The Resource field MUST be a JSON ARRAY (`[]string`) even with a single
//	element. AWS IAM returns an opaque InternalError 500 when Resource is a
//	bare string — the error does not hint at the root cause. This is one of
//	the most insidious bugs in the AWS session policy surface.
//
//	The struct-typed sessionPolicyDoc below uses `[]string` for Resource,
//	so Go's type system enforces the array invariant at compile time rather
//	than by convention. The integration test TestCredvend_SessionPolicy_ResourceIsArray
//	decodes the output JSON and asserts reflect.TypeOf(Resource).Kind() ==
//	reflect.Slice as an additional CI invariant.
//
// Threat model (T-2-sts-overscope / T-2-sts-resource-string-bug):
//
//	Action: s3:PutObject ONLY — not s3:* (least privilege).
//	Resource: scoped to arn:aws:s3:::{bucket}/{table_prefix}/* (table level).
//	Condition: aws:RequestedRegion = allowed_region (P4 data residency
//	enforcement at the STS level, not just application level).
package credvend

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// SessionPolicy is the exported alias for the JSON-serialisable inline
// session policy document. Exported so integration tests can decode and
// inspect the structure without re-implementing the JSON shape.
type SessionPolicy = sessionPolicyDoc

// sessionPolicyDoc is the JSON shape for an AWS inline session policy.
// Using explicit structs (not map[string]any) guarantees the Resource
// JSON array invariant via Go's type system (Pitfall 1 mitigation).
type sessionPolicyDoc struct {
	Version   string                   `json:"Version"`
	Statement []sessionPolicyStatement `json:"Statement"`
}

// sessionPolicyStatement is one Statement entry in the policy document.
// Resource MUST be []string — this is enforced by the struct field type.
type sessionPolicyStatement struct {
	Effect    string                       `json:"Effect"`
	Action    string                       `json:"Action"`
	Resource  []string                     `json:"Resource"` // MUST be []string — Pitfall 1
	Condition map[string]map[string]string `json:"Condition"`
}

// BuildSessionPolicy constructs the JSON inline session policy for the STS
// AssumeRole call per D-2.09 + RESEARCH §Code Example 6 lines 1051-1066.
//
// Parameters:
//   - table: the Iceberg table ref to scope the policy to
//   - region: the allowed AWS region (P4 data residency enforcement)
//   - warehouse: the Polaris warehouse URI (MUST start with "s3://" —
//     CR-06 hardening rejects bare strings and operator-typo'd values
//     that would otherwise mint credentials against the wrong bucket).
//
// Returns the JSON-encoded policy bytes and an error if the bucket
// cannot be derived from the warehouse URI.
func BuildSessionPolicy(table iceberg.TableRef, region, warehouse string) ([]byte, error) {
	bucket, err := extractBucketFromWarehouse(warehouse)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionPolicyMalformed, err)
	}

	tablePrefix := tableS3PrefixFromRef(table)
	resource := fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, tablePrefix)

	policy := sessionPolicyDoc{
		Version: "2012-10-17",
		Statement: []sessionPolicyStatement{{
			Effect: "Allow",
			// s3:PutObject ONLY — least privilege. NOT s3:* (T-2-sts-overscope).
			Action: "s3:PutObject",
			// MUST be []string — Pitfall 1 + rustfs#1337.
			Resource: []string{resource},
			Condition: map[string]map[string]string{
				"StringEquals": {
					"aws:RequestedRegion": region,
				},
			},
		}},
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("%w: json marshal: %s", ErrSessionPolicyMalformed, err)
	}
	return data, nil
}

// extractBucketFromWarehouse extracts the S3 bucket name from a warehouse
// URI. CR-06: the warehouse MUST start with "s3://" — bare strings and
// non-S3 schemes are rejected so an operator typo cannot mint credentials
// against the wrong account's bucket or build a malformed ARN that AWS
// IAM treats as a wildcard match against the assumed-role's permission
// boundary. The bucket must also be followed by a "/" path component so
// "arn:aws:s3:::{bucket}/{prefix}/*" interpolates with a real prefix.
func extractBucketFromWarehouse(warehouse string) (string, error) {
	after, ok := strings.CutPrefix(warehouse, "s3://")
	if !ok {
		return "", fmt.Errorf("warehouse %q must start with s3:// — refusing to derive bucket", warehouse)
	}
	idx := strings.Index(after, "/")
	if idx <= 0 {
		return "", fmt.Errorf("warehouse %q has no path component after bucket — refusing to derive bucket", warehouse)
	}
	return after[:idx], nil
}

// tableS3PrefixFromRef derives the S3 key prefix for a table by joining
// namespace components and table name with "/".
func tableS3PrefixFromRef(ref iceberg.TableRef) string {
	parts := make([]string, 0, len(ref.Namespace)+1)
	parts = append(parts, ref.Namespace...)
	parts = append(parts, ref.Name)
	return strings.Join(parts, "/")
}
