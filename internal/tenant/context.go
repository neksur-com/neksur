package tenant

import (
	"context"

	"github.com/google/uuid"
)

// tenantIDKey is the unexported context-key type. Using a private struct
// type (NOT a string) is the canonical Go idiom for context keys; it
// guarantees zero collision with any other package's context values.
// External code can only inject/read via the WithID + IDFromContext
// helpers below — direct ctx.Value(tenantIDKey{}) is impossible from
// outside this package.
type tenantIDKey struct{}

// TenantIDKey is exported for the rare callers (testing-only) that need
// to assert the key shape directly. Production code uses WithID + IDFromContext.
//
// Why exported despite the unexported type? Symmetry with Phase 0's
// tenant_id GUC name (`app.current_tenant`) — having the key publicly
// named lets test diagnostics print the key's shape via `%T`.
var TenantIDKey = tenantIDKey{}

// WithID returns a copy of ctx that carries the given tenant ID. This
// is the SOLE injection point — only the WorkOS middleware should call
// it during request handling.
//
// The tenant ID is a UUID v4 per D-0.5.04 (the canonical format). The
// value flows through to internal/tenant/dbsession.go::WithTenantTx,
// which reads it back and applies the three SET LOCAL layers.
func WithID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// IDFromContext returns the tenant ID previously stored by WithID and
// `ok = true`; if no tenant is in context the zero UUID and `ok = false`
// are returned. Callers should NOT proceed to a database call when
// `!ok` — return ErrTenantNotInContext instead.
func IDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v := ctx.Value(tenantIDKey{})
	if v == nil {
		return uuid.UUID{}, false
	}
	id, ok := v.(uuid.UUID)
	if !ok {
		return uuid.UUID{}, false
	}
	return id, true
}
