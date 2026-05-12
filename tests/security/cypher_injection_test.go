package security

import (
	"testing"

	"github.com/neksur-com/neksur/internal/graph"
)

// TestStringConcatRejectedByWhitelist verifies the label-whitelist gate
// rejects a malicious label string before it can be spliced into a
// Cypher query. Labels cannot be parameterised in AGE Cypher — they
// are identifiers, not values — so the whitelist is the ONLY defence
// against an attacker who controls a label name in a code path.
//
// This is the integration-tier mirror of the unit test
// internal/graph/client_test.go::TestLabelWhitelistRejectsInjection;
// kept here because the original Python tier had its analog in
// tests/security/test_cypher_injection.py and the parity matters for
// the test-count contract.
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-tenant-isolation / T-0-INJ /
// "Cypher injection via string-concat does not bypass tenant filter".
func TestStringConcatRejectedByWhitelist(t *testing.T) {
	malicious := `Table; DROP TABLE foo --`
	if graph.IsAllowedLabel(malicious) {
		t.Fatalf("IsAllowedLabel(%q) = true; expected false (the whitelist must reject injection payloads)", malicious)
	}
	// Also confirm the malicious string is literally not in the set.
	if _, ok := graph.LabelWhitelist[malicious]; ok {
		t.Fatalf("LabelWhitelist contains the malicious label %q", malicious)
	}
}

// TestParameterPassthroughSafe is the runtime end-to-end check: a
// tenant_id carrying what looks like a SQL escape sequence is treated
// as a literal string by pgx's binder, NOT as SQL to execute.
// Procedure:
//  1. Set the tenant context to a malicious payload via set_config
//     (which uses $1 binding — proven safe in tenant.go).
//  2. Insert a row carrying the same string as its tenant_id property.
//  3. Read it back and assert the row exists.
//  4. Confirm neksur."Table" still exists in ag_catalog — if the
//     payload had been interpreted as SQL, the DROP TABLE would have
//     fired and the row would be unreachable.
func TestParameterPassthroughSafe(t *testing.T) {
	malicious := `X'; DROP TABLE neksur."Table"; --`

	tx, commit := tenantTxCommit(t, malicious)
	props := `{"uri":"iceberg://t/param-probe","tenant_id":"` + escapeJSONString(malicious) + `"}`
	if _, err := tx.Exec(fix.ctx,
		`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
		"281474976710831", props,
	); err != nil {
		t.Fatalf("insert with malicious tenant: %v", err)
	}
	if err := commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read back inside the same (malicious-tid) context.
	readTx, release := tenantTx(t, malicious)
	defer release()

	var got string
	err := readTx.QueryRow(fix.ctx,
		`SELECT properties::text FROM neksur."Table" WHERE id = $1::ag_catalog.graphid`,
		"281474976710831",
	).Scan(&got)
	if err != nil {
		t.Fatalf("read back: %v (a successful DROP TABLE would have made this unreachable)", err)
	}
	if got == "" {
		t.Fatalf("row vanished — malicious tenant_id may have triggered a DROP or binder failed")
	}

	// Confirm the 'Table' vlabel is still in ag_catalog. If the
	// malicious payload had been interpreted as SQL the row above
	// would be lost AND the label would be gone.
	var n int
	if err := fix.superPool.QueryRow(fix.ctx, `
		SELECT count(*) FROM ag_catalog.ag_label
		WHERE name = 'Table'
		  AND graph = (SELECT graphid FROM ag_catalog.ag_graph WHERE name = 'neksur')
	`).Scan(&n); err != nil {
		t.Fatalf("ag_label count: %v", err)
	}
	if n != 1 {
		t.Fatalf("T-0-INJ CRITICAL: the 'Table' vlabel is no longer in ag_catalog (count=%d). Parameter passthrough is NOT safe.", n)
	}
}

// escapeJSONString produces a JSON-safe string literal — minimal
// escaping for embedding inside a JSON document. We need this because
// the malicious payload contains a `"` which would break the JSON.
func escapeJSONString(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
