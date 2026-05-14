// Package admin implements the minimum admin UI surface for Phase 0.5
// SaaS pilot per CONTEXT Claude's Discretion line 93:
//
//   - GET /admin/tenants            — list view of public.tenants rows
//   - GET /admin/tenants/<id>/audit — paginated audit-log viewer
//                                     (keyset pagination on (occurred_at DESC, id DESC))
//   - GET /admin/incidents          — PagerDuty incident-list embed
//
// All handlers are gated by `AdminOrgGate`, which requires the WorkOS
// session to carry an `organization_id` equal to the operator-provisioned
// `internal_admin` org id (sourced from env var WORKOS_INTERNAL_ADMIN_ORG_ID).
// Non-admin sessions get a 403.
//
// Threat model:
//   - T-0.5-admin-org-bypass (mitigate): AdminOrgGate is the org-membership
//     check. The tenant middleware runs first (validates session); admin
//     gate runs after and adds the org-id equality check.
//   - T-0.5-audit-tamper (accept — Plan 02/04 ship the GRANT discipline):
//     this UI is read-only. Handlers query under `admin_role` (BYPASSRLS)
//     to read cross-tenant audit_log rows; they NEVER issue
//     INSERT/UPDATE/DELETE against public.system_audit_log.
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminGate is the interface AdminOrgGate uses to read the WorkOS
// session organization-id from the incoming request. Defined as an
// interface so the test suite can inject a stub without depending on
// the full WorkOS client.
type AdminGate interface {
	LoadSessionOrgID(r *http.Request) (string, error)
}

// AdminOrgGate returns middleware that requires the incoming session's
// organization_id to equal `internalAdminOrgID`. Non-admin requests get
// HTTP 403.
//
// Usage in cmd/neksur-server/main.go:
//
//	mux.Handle("/admin/", admin.AdminOrgGate(workosGate, os.Getenv("WORKOS_INTERNAL_ADMIN_ORG_ID"))(adminMux))
//
// where workosGate is a thin adapter around the *workos.Client that
// implements AdminGate.LoadSessionOrgID.
func AdminOrgGate(gate AdminGate, internalAdminOrgID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if internalAdminOrgID == "" {
				http.Error(w, "admin gate misconfigured (WORKOS_INTERNAL_ADMIN_ORG_ID unset)", http.StatusInternalServerError)
				return
			}
			orgID, err := gate.LoadSessionOrgID(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if orgID != internalAdminOrgID {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TenantRow is the minimum subset of public.tenants the admin UI lists.
// Mirrors the storage layer's tenant.Tenant struct (kept duplicated to
// avoid an import cycle between admin and tenant packages).
type TenantRow struct {
	ID                uuid.UUID    `json:"id"`
	WorkOSOrgID       string       `json:"workos_org_id"`
	LifecycleState    string       `json:"lifecycle_state"`
	Pool              string       `json:"pool"`
	OnboardedAt       time.Time    `json:"onboarded_at"`
	LastAuditLogEvent sql.NullTime `json:"last_audit_log_event,omitempty"`
}

// ListTenants returns an HTTP handler that lists the most recently
// onboarded 100 tenants from public.tenants. The handler queries under
// `admin_role` (BYPASSRLS) via a `SET LOCAL ROLE` inside its tx; this
// is the only path that reads cross-tenant rows.
//
// Output: JSON if Accept includes "application/json"; HTML table
// otherwise.
func ListTenants(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rows, err := readTenantsAdmin(ctx, pool, 100)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if wantsJSON(r) {
			writeJSON(w, rows)
			return
		}
		renderTenantList(w, rows)
	}
}

// ViewTenantAuditLog returns an HTTP handler that renders a paginated
// audit-log view for one tenant. Query string params:
//
//	tenant=<uuid>             — required
//	before_at=<RFC3339>       — keyset cursor (optional; default: now)
//	before_id=<bigint>        — keyset cursor (optional; default: MaxInt64)
//	limit=<int>               — default 50; max 200
//
// Keyset pagination on (occurred_at DESC, id DESC) per ADR-001 §3.4 +
// V0043's `idx_system_audit_log_keyset` index.
func ViewTenantAuditLog(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tenantStr := r.URL.Query().Get("tenant")
		if tenantStr == "" {
			http.Error(w, "missing ?tenant=<uuid>", http.StatusBadRequest)
			return
		}
		tenantID, err := uuid.Parse(tenantStr)
		if err != nil {
			http.Error(w, "invalid tenant uuid", http.StatusBadRequest)
			return
		}
		beforeAt := time.Now()
		if v := r.URL.Query().Get("before_at"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				beforeAt = t
			}
		}
		beforeID := int64(1 << 62)
		if v := r.URL.Query().Get("before_id"); v != "" {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				beforeID = id
			}
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		rows, err := readAuditLogAdmin(ctx, pool, tenantID, beforeAt, beforeID, limit)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if wantsJSON(r) {
			writeJSON(w, rows)
			return
		}
		renderAuditList(w, tenantID, rows)
	}
}

// AuditRow is one row of public.system_audit_log.
type AuditRow struct {
	ID               int64           `json:"id"`
	OccurredAt       time.Time       `json:"occurred_at"`
	ActorUserID      sql.NullString  `json:"actor_user_id,omitempty"`
	ActorWorkOSOrgID sql.NullString  `json:"actor_workos_org_id,omitempty"`
	TargetTenantID   *uuid.UUID      `json:"target_tenant_id,omitempty"`
	EventType        string          `json:"event_type"`
	Payload          json.RawMessage `json:"payload"`
}

// EmbedPagerDutyIncidents renders a wrapper page that embeds the
// PagerDuty incident-list iframe for the given service id. PagerDuty
// owns the rendering; this handler is a thin URL builder.
//
// serviceID is the PagerDuty service id whose incidents we display
// (e.g. P01ABC2). It's a config value, not user input.
func EmbedPagerDutyIncidents(serviceID string) http.HandlerFunc {
	const tmplSrc = `<!doctype html>
<html><head><title>Neksur SaaS — Incidents</title></head>
<body>
<h1>Active PagerDuty Incidents (service {{.ServiceID}})</h1>
<p>Open the PagerDuty console <a target="_blank" rel="noopener" href="https://neksur.pagerduty.com/service-directory/{{.ServiceID}}">here</a> to view + ack.</p>
<iframe
  src="https://neksur.pagerduty.com/service-directory/{{.ServiceID}}"
  width="100%" height="800" frameborder="0"
  referrerpolicy="strict-origin-when-cross-origin"
  sandbox="allow-scripts allow-same-origin"
></iframe>
</body></html>
`
	tmpl := template.Must(template.New("incidents").Parse(tmplSrc))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, struct{ ServiceID string }{ServiceID: serviceID})
	}
}

// ----- internal: DB access ---------------------------------------------------

// readTenantsAdmin runs SELECT under admin_role to bypass RLS on
// public.tenants. SET LOCAL ROLE reverts at txn end.
func readTenantsAdmin(ctx context.Context, pool *pgxpool.Pool, limit int) ([]TenantRow, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE admin_role"); err != nil {
		return nil, fmt.Errorf("admin: set role: %w", err)
	}

	pgxRows, err := tx.Query(ctx, `
		SELECT id, workos_org_id, lifecycle_state, pool, onboarded_at, last_audit_log_event
		  FROM public.tenants
		 ORDER BY onboarded_at DESC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("admin: query tenants: %w", err)
	}
	defer pgxRows.Close()

	var out []TenantRow
	for pgxRows.Next() {
		var t TenantRow
		if err := pgxRows.Scan(&t.ID, &t.WorkOSOrgID, &t.LifecycleState, &t.Pool, &t.OnboardedAt, &t.LastAuditLogEvent); err != nil {
			return nil, fmt.Errorf("admin: scan tenants: %w", err)
		}
		out = append(out, t)
	}
	if err := pgxRows.Err(); err != nil {
		return nil, fmt.Errorf("admin: rows err: %w", err)
	}
	return out, nil
}

// readAuditLogAdmin runs SELECT against public.system_audit_log under
// admin_role. Keyset pagination on (occurred_at DESC, id DESC) — the
// V0043 index is `idx_system_audit_log_keyset`.
func readAuditLogAdmin(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, beforeAt time.Time, beforeID int64, limit int) ([]AuditRow, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE admin_role"); err != nil {
		return nil, fmt.Errorf("admin: set role: %w", err)
	}

	pgxRows, err := tx.Query(ctx, `
		SELECT id, occurred_at, actor_user_id, actor_workos_org_id, target_tenant_id, event_type, payload
		  FROM public.system_audit_log
		 WHERE target_tenant_id = $1
		   AND (occurred_at, id) < ($2, $3)
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT $4
	`, tenantID, beforeAt, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("admin: query audit_log: %w", err)
	}
	defer pgxRows.Close()

	var out []AuditRow
	for pgxRows.Next() {
		var a AuditRow
		var rawTenant sql.NullString
		if err := pgxRows.Scan(&a.ID, &a.OccurredAt, &a.ActorUserID, &a.ActorWorkOSOrgID, &rawTenant, &a.EventType, &a.Payload); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			return nil, fmt.Errorf("admin: scan audit_log: %w", err)
		}
		if rawTenant.Valid {
			if tid, perr := uuid.Parse(rawTenant.String); perr == nil {
				a.TargetTenantID = &tid
			}
		}
		out = append(out, a)
	}
	if err := pgxRows.Err(); err != nil {
		return nil, fmt.Errorf("admin: audit_log rows err: %w", err)
	}
	return out, nil
}

// ----- internal: rendering ---------------------------------------------------

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var tenantListTmpl = template.Must(template.New("tenants").Parse(`<!doctype html>
<html><head><title>Neksur SaaS — Tenants</title></head>
<body>
<h1>Tenants ({{len .}} most recent)</h1>
<table border="1" cellpadding="4">
<tr><th>id</th><th>workos_org</th><th>state</th><th>pool</th><th>onboarded</th><th>last audit event</th></tr>
{{range .}}
<tr>
  <td><a href="/admin/tenants/audit?tenant={{.ID}}">{{.ID}}</a></td>
  <td>{{.WorkOSOrgID}}</td>
  <td>{{.LifecycleState}}</td>
  <td>{{.Pool}}</td>
  <td>{{.OnboardedAt}}</td>
  <td>{{if .LastAuditLogEvent.Valid}}{{.LastAuditLogEvent.Time}}{{else}}—{{end}}</td>
</tr>
{{end}}
</table>
</body></html>`))

func renderTenantList(w http.ResponseWriter, rows []TenantRow) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tenantListTmpl.Execute(w, rows)
}

var auditListTmpl = template.Must(template.New("audit").Parse(`<!doctype html>
<html><head><title>Neksur SaaS — Audit Log</title></head>
<body>
<h1>Audit Log — tenant {{.TenantID}}</h1>
<table border="1" cellpadding="4">
<tr><th>id</th><th>occurred_at</th><th>event_type</th><th>actor_user</th><th>payload</th></tr>
{{range .Rows}}
<tr>
  <td>{{.ID}}</td>
  <td>{{.OccurredAt}}</td>
  <td>{{.EventType}}</td>
  <td>{{if .ActorUserID.Valid}}{{.ActorUserID.String}}{{else}}—{{end}}</td>
  <td><pre>{{printf "%s" .Payload}}</pre></td>
</tr>
{{end}}
</table>
</body></html>`))

func renderAuditList(w http.ResponseWriter, tenantID uuid.UUID, rows []AuditRow) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = auditListTmpl.Execute(w, struct {
		TenantID uuid.UUID
		Rows     []AuditRow
	}{TenantID: tenantID, Rows: rows})
}
