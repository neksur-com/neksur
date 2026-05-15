//go:build integration

// Integration tests for the AGE-backed Policy loader — Plan 01-05 Task 2.
//
// Three tests:
//
//   - TestLoadPoliciesForTable: seed two Policy nodes (SCHEMA_GOVERNS +
//     WRITE_GOVERNS) + one RetentionPolicy node (RETAINS) connected to
//     a Table; assert LoadPoliciesForTable returns 3 entries with kinds
//     ["schema", "write_acl", "retention"] (order-insensitive).
//
//   - TestLoadPoliciesRequiresTenantCtx: call without tenant context;
//     assert errors.Is(err, ErrTenantMissing).
//
//   - TestLoadPoliciesReturnsEmptyWhenNoMatch: unknown table; assert
//     len(policies) == 0 (NOT an error — empty is valid).

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
	"github.com/neksur-com/neksur/tests/integration"
)

const policyStoreTenant = "66666666-6666-4666-6666-666666666666"

// TestLoadPoliciesForTable seeds 3 policies (P1 schema + P2 write-ACL +
// P3 retention) attached to the same Table; asserts LoadPoliciesForTable
// returns all 3 with the correct Kind discrimination.
//
// The seed uses raw Cypher CREATE (not MERGE) — this is a fresh tenant
// per test, so CREATE is correct + faster. Avoids the AGE 1.6
// `ON CREATE SET / ON MATCH SET` parser regression entirely.
func TestLoadPoliciesForTable(t *testing.T) {
	fx := integration.StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, policyStoreTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Seed: 1 Table + 2 Policy + 1 RetentionPolicy + 3 edges. All
	// nodes/edges carry tenant_id inline (V0030 CHECK constraint).
	if err := gc.ExecuteInTenant(context.Background(), policyStoreTenant, func(ctx context.Context, tx pgx.Tx) error {
		seeds := []string{
			`CREATE (:Table {name: 'orders', namespace: 'test', tenant_id: '` + policyStoreTenant + `', iceberg_id: 'tbl-orders-v1'})`,
			`CREATE (:Policy {id: 'p1-schema', kind: 'schema', definition_cel: 'true', tenant_id: '` + policyStoreTenant + `'})`,
			`CREATE (:Policy {id: 'p2-acl', kind: 'write_acl', definition_cel: 'true', tenant_id: '` + policyStoreTenant + `'})`,
			`CREATE (:RetentionPolicy {id: 'p3-retain', definition_cel: 'true', tenant_id: '` + policyStoreTenant + `'})`,
			`MATCH (p:Policy {id: 'p1-schema'}), (t:Table {name: 'orders', namespace: 'test'}) CREATE (p)-[:SCHEMA_GOVERNS {tenant_id: '` + policyStoreTenant + `'}]->(t)`,
			`MATCH (p:Policy {id: 'p2-acl'}), (t:Table {name: 'orders', namespace: 'test'}) CREATE (p)-[:WRITE_GOVERNS {tenant_id: '` + policyStoreTenant + `'}]->(t)`,
			`MATCH (rp:RetentionPolicy {id: 'p3-retain'}), (t:Table {name: 'orders', namespace: 'test'}) CREATE (rp)-[:RETAINS {tenant_id: '` + policyStoreTenant + `'}]->(t)`,
		}
		for _, c := range seeds {
			q := fmt.Sprintf(
				"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (result ag_catalog.agtype)",
				c,
			)
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("seed %q: %w", c, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now LoadPoliciesForTable.
	tenantUUID := uuid.MustParse(policyStoreTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	s := store.NewAGEStore(gc)
	policies, err := s.LoadPoliciesForTable(ctx, iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "orders",
	})
	if err != nil {
		t.Fatalf("LoadPoliciesForTable: %v", err)
	}
	if len(policies) != 3 {
		t.Fatalf("got %d policies; want 3 — got=%+v", len(policies), policies)
	}

	// Order-insensitive kind check.
	kindsSeen := map[string]int{}
	for _, p := range policies {
		kindsSeen[p.Kind]++
	}
	for _, want := range []string{"schema", "write_acl", "retention"} {
		if kindsSeen[want] != 1 {
			t.Errorf("kind=%q count=%d; want 1 (kindsSeen=%+v, policies=%+v)",
				want, kindsSeen[want], kindsSeen, policies)
		}
	}
}

// TestLoadPoliciesRequiresTenantCtx — call without tenant context;
// must return ErrTenantMissing (CC1: tenant ctx required).
func TestLoadPoliciesRequiresTenantCtx(t *testing.T) {
	fx := integration.StartPhase1Fixture(t)
	defer fx.Terminate()

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	s := store.NewAGEStore(gc)
	policies, err := s.LoadPoliciesForTable(context.Background(),
		iceberg.TableRef{Namespace: []string{"test"}, Name: "orders"})
	if err == nil {
		t.Fatalf("expected error; got nil (policies=%+v)", policies)
	}
	if !errors.Is(err, store.ErrTenantMissing) {
		t.Errorf("errors.Is ErrTenantMissing = false; want true (err=%v)", err)
	}
	if policies != nil {
		t.Errorf("expected nil policies on err; got %+v", policies)
	}
}

// TestLoadPoliciesReturnsEmptyWhenNoMatch — unknown table; expect
// (nil, nil) or ([], nil) — empty is valid.
func TestLoadPoliciesReturnsEmptyWhenNoMatch(t *testing.T) {
	fx := integration.StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "77777777-7777-4777-7777-777777777777"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	s := store.NewAGEStore(gc)
	policies, err := s.LoadPoliciesForTable(ctx, iceberg.TableRef{
		Namespace: []string{"test"},
		Name:      "nonexistent",
	})
	if err != nil {
		t.Fatalf("LoadPoliciesForTable: %v", err)
	}
	if len(policies) != 0 {
		t.Errorf("got %d policies; want 0 — got=%+v", len(policies), policies)
	}
}
