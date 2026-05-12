package integration

import "testing"

// TestSelfReadVisible is the positive counterpart of the cross-tenant
// RLS isolation test in tests/security/. With the correct tenant set
// via set_config('app.current_tenant', ...), a tenant's own rows MUST
// be visible. If this fails, RLS is over-restricting (which breaks
// the application entirely, not just tenant isolation).
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-tenant-isolation /
// "Self-tenant read sees own nodes".
func TestSelfReadVisible(t *testing.T) {
	tid := "tenant-self-read"

	// Seed as Tenant A.
	{
		tx, commit := tenantTxCommit(t, tid)
		props := `{"uri":"iceberg://t/self/visible","name":"visible","tenant_id":"` + tid + `"}`
		if _, err := tx.Exec(fix.ctx,
			`INSERT INTO neksur."Table" (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
			"281474976710901", props,
		); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		if err := commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}

	// Read as Tenant A — must see the row.
	tx, release := tenantTx(t, tid)
	defer release()

	rows, err := tx.Query(fix.ctx, `
		SELECT * FROM cypher('neksur', $$
			MATCH (t:Table {uri: 'iceberg://t/self/visible'})
			RETURN t
		$$) AS (t ag_catalog.agtype)
	`)
	if err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	defer rows.Close()

	got := 0
	for rows.Next() {
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if got != 1 {
		t.Errorf("self-tenant read returned %d rows; expected 1 — RLS may be over-restricting", got)
	}
}
