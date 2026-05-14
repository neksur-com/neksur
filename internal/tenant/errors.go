package tenant

import "errors"

// Sentinel errors used by middleware, provisioning, and tenant
// lifecycle code. Naming follows the Phase 0 idiom established by
// internal/graph/client.go:20 (ErrUnboundedTraversal) — lowercase
// "package: short-reason" prefixes so wrapped error messages remain
// grep-friendly.
//
// Callers compare with errors.Is. Most are returned via wrapping:
//
//	return fmt.Errorf("repo: byworkosorgid: %w", tenant.ErrTenantNotFound)
//
// then middleware does:
//
//	if errors.Is(err, tenant.ErrTenantNotFound) { http.Error(... 404) }
var (
	// ErrTenantNotFound — a WorkOS session is valid but no row in
	// public.tenants matches its organization_id. Middleware returns
	// HTTP 404 to the caller (D-0.5.21 T-0.5-rls-bypass-without-guc).
	ErrTenantNotFound = errors.New("tenant: not found")

	// ErrTenantSuspended — public.tenants.lifecycle_state == 'suspended'.
	// Read paths continue; commit paths return 503 (D-0.5.20 contract).
	// Plan 04+ enforces; Plan 03 declares the sentinel for forward use.
	ErrTenantSuspended = errors.New("tenant: suspended")

	// ErrCrossTenantAccess — defence-in-depth assertion. The RLS Layer 3
	// predicate + Layer 2 GRANT should already prevent cross-tenant
	// access; this sentinel is reserved for code paths that detect a
	// mismatched tenant in a query result (the impossible-by-design path).
	ErrCrossTenantAccess = errors.New("tenant: cross-tenant access denied")

	// ErrWorkOSSessionInvalid — JWT verification failed or session expired.
	// Middleware returns HTTP 401 (D-0.5.21 T-0.5-session-hijack).
	ErrWorkOSSessionInvalid = errors.New("workos: session invalid")

	// ErrVPCPeeringNotActive — used by Plan 04/05 customer-peering code
	// paths when an outgoing call discovers the peering is "pending-acceptance"
	// or "expired". Declared here for forward compatibility per plan
	// must-haves: "ErrVPCPeeringNotActive ... can be declared here for
	// forward compatibility".
	ErrVPCPeeringNotActive = errors.New("tenant: VPC peering not active")

	// ErrTenantNotInContext — WithTenantTx (or any other ctx-dependent
	// tenant call) was invoked on a context that has no tenant ID. This
	// indicates a coding error (handler bypassed the middleware) — the
	// caller should return 500 to the user.
	ErrTenantNotInContext = errors.New("tenant: not in context")

	// ErrValidation — base sentinel for the regex validators in id.go.
	// Wrapping shape: `fmt.Errorf("tenant: %w: not a ...: %q", ErrValidation, s)`.
	ErrValidation = errors.New("validation failed")
)
