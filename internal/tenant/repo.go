package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tenant is the in-memory mirror of one row of public.tenants
// (created by V0041). Fields match the column order of V0041's CREATE
// TABLE statement so the repo can compose SELECT lists row-by-row.
type Tenant struct {
	ID                 uuid.UUID
	WorkOSOrgID        string
	LifecycleState     string // 'active' | 'suspended' | 'wind_down' | 'deleted'
	Pool               string // 'A' | 'B'
	ConnectionDSN      sql.NullString
	OnboardedAt        time.Time
	UpdatedAt          time.Time
	LastAuditLogEvent  sql.NullTime
}

// Repo is the thin CRUD surface over public.tenants. The repo owns a
// *pgxpool.Pool (not a *pgx.Conn) so callers can share one pool with
// the rest of the application. Phase 0.5 Plan 03 uses the same pool
// for the WorkOS middleware lookup path AND for downstream tenant-scoped
// transactions; the pool's BeforeAcquire hook (graph/pool.go) ensures
// DISCARD ALL runs on every acquisition.
//
// All methods are context-first. Errors are wrapped using
// fmt.Errorf with %w so callers can use errors.Is against the
// sentinels declared in errors.go.
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo constructs a Repo bound to the given *pgxpool.Pool.
func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

// ByWorkOSOrgID looks up the tenant.id for a given WorkOS organization
// id. This is the FIRST DB call in a request, BEFORE app.current_tenant
// is set, so Layer 3 RLS would normally hide every row from us.
//
// We invoke public.tenant_by_workos_org(text) — the V0044 SECURITY
// DEFINER STABLE function whose ONLY job is this one safe RLS-bypass
// lookup. The function returns the matching tenant.id when the
// lifecycle_state is 'active' OR 'suspended' (so middleware can still
// route suspended-tenant traffic — read paths continue; the gateway
// layer enforces the 503-on-commit behavior per D-0.5.20).
//
// Returns ErrTenantNotFound when the function returns NULL.
//
// Errors are wrapped: callers should use errors.Is to detect the
// sentinel.
func (r *Repo) ByWorkOSOrgID(ctx context.Context, orgID string) (uuid.UUID, error) {
	const op = "byworkosorgid"
	var id uuid.UUID
	err := r.pool.QueryRow(ctx,
		`SELECT public.tenant_by_workos_org($1)`,
		orgID,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.UUID{}, fmt.Errorf("tenant: %s: %w", op, ErrTenantNotFound)
		}
		return uuid.UUID{}, fmt.Errorf("tenant: %s: %w", op, err)
	}
	// public.tenant_by_workos_org returns SQL NULL when no match is
	// found; pgx maps SQL NULL into the zero-value uuid.UUID. Treat
	// zero UUID as "not found" — a real tenant cannot have the
	// all-zeros UUID.
	if id == (uuid.UUID{}) {
		return uuid.UUID{}, fmt.Errorf("tenant: %s: %w", op, ErrTenantNotFound)
	}
	return id, nil
}

// Create inserts a new tenant row. Uses ON CONFLICT (workos_org_id)
// DO NOTHING so re-running the provisioning script for a partially-
// onboarded tenant is a no-op (D-0.5.19 idempotency). The caller
// supplies the UUID (Plan 04 generates via uuid.New()) so the
// canonical schema name can be computed BEFORE the row is committed.
//
// Requires admin_role (BYPASSRLS) in the executing transaction —
// Layer 3 RLS on public.tenants would otherwise block the INSERT.
// Plan 04 provisioning runs under admin_role; the middleware does NOT
// call Create.
func (r *Repo) Create(ctx context.Context, t Tenant) error {
	const op = "create"
	state := t.LifecycleState
	if state == "" {
		state = "active"
	}
	pool := t.Pool
	if pool == "" {
		pool = "A"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO public.tenants
		    (id, workos_org_id, lifecycle_state, pool, connection_dsn, onboarded_at, updated_at)
		VALUES
		    ($1, $2, $3, $4, $5, COALESCE($6, now()), now())
		ON CONFLICT (workos_org_id) DO NOTHING
	`, t.ID, t.WorkOSOrgID, state, pool, t.ConnectionDSN, sql.NullTime{
		Time:  t.OnboardedAt,
		Valid: !t.OnboardedAt.IsZero(),
	})
	if err != nil {
		return fmt.Errorf("tenant: %s: %w", op, err)
	}
	return nil
}

// SetLifecycleState transitions a tenant through the D-0.5.20 state
// machine. Valid states: 'active', 'suspended', 'wind_down', 'deleted'.
// The CHECK constraint on public.tenants.lifecycle_state will reject
// any other value — we surface that as an error.
//
// Requires admin_role; tenant role's GRANT does not include UPDATE on
// public.tenants.
func (r *Repo) SetLifecycleState(ctx context.Context, id uuid.UUID, state string) error {
	const op = "setlifecyclestate"
	cmd, err := r.pool.Exec(ctx, `
		UPDATE public.tenants
		   SET lifecycle_state = $2,
		       updated_at      = now()
		 WHERE id = $1
	`, id, state)
	if err != nil {
		return fmt.Errorf("tenant: %s: %w", op, err)
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("tenant: %s: %w", op, ErrTenantNotFound)
	}
	return nil
}
