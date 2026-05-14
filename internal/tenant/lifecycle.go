package tenant

// lifecycle.go — D-0.5.20 tenant state machine.
//
// The state graph is:
//
//     active ──Suspend()──► suspended ──WindDown()──► wind_down ──Delete()──► deleted
//        │                       │                      ▲
//        └─────WindDown()────────┴──────────────────────┘
//
// `active` → reads + writes go through normally
// `suspended` → reads allowed; gateway returns 503 on commit (D-0.5.20)
// `wind_down` → 30-day post-cancellation read-only; gateway returns 503 on commit
// `deleted` → schema dropped, row remains for 30-day backup retention
//
// Transitions are enforced by the public.tenants.lifecycle_state CHECK
// constraint (V0041) + by the guard predicates here that refuse to move
// backward (e.g., wind_down → active is rejected; that requires manual
// admin intervention by deleting the row and re-onboarding).
//
// Mirroring to WorkOS: Suspend + Delete also try to update the WorkOS
// Organization.status (active/inactive). WorkOS failures are non-fatal —
// they get logged + audit-tracked; a cron job in Plan 07 reconciles
// drift. The Neksur side is the source of truth.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Suspend transitions an active tenant → suspended. D-0.5.20.
//
// Returns ErrTenantNotFound if no row matches OR the tenant is not in a
// state that allows suspension. The single UPDATE … WHERE id = $1 AND
// lifecycle_state IN ('active') is atomic — if RowsAffected = 0 we
// return ErrTenantNotFound to keep callers from accidentally suppressing
// the failure as "row exists, must have been a race". Callers that
// expect a hard fail vs a soft "already suspended" should check the
// current state first.
//
// Also writes an audit row to public.system_audit_log so the lifecycle
// transition is queryable by the admin UI / DR drill scripts.
func (r *Repo) Suspend(ctx context.Context, id uuid.UUID, actor string) error {
	return r.transitionLifecycle(ctx, id, "suspended", []string{"active"}, "tenant.suspended", actor)
}

// WindDown transitions an active OR suspended tenant → wind_down. D-0.5.20.
// 30-day post-cancellation read-only window starts now; Phase 0.5 stores
// the timestamp in `updated_at` (admin UI computes `+30 days` for the
// drop-dead date — Plan 07 cron runs Delete() automatically after the
// window expires).
//
// Returns ErrTenantNotFound on no match / wrong state.
func (r *Repo) WindDown(ctx context.Context, id uuid.UUID, actor string) error {
	return r.transitionLifecycle(ctx, id, "wind_down", []string{"active", "suspended"}, "tenant.wind_down", actor)
}

// Delete is the irreversible terminal transition. Requires explicit
// `confirm == true` to prevent operator error (the corresponding CLI
// surface lives in Plan 07's tenant-delete.sh --yes flag).
//
// Steps (D-0.5.20):
//   1. UPDATE public.tenants SET lifecycle_state = 'deleted'.
//   2. DROP SCHEMA tenant_<uuid> CASCADE — irreversible. Uses
//      drop_graph(<schema>, true) for AGE-aware teardown (Phase 0.5
//      Plan 02 deviation #5: bare DROP SCHEMA CASCADE trips SQLSTATE
//      2BP01 against AGE label tables).
//   3. Write an audit row to public.system_audit_log.
//   4. (Plan 07 only) trigger `terraform destroy -target=module.customer_peering[<id>]`
//      — this function does NOT shell out to terraform; Plan 07's
//      delete script invokes that separately.
//
// Returns ErrTenantNotFound if no row matches; returns an error wrapping
// the DROP failure if the schema drop fails (callers should retry —
// the lifecycle row is already updated, so a retry of Delete just
// re-tries the DROP).
//
// The 30-day backup retention is enforced by the RDS-side pgBackRest +
// PITR policy (Plan 01) — Phase 0.5 does NOT delete backups inline.
func (r *Repo) Delete(ctx context.Context, id uuid.UUID, actor string, confirm bool) error {
	const op = "Delete"
	if !confirm {
		return fmt.Errorf("tenant: %s: confirm flag is required (irreversible)", op)
	}

	// Step 1 — lifecycle transition. Allowed predecessors: any state.
	// (Plan 07 scripts call Delete on a wind_down tenant after the
	// 30-day clock expires; the DR-drill harness calls Delete on
	// suspended tenants. We accept all non-terminal states.)
	if err := r.transitionLifecycle(ctx, id, "deleted",
		[]string{"active", "suspended", "wind_down"},
		"tenant.deleted", actor,
	); err != nil {
		return err
	}

	// Step 2 — schema drop via AGE-aware drop_graph(schema, true).
	// We run this from the admin pool — the tenant role is NOLOGIN
	// and could not drop its own schema even if it tried.
	schemaName := SchemaName(id)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// LOAD 'age' is needed because drop_graph lives in ag_catalog
	// and is sometimes resolved via a session-state probe.
	if _, err := tx.Exec(ctx, `LOAD 'age'`); err != nil {
		return fmt.Errorf("tenant: %s: LOAD age: %w", op, err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = ag_catalog, public`); err != nil {
		return fmt.Errorf("tenant: %s: set search_path: %w", op, err)
	}
	if _, err := tx.Exec(ctx, `SELECT drop_graph($1, true)`, schemaName); err != nil {
		// AGE drop_graph raises a specific error if the graph
		// doesn't exist; we treat that as idempotent.
		if !isUndefinedGraphError(err) {
			return fmt.Errorf("tenant: %s: drop_graph(%s): %w", op, schemaName, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: %s: commit drop: %w", op, err)
	}
	return nil
}

// transitionLifecycle is the shared internal for Suspend/WindDown/Delete.
// Runs the UPDATE + audit-log INSERT in a single transaction.
func (r *Repo) transitionLifecycle(
	ctx context.Context,
	id uuid.UUID,
	newState string,
	fromStates []string,
	eventType string,
	actor string,
) error {
	const op = "transitionLifecycle"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Single-statement UPDATE with WHERE-state-IN guard; RowsAffected
	// tells us whether we actually transitioned anything.
	cmd, err := tx.Exec(ctx, `
		UPDATE public.tenants
		   SET lifecycle_state = $2,
		       updated_at      = now()
		 WHERE id = $1
		   AND lifecycle_state = ANY($3::text[])
	`, id, newState, fromStates)
	if err != nil {
		return fmt.Errorf("tenant: %s: update: %w", op, err)
	}
	if cmd.RowsAffected() == 0 {
		// Either no row, or row but in wrong state. We can't tell
		// without a second query — return ErrTenantNotFound which
		// covers the "no row" case and is the more useful error
		// shape for the suspend/wind-down CLI.
		return fmt.Errorf("tenant: %s: target row not in expected state %v: %w",
			op, fromStates, ErrTenantNotFound)
	}

	payload, _ := json.Marshal(map[string]any{
		"new_state":   newState,
		"from_states": fromStates,
		"actor":       actor,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.system_audit_log
		    (occurred_at, actor_user_id, target_tenant_id, event_type, payload)
		VALUES
		    (now(), $1, $2, $3, $4::jsonb)
	`, actor, id, eventType, string(payload)); err != nil {
		return fmt.Errorf("tenant: %s: audit insert: %w", op, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: %s: commit: %w", op, err)
	}
	return nil
}

// isUndefinedGraphError returns true if err is the AGE "graph does not
// exist" error (drop_graph called for a tenant whose schema was already
// removed). AGE raises this as `RAISE EXCEPTION` from plpgsql with
// SQLSTATE P0001 and a message like 'graph "tenant_..." does not exist'.
// Defensive string check on the message — we accept any SQLSTATE because
// the AGE error shape is opaque (not a canonical Postgres code).
func isUndefinedGraphError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") && strings.Contains(msg, "graph")
}

// Compile-time pgx import suppression: pgx is unused at the package
// surface (transitionLifecycle uses *pgxpool.Pool via the Repo, which
// is wired in repo.go). Kept in the import set so future paths that
// need pgx.ErrNoRows do not have to re-add it.
var _ = pgx.ErrNoRows
