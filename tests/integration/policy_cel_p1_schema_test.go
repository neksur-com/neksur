//go:build integration

// Plan 01-05 Task 3 [BLOCKING] — P1 schema policy end-to-end.
//
// Two tests exercise the full P1 round-trip:
//   - TestPolicyCEL_P1_RejectsBannedColumn — policy "no `ssn` column"
//     denies a commit that adds the `ssn` column.
//   - TestPolicyCEL_P1_AllowsValidSchema   — same policy allows a commit
//     that adds a non-PII column.
//
// We construct the Inputs directly (the gateway will build them from
// iceberg.TableMetadata + iceberg.CommitRequest in Plan 01-06; for the
// engine-only test the typed map suffices). The policy itself is
// loaded from the AGE graph via store.LoadPoliciesForTable so the
// SCHEMA_GOVERNS edge is exercised.

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	celpkg "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const p1SchemaTenant = "11111111-1111-4111-8111-111111111111"

// TestPolicyCEL_P1_RejectsBannedColumn — seed a Policy that bans the
// `ssn` column on `orders`; build Inputs reflecting a commit that adds
// `ssn`; expect ActionDeny.
func TestPolicyCEL_P1_RejectsBannedColumn(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, p1SchemaTenant)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `!manifest.has_column(table, "ssn")`
	seedSchemaPolicy(t, gc, p1SchemaTenant, "p1-noSsn", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(p1SchemaTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	policies := loadPolicies(t, ctx, gc, "orders", "test")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy; got %d (%+v)", len(policies), policies)
	}

	env, err := celpkg.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	comp, err := celpkg.NewCompiler(env, 16)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	ev := celpkg.NewEvaluator(comp)

	// Inputs: commit adds an `ssn` column → policy fires deny.
	in := &celpkg.Inputs{
		Table: map[string]any{
			"name":      "orders",
			"namespace": "test",
			"schema": map[string]any{
				"fields": []any{
					map[string]any{"name": "id", "type": "long"},
					map[string]any{"name": "email", "type": "string"},
					map[string]any{"name": "ssn", "type": "string"},
				},
			},
		},
	}

	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionDeny {
		t.Errorf("expected ActionDeny on ssn column add; got %+v", dec)
	}
}

// TestPolicyCEL_P1_AllowsValidSchema — same policy, schema without ssn.
func TestPolicyCEL_P1_AllowsValidSchema(t *testing.T) {
	fx := StartPhase1Fixture(t)
	defer fx.Terminate()

	const tenantID = "11111111-1111-4111-8111-111111111112"
	_ = fx.ProvisionTenant(t, tenantID)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	const policyText = `!manifest.has_column(table, "ssn")`
	seedSchemaPolicy(t, gc, tenantID, "p1-noSsn-2", policyText, "orders", "test")

	tenantUUID := uuid.MustParse(tenantID)
	ctx := tenant.WithID(context.Background(), tenantUUID)
	policies := loadPolicies(t, ctx, gc, "orders", "test")

	env, _ := celpkg.NewEnv()
	comp, _ := celpkg.NewCompiler(env, 16)
	ev := celpkg.NewEvaluator(comp)

	in := &celpkg.Inputs{
		Table: map[string]any{
			"schema": map[string]any{
				"fields": []any{
					map[string]any{"name": "id", "type": "long"},
					map[string]any{"name": "email", "type": "string"},
				},
			},
		},
	}
	dec, err := ev.Evaluate(ctx, policies[0], in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec.Action != celpkg.ActionAllow {
		t.Errorf("expected ActionAllow when ssn absent; got %+v", dec)
	}
}

// ---- shared test helpers -----------------------------------------------

// seedSchemaPolicy creates a Table + Policy + SCHEMA_GOVERNS edge with
// the given policy text. Each call is in its own ExecuteInTenant tx so
// the seed errors surface clearly.
func seedSchemaPolicy(t *testing.T, gc *graph.GraphClient, tenantID, policyID, text, tableName, ns string) {
	t.Helper()
	seedPolicyOfKind(t, gc, tenantID, policyID, text, tableName, ns, "Policy", "schema", "SCHEMA_GOVERNS")
}

// seedAclPolicy creates a Table + Policy + WRITE_GOVERNS edge.
func seedAclPolicy(t *testing.T, gc *graph.GraphClient, tenantID, policyID, text, tableName, ns string) {
	t.Helper()
	seedPolicyOfKind(t, gc, tenantID, policyID, text, tableName, ns, "Policy", "write_acl", "WRITE_GOVERNS")
}

// seedRetentionPolicy creates a Table + RetentionPolicy + RETAINS edge
// per ADR-010 shape (CONTEXT line 86).
func seedRetentionPolicy(t *testing.T, gc *graph.GraphClient, tenantID, policyID, text, tableName, ns string) {
	t.Helper()
	seedPolicyOfKind(t, gc, tenantID, policyID, text, tableName, ns, "RetentionPolicy", "", "RETAINS")
}

func seedPolicyOfKind(t *testing.T, gc *graph.GraphClient, tenantID, policyID, text, tableName, ns,
	policyVlabel, kindProp, edgeLabel string) {
	t.Helper()
	if err := gc.ExecuteInTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Table.
		seeds := []string{
			fmt.Sprintf(`CREATE (:Table {name: '%s', namespace: '%s', tenant_id: '%s', iceberg_id: 'tbl-%s-%s-v1'})`,
				escapeForCypher(tableName), escapeForCypher(ns), tenantID,
				escapeForCypher(tableName), escapeForCypher(ns)),
		}
		// Policy / RetentionPolicy.
		if kindProp == "" {
			seeds = append(seeds, fmt.Sprintf(
				`CREATE (:%s {id: '%s', definition_cel: '%s', tenant_id: '%s'})`,
				policyVlabel, escapeForCypher(policyID),
				escapeForCypher(text), tenantID))
		} else {
			seeds = append(seeds, fmt.Sprintf(
				`CREATE (:%s {id: '%s', kind: '%s', definition_cel: '%s', tenant_id: '%s'})`,
				policyVlabel, escapeForCypher(policyID), kindProp,
				escapeForCypher(text), tenantID))
		}
		// Edge.
		seeds = append(seeds, fmt.Sprintf(
			`MATCH (p:%s {id: '%s'}), (t:Table {name: '%s', namespace: '%s'}) CREATE (p)-[:%s {tenant_id: '%s'}]->(t)`,
			policyVlabel, escapeForCypher(policyID),
			escapeForCypher(tableName), escapeForCypher(ns),
			edgeLabel, tenantID))
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
		t.Fatalf("seedPolicyOfKind: %v", err)
	}
}

// loadPolicies invokes store.AGEStore.LoadPoliciesForTable; t.Fatal on err.
func loadPolicies(t *testing.T, ctx context.Context, gc *graph.GraphClient, tableName, ns string) []celpkg.Policy {
	t.Helper()
	s := store.NewAGEStore(gc)
	policies, err := s.LoadPoliciesForTable(ctx, iceberg.TableRef{
		Namespace: []string{ns},
		Name:      tableName,
	})
	if err != nil {
		t.Fatalf("LoadPoliciesForTable: %v", err)
	}
	return policies
}

// escapeForCypher mirrors the policy/store package's internal helper —
// duplicated here to keep the test package self-contained.
func escapeForCypher(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\\', '\'')
			continue
		}
		if r == 0 {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
