//go:build integration

// credvend_session_policy_test.go — Pitfall 1 lint test: asserts that
// BuildSessionPolicy always emits Resource as a JSON ARRAY ([]string),
// never a bare string.
//
// rustfs#1337 / RESEARCH §line 1058 document a class of silent AWS IAM
// failures where passing Resource as a JSON string (instead of an array)
// causes AWS STS to return an opaque InternalError 500 that gives no
// hint at the root cause. This test is a mandatory CI invariant.
//
// Also asserts:
//   - Action == "s3:PutObject" (NOT "s3:*" — least privilege, T-2-sts-overscope).
//   - Condition.StringEquals.aws:RequestedRegion is present (P4 residency).
//
// No Docker required — pure in-process policy construction.
package integration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neksur-com/neksur/internal/credvend"
	"github.com/neksur-com/neksur/internal/iceberg"
)

// TestCredvend_SessionPolicy_ResourceIsArray decodes the BuildSessionPolicy
// output and asserts the Resource field is a JSON array (Pitfall 1 CI invariant).
func TestCredvend_SessionPolicy_ResourceIsArray(t *testing.T) {
	t.Parallel()

	tableRef := iceberg.TableRef{
		Namespace: []string{"prod"},
		Name:      "orders",
	}
	const region = "us-east-1"
	const warehouse = "s3://my-bucket/warehouse"

	policyBytes, err := credvend.BuildSessionPolicy(tableRef, region, warehouse)
	if err != nil {
		t.Fatalf("BuildSessionPolicy: unexpected error: %v", err)
	}

	// Decode into a raw structure to inspect JSON types independent of
	// Go's struct unmarshalling (which would silently coerce a string to
	// []string via custom unmarshaling). We use map[string]any so the
	// JSON decoder preserves the original JSON types.
	var raw struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string `json:"Effect"`
			Action    string `json:"Action"`
			Resource  any    `json:"Resource"` // keep as any to catch string vs array
			Condition any    `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(policyBytes, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v\nbytes: %s", err, string(policyBytes))
	}

	if len(raw.Statement) == 0 {
		t.Fatal("BuildSessionPolicy: Statement is empty")
	}

	stmt := raw.Statement[0]

	// --- Pitfall 1 assertion (T-2-sts-resource-string-bug) ---
	// Resource MUST be a JSON array. json.Unmarshal into any gives
	// []interface{} for a JSON array and string for a JSON string.
	resourceKind := reflect.TypeOf(stmt.Resource).Kind()
	if resourceKind != reflect.Slice {
		t.Errorf("PITFALL 1 VIOLATION: Resource is %s (want slice/array); AWS STS will return "+
			"InternalError 500 — see rustfs#1337 + RESEARCH §line 1058", resourceKind)
	}

	// --- Least-privilege assertion (T-2-sts-overscope) ---
	if stmt.Action != "s3:PutObject" {
		t.Errorf("Action = %q; want %q (s3:* is over-scoped — T-2-sts-overscope)", stmt.Action, "s3:PutObject")
	}

	// --- P4 residency enforcement assertion ---
	condMap, ok := stmt.Condition.(map[string]any)
	if !ok {
		t.Fatalf("Condition is %T; want map[string]any", stmt.Condition)
	}
	stringEquals, ok := condMap["StringEquals"].(map[string]any)
	if !ok {
		t.Fatalf("Condition.StringEquals is %T; want map[string]any", condMap["StringEquals"])
	}
	if _, ok := stringEquals["aws:RequestedRegion"]; !ok {
		t.Error("Condition.StringEquals.aws:RequestedRegion not present — P4 residency condition missing")
	}
	if got, _ := stringEquals["aws:RequestedRegion"].(string); got != region {
		t.Errorf("Condition.StringEquals.aws:RequestedRegion = %q; want %q", got, region)
	}

	// --- Resource content assertion ---
	// Also verify the resource ARN targets the correct table prefix.
	resources, _ := stmt.Resource.([]any)
	if len(resources) == 0 {
		t.Fatal("Resource array is empty")
	}
	resourceARN, _ := resources[0].(string)
	if resourceARN == "" {
		t.Fatal("Resource[0] is not a string or is empty")
	}
	// Should contain the bucket and table path.
	// warehouse = "s3://my-bucket/warehouse", table = prod/orders
	// → arn:aws:s3:::my-bucket/prod/orders/*
	const wantBucket = "my-bucket"
	if !strings.Contains(resourceARN, wantBucket) {
		t.Errorf("Resource ARN %q does not contain bucket %q", resourceARN, wantBucket)
	}
	const wantTable = "orders"
	if !strings.Contains(resourceARN, wantTable) {
		t.Errorf("Resource ARN %q does not contain table name %q", resourceARN, wantTable)
	}
}
