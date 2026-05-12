package integration

import (
	"fmt"
	"testing"
)

// TestNodePropertySizeUnder2KB exercises the hybrid storage rule
// (D-001.04): the graph holds *relations and identifiers*, with
// high-volume payloads (full SQL text, manifests, blobs) living in
// auxiliary Postgres tables. Empirically: realistic node shapes' avg
// pg_column_size(properties) MUST stay under 2048 bytes.
//
// Maps to 00-VALIDATION.md row: 02-T3 / REQ-knowledge-graph-foundation /
// "Hybrid storage rule honored — per-node property bag avg <2KB".
func TestNodePropertySizeUnder2KB(t *testing.T) {
	tid := "test-tenant-storage"

	// Three canonical node shapes per ADR §3.4. The base id is the
	// AGE graphid encoding for label-id 4 (Snapshot) entry-id 0; we
	// just need distinct ids per insert.
	base := int64(281474976710700)
	nodes := []struct {
		label string
		id    int64
		props string
	}{
		{"Table", base + 1, `{
			"uri": "iceberg://polaris-prod/sales/orders",
			"catalog_id": "polaris-prod",
			"namespace": "sales",
			"name": "orders",
			"current_snapshot_id": 7842901234567890,
			"partition_spec_id": 3,
			"format_version": 2,
			"owner_team_id": "team-data-eng",
			"created_at": "2025-03-15T10:30:00Z",
			"description": "Order events from Kafka stream",
			"tenant_id": "` + tid + `"
		}`},
		{"Column", base + 2, `{
			"uri": "iceberg://polaris-prod/sales/orders#user_email",
			"parent_table_uri": "iceberg://polaris-prod/sales/orders",
			"name": "user_email",
			"type": "string",
			"ordinal_position": 4,
			"nullable": false,
			"field_id": 4,
			"tenant_id": "` + tid + `"
		}`},
		{"Snapshot", base + 3, `{
			"snapshot_id": 7842901234567890,
			"table_uri": "iceberg://polaris-prod/sales/orders",
			"parent_snapshot_id": 7842876543210987,
			"committed_at": "2025-05-10T14:23:45.123Z",
			"committed_by_engine": "spark",
			"committed_by_engine_id": "spark-prod-cluster-1",
			"operation": "append",
			"added_files": 47,
			"added_records": 1450000,
			"summary": "Hourly batch ingestion from Kafka",
			"tenant_id": "` + tid + `"
		}`},
	}

	tx, release := tenantTx(t, tid)
	defer release()

	for _, n := range nodes {
		stmt := fmt.Sprintf(
			`INSERT INTO neksur.%q (id, properties) VALUES ($1::ag_catalog.graphid, $2::ag_catalog.agtype)`,
			n.label,
		)
		if _, err := tx.Exec(fix.ctx, stmt, fmt.Sprintf("%d", n.id), n.props); err != nil {
			t.Fatalf("insert %s id=%d: %v", n.label, n.id, err)
		}
	}

	var sum, count, maxSeen int
	for _, n := range nodes {
		stmt := fmt.Sprintf(
			`SELECT pg_column_size(properties) FROM neksur.%q WHERE id = $1::ag_catalog.graphid`,
			n.label,
		)
		var size int
		if err := tx.QueryRow(fix.ctx, stmt, fmt.Sprintf("%d", n.id)).Scan(&size); err != nil {
			t.Fatalf("read size %s id=%d: %v", n.label, n.id, err)
		}
		sum += size
		count++
		if size > maxSeen {
			maxSeen = size
		}
	}

	if count == 0 {
		t.Fatalf("no rows measured — fixture seeding broken")
	}
	avg := sum / count
	if avg >= 2048 {
		t.Errorf("avg property-bag size %d bytes >= 2048 — hybrid storage D-001.04 violated", avg)
	}
	if maxSeen >= 2048 {
		t.Errorf("max property-bag size %d bytes >= 2048 — shape is wrong", maxSeen)
	}
}
