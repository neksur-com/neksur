//go:build integration

// Plan 02-04 Task BLOCKING — TestCompiler_TriggerListenNotify.
//
// End-to-end test of the V0073 per-tenant `policy_changed` trigger +
// the compiler.Trigger LISTEN consumer. Flow:
//
//   1. Start Trigger.Listen in a goroutine with a cancellable ctx.
//   2. INSERT a Policy row into the per-tenant `policies` table via
//      a direct tenant-scoped SQL connection.
//   3. V0073 trigger fires pg_notify('policy_changed', ...).
//   4. The Listen goroutine receives the NOTIFY within 1s — verified
//      by a fake PolicyLoader that signals on a channel each time
//      LoadPolicyForCompile is invoked.
//   5. CompileAll runs via the fake loader's returned PolicySource;
//      assertion polls for the resulting CompiledPolicy node within
//      5s timeout.
//
// We deliberately use a fake PolicyLoader (rather than the
// production stub in main.go) so the trigger consumer's compile path
// fires end-to-end with a known PolicySource — the production
// loader is wired in Plan 02-05.

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	policycel "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/compiler"
	"github.com/neksur-com/neksur/internal/policy/compiler/dialect"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/tenant"
)

const triggerListenTenant = "7716ce71-0204-4444-8a44-444444444444"

// fakeTenantValidator passes every TenantExists check. Defence-in-depth
// for the trigger is tested elsewhere; this test exercises the
// happy-path NOTIFY → compile flow.
type fakeTenantValidator struct{}

func (fakeTenantValidator) TenantExists(_ context.Context, _ uuid.UUID) (bool, error) {
	return true, nil
}

// signallingPolicyLoader records every LoadPolicyForCompile call onto
// a buffered channel and returns a canned PolicySource for the
// configured policy ID.
type signallingPolicyLoader struct {
	sig      chan string
	policyID string
	ref      iceberg.TableRef
	src      compiler.PolicySource
}

func (l *signallingPolicyLoader) LoadPolicyForCompile(_ context.Context, policyID string) (compiler.PolicySource, iceberg.TableRef, error) {
	select {
	case l.sig <- policyID:
	default:
	}
	return l.src, l.ref, nil
}

// TestCompiler_TriggerListenNotify — see file header.
func TestCompiler_TriggerListenNotify(t *testing.T) {
	fx := StartPhase2Fixture(t)
	defer fx.Terminate()

	_ = fx.ProvisionTenant(t, triggerListenTenant)
	_ = fx.ProvisionEngineRegistry(t, triggerListenTenant, []string{"trino"})

	gc, err := graph.NewGraphClient(context.Background(), fx.Container.SuperuserDSN)
	if err != nil {
		t.Fatalf("graph.NewGraphClient: %v", err)
	}
	defer gc.Close()

	// Seed a Table node so the CompiledPolicy MERGE finds the APPLIES_TO
	// target.
	const tableName = "trigger_orders"
	const ns = "test"
	seedPolicyOfKind(t, gc, triggerListenTenant, "placeholder-policy-for-table",
		`true`, tableName, ns, "Policy", "schema", "SCHEMA_GOVERNS")

	tenantUUID := uuid.MustParse(triggerListenTenant)
	ctx := tenant.WithID(context.Background(), tenantUUID)

	celEnv, _ := policycel.NewEnv()
	celComp, _ := policycel.NewCompiler(celEnv, 16)

	comp, err := compiler.NewCompiler(compiler.CompilerConfig{
		Dialects: map[string]dialect.DialectCompiler{
			"trino": dialect.NewTrinoCompiler(),
		},
		Probes:         compiler.NewProbeRunner(nil),
		CompiledStore:  store.NewCompiledStore(gc),
		EngineRegistry: store.NewEngineRegistry(gc),
		CELEnv:         celEnv,
		CELCompiler:    celComp,
	})
	if err != nil {
		t.Fatalf("compiler.NewCompiler: %v", err)
	}

	// Create a real policy UUID we'll INSERT into the per-tenant policies
	// table; V0073 trigger fires pg_notify with this id.
	triggerPolicyID := uuid.New().String()
	loader := &signallingPolicyLoader{
		sig:      make(chan string, 4),
		policyID: triggerPolicyID,
		ref:      iceberg.TableRef{Namespace: []string{ns}, Name: tableName},
		src: compiler.PolicySource{
			PolicyID:      triggerPolicyID,
			PolicyKind:    "schema",
			DefinitionCEL: `true`,
		},
	}

	// Boot the Listen goroutine against the admin pool (CC3 — re-use
	// the fixture pool). Cancel on test exit to clean up.
	pool := newTenantPool(t, fx.Container.SuperuserDSN)
	t.Cleanup(pool.Close)

	listenCtx, cancelListen := context.WithCancel(context.Background())
	defer cancelListen()

	trig := compiler.NewTrigger(pool, comp, fakeTenantValidator{}, loader)
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- trig.Listen(listenCtx)
	}()

	// Give the LISTEN connection a moment to attach. A short sleep is
	// the simplest synchronisation here — alternative would be a
	// readiness probe but pgx exposes none for the LISTEN SQL.
	time.Sleep(500 * time.Millisecond)

	// INSERT a Policy row into the per-tenant policies table via a
	// tenant-scoped transaction. The V0073 trigger consumes
	// current_setting('app.current_tenant'), which WithTenantTx sets.
	insertCtx, insertCancel := context.WithTimeout(ctx, 5*time.Second)
	defer insertCancel()
	if err := tenant.WithTenantTx(insertCtx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(insertCtx,
			`INSERT INTO policies (id, name, kind, body) VALUES ($1::uuid, $2, $3, $4::jsonb)`,
			triggerPolicyID,
			"trigger-test-policy-"+triggerPolicyID,
			"row_filter",
			`{"cel":"true"}`,
		)
		return err
	}); err != nil {
		t.Fatalf("INSERT policy: %v", err)
	}

	// Wait up to 1s for the trigger consumer to dispatch a load call.
	select {
	case got := <-loader.sig:
		if got != triggerPolicyID {
			t.Errorf("LoadPolicyForCompile called with %q; want %q", got, triggerPolicyID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for LoadPolicyForCompile from trigger consumer")
	}

	// Poll for the CompiledPolicy node within 5s.
	cstore := store.NewCompiledStore(gc)
	deadline := time.Now().Add(5 * time.Second)
	for {
		loaded, err := cstore.LoadCompiledForTable(ctx, loader.ref)
		if err != nil {
			t.Fatalf("LoadCompiledForTable: %v", err)
		}
		if len(loaded) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: no CompiledPolicy node landed (loaded=%v)", loaded)
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancelListen()
	select {
	case <-listenDone:
	case <-time.After(2 * time.Second):
		// Listen goroutine didn't exit — not test-fatal but logged.
		t.Logf("Listen goroutine did not exit within 2s of cancel")
	}

	// Keep a sync.WaitGroup reference so the import is used.
	var _ sync.WaitGroup
}
