//go:build integration && polaris

// Plan 02-04 Task BLOCKING — TestGatewayP4P5P7Reject.
//
// The plan-level cross-plan E2E test. Exercises:
//   - Plan 02-03's CEL bindings (location.region, manifest.partition_spec,
//     manifest.classification_satisfied, principal.attribute)
//   - Plan 02-03's AGEStore extensions (RESIDENCY_GOVERNS,
//     CLASSIFICATION_GOVERNS, PARTITION_GOVERNS, ABAC_GOVERNS edge
//     labels in LoadPoliciesForTable)
//   - Plan 02-04's gateway Deps.AttributeResolver wiring (the
//     activation seam) — concretely, the deps below set
//     AttributeResolver = store.NewAttributeResolver(gc, pool); for the
//     ABAC sub-case the resolver is invoked from CEL.
//
// Flow per sub-case:
//   1. Boot Phase2Fixture, provision a per-subtest tenant.
//   2. Seed one violating Policy of the test kind (P4/P5/P7/ABAC).
//   3. POST a commit that violates the policy.
//   4. Expect HTTP 403 + audit_log row decision='REJECTED' +
//      commit_rejected_total{reason=policy_denied} counter increments.

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	workosauth "github.com/neksur-com/neksur/internal/auth/workos"
	"github.com/neksur-com/neksur/internal/catalog"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/ingest"
	"github.com/neksur-com/neksur/internal/observability"
	celpolicy "github.com/neksur-com/neksur/internal/policy/cel"
	policystore "github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

// TestGatewayP4P5P7Reject — see file header.
func TestGatewayP4P5P7Reject(t *testing.T) {
	fx := StartPhase2Fixture(t)
	t.Cleanup(fx.Terminate)

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	t.Cleanup(gc.Close)

	cases := []struct {
		name       string
		tenantStr  string
		policyKind string // graph `kind` property
		edgeLabel  string
		policyID   string
		cel        string
	}{
		{
			name:       "P4_residency_denied",
			tenantStr:  "10000204-0001-4001-8001-000000000004",
			policyKind: "residency",
			edgeLabel:  "RESIDENCY_GOVERNS",
			policyID:   "p4-region-must-be-uswest",
			// commit.location_region unset in default body → "" != "us-west-1" → deny.
			cel: `location.region(commit) == "us-west-1"`,
		},
		{
			name:       "P5_classification_denied",
			tenantStr:  "10000204-0001-4001-8001-000000000005",
			policyKind: "classification",
			edgeLabel:  "CLASSIFICATION_GOVERNS",
			policyID:   "p5-ssn-must-be-encrypted",
			cel:        `manifest.classification_satisfied(table, "^.*_ssn$", "ENCRYPTED")`,
		},
		{
			name:       "P7_partition_denied",
			tenantStr:  "10000204-0001-4001-8001-000000000007",
			policyKind: "partition",
			edgeLabel:  "PARTITION_GOVERNS",
			policyID:   "p7-ts-must-be-hours",
			cel:        `manifest.partition_spec(table)["ts"] == "hours"`,
		},
		{
			name:       "ABAC_clearance_denied",
			tenantStr:  "10000204-0001-4001-8001-0a0a0a0abacc",
			policyKind: "abac",
			edgeLabel:  "ABAC_GOVERNS",
			policyID:   "abac-must-be-top-secret",
			cel:        `principal.attribute(principal, "clearance") == "top-secret"`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := startGatewayHarnessP2(t, fx, gc, tc.tenantStr)
			seedPolicyOfKind(t, gc, h.tenantStr, tc.policyID, tc.cel,
				"orders", "test", "Policy", tc.policyKind, tc.edgeLabel)

			before := readPolicyDeniedCount(t)

			status, body := h.postCommit(t, "prod-polaris", "test", "orders", validCommitBody())
			if status != 403 {
				t.Fatalf("status = %d; want 403. body=%s", status, body)
			}
			if !strings.Contains(body, "policy denied") {
				t.Errorf("body missing 'policy denied': %s", body)
			}

			decision, _, _ := queryAuditLogDecision(t, context.Background(), h)
			if decision != "REJECTED" {
				t.Errorf("audit_log decision = %q; want REJECTED", decision)
			}

			weCount := countMatching(t, context.Background(), h.gc, h.tenantStr,
				`MATCH (we:WriteEvent) WHERE we.decision = 'REJECTED' RETURN count(we)`)
			if weCount < 1 {
				t.Errorf("WriteEvent REJECTED count = %d; want >= 1", weCount)
			}

			after := readPolicyDeniedCount(t)
			if after <= before {
				t.Errorf("commit_rejected_total{reason=policy_denied} not incremented: before=%v after=%v",
					before, after)
			}
		})
	}
}

// startGatewayHarnessP2 builds a gatewayHarness against an already-
// booted Phase2Fixture (so we don't pay 4× container pulls per test).
// Provisions the tenant against the embedded Phase1Fixture and wires
// Deps.AttributeResolver (Plan 02-04 seam).
func startGatewayHarnessP2(t *testing.T, fx *Phase2Fixture, gc *graph.GraphClient, tenantStr string) *gatewayHarness {
	t.Helper()
	tenantUUID := uuid.MustParse(tenantStr)
	_ = fx.ProvisionTenant(t, tenantStr)

	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	t.Cleanup(pool.Close)

	celEnv, err := celpolicy.NewEnv()
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	celCompiler, err := celpolicy.NewCompiler(celEnv, 16)
	if err != nil {
		t.Fatalf("cel.NewCompiler: %v", err)
	}

	fake := newFakeAdapter()
	attrResolver := policystore.NewAttributeResolver(gc, pool)

	deps := iceberggw.Deps{
		Pool:              pool,
		Graph:             gc,
		CredStore:         catalog.NewRepo(pool),
		PolicyStore:       policystore.NewAGEStore(gc),
		Evaluator:         celpolicy.NewEvaluator(celCompiler),
		IngestSvc:         ingest.NewService(gc),
		AttributeResolver: attrResolver,
		AdapterFactory: func(_ context.Context, _ *catalog.Credentials) (iceberg.IcebergCatalogClient, error) {
			return fake, nil
		},
	}

	mux := http.NewServeMux()
	wrap := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.WithID(r.Context(), tenantUUID)
			handler(w, r.WithContext(ctx))
		})
	}
	mux.Handle("POST /v1/iceberg/{prefix}/namespaces/{namespace}/tables/{table}",
		wrap(iceberggw.CommitHandler(deps)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Compile-time anchor matching gateway_helpers_test.go.
	var _ = workosauth.TenantMiddleware

	return &gatewayHarness{
		t: t, fx: fx.Phase1Fixture, tenantUUID: tenantUUID, tenantStr: tenantStr,
		gc: gc, deps: deps, fake: fake, srv: srv,
	}
}

// readPolicyDeniedCount returns the current sum of
// commit_rejected_total{reason=policy_denied} across all label
// combinations. Returns 0 if no samples have been emitted yet.
func readPolicyDeniedCount(t *testing.T) float64 {
	t.Helper()
	var total float64
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Logf("readPolicyDeniedCount: gather: %v", err)
		return 0
	}
	for _, mf := range mfs {
		if mf.GetName() != "commit_rejected_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelMatch(m.GetLabel(), "reason", observability.ReasonPolicyDenied) {
				continue
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

func labelMatch(labels []*dto.LabelPair, name, want string) bool {
	for _, l := range labels {
		if l.GetName() == name {
			return l.GetValue() == want
		}
	}
	return false
}
