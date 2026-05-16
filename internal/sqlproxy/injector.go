// Injector — the per-dialect rewriter interface the sqlproxy HTTP
// handler dispatches on. One Injector implementation per supported
// engine kind (Trino, Spark, BigQuery, Databricks in Plan 02-05;
// Dremio + Snowflake light up in Phase 3).
//
// Concrete implementations live under internal/sqlproxy/dialect/ and
// land in dispatch B. This file declares the contract only.

package sqlproxy

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/neksur-com/neksur/internal/policy/store"
)

// TableRef is the proxy-local table identifier — namespace + name,
// matching the iceberg.TableRef shape but flattened to a single
// namespace string (the proxy's URL path carries a single segment
// per RESEARCH §Pattern 7; multi-segment Iceberg namespaces are
// dot-joined upstream of the proxy).
type TableRef struct {
	// Namespace is the dot-joined namespace path (e.g. "sales.us").
	Namespace string

	// Name is the unqualified table name (e.g. "orders").
	Name string
}

// Claims is the principal projection passed to the Injector — it
// matches the Phase 1 ExtractPrincipal shape (Sub + Email + Roles)
// so existing CEL bindings (principal.sub / principal.roles) align
// without a translation layer. Per Pitfall 8, the proxy trusts the
// upstream chain (mTLS / Authorization bearer / WorkOS session) and
// does NOT re-verify JWT signatures here.
type Claims struct {
	// Sub is the subject identifier (mTLS SAN URI / JWT sub /
	// WorkOS user id).
	Sub string

	// Email is the user's email when available.
	Email string

	// Roles is the role list for ACL evaluation (P2 / ABAC).
	Roles []string
}

// CacheStatus values reported by Injector implementations on each
// InjectPolicy call. The server emits these verbatim as the
// `cache_status` label on sql_proxy_lookup_total — the cardinality
// is bounded to the three values below (the metric registration
// rejects any other label value at registration time, but Go's type
// system can't enforce the constraint, so callers MUST use these
// constants).
const (
	// CacheStatusHit signals the artifact was served from the
	// process-local LRU.
	CacheStatusHit = "hit"

	// CacheStatusMiss signals the artifact was fetched from the
	// CompiledStore and cached for future requests.
	CacheStatusMiss = "miss"

	// CacheStatusError signals the cache layer threw — the request
	// proceeded against the store directly (best-effort fallback).
	CacheStatusError = "error"
)

// CacheKey is the LRU key shape — (TenantID, Namespace, Table,
// Engine). All four fields are required for correctness: the
// TenantID prevents cross-tenant cache hits (CC1); the Namespace
// + Table identify the row-level policy; the Engine isolates per-
// dialect artifacts (a Trino SQL fragment must NOT serve a Spark
// request).
type CacheKey struct {
	TenantID  string
	Namespace string
	Table     string
	Engine    string
}

// Injector is the per-dialect rewriter contract. Implementations
// are constructed via BuildInjector and stored in Server.Deps.Injectors
// keyed by their EngineKind string ("trino", "spark", "bigquery",
// "databricks").
//
// Thread-safety: implementations MUST be safe for concurrent use —
// the sqlproxy HTTP handler dispatches each request on a fresh
// goroutine and shares one Injector instance across all callers.
type Injector interface {
	// InjectPolicy rewrites `query` against the active CompiledPolicy
	// for (tenant=ctx, table, engine=implementation kind). Returns
	// the rewritten SQL, the cache lookup outcome (CacheStatusHit /
	// Miss / Error), and an error.
	//
	// Error contract:
	//
	//   - sqlproxy.ErrPolicyEngineUnavailable — store fetch failed
	//     or active artifact malformed. Server maps to 503.
	//   - sqlproxy.ErrEngineNotSupported — implementation refuses
	//     the request (e.g., Dremio stub). Server maps to 501.
	//   - sqlproxy.ErrInjectionFailed — query did not parse or the
	//     emitter rejected the rewrite. Server maps to 422.
	//   - any other error — server maps to 500 (unexpected).
	//
	// Per Pitfall 11 implementations MUST NOT log the query body.
	InjectPolicy(ctx context.Context, query string, table TableRef, principal Claims) (rewritten string, cacheStatus string, err error)
}

// InjectorDeps is the constructor-injected dependency bag shared by
// every dialect implementation. Construct ONCE at neksur-server
// startup; pass the value (NOT a pointer) to BuildInjector.
//
// All fields are required unless documented otherwise.
type InjectorDeps struct {
	// Store is the CompiledStore (AGE-backed) — the dialect reads
	// LoadCompiledForTable to fetch the active CompiledPolicy
	// artifact for the request's (table, engine) pair.
	Store *store.CompiledStore

	// Cache is the process-local LRU for compiled artifacts. Shared
	// across all dialect implementations: the CacheKey carries the
	// Engine field so per-dialect entries never collide.
	Cache *lru.Cache[CacheKey, []byte]
}

// BuildInjector is the factory the neksur-server wiring (dispatch C)
// calls once per supported engine kind. Returns ErrEngineNotSupported
// if the engineKind is not one of the four Plan 02-05 dialects
// ("trino", "spark", "bigquery", "databricks") — Dremio + Snowflake
// callers receive the sentinel and the wiring layer either skips the
// registration (preferred) or registers a stub Injector that returns
// ErrEngineNotSupported on every call.
//
// Dispatch boundary: the concrete dialect constructors land in
// dispatch B. This factory's body is intentionally a stub until then
// — it returns ErrEngineNotSupported for every kind so dispatch A
// compiles standalone. Dispatch B replaces the switch body with the
// per-dialect constructor calls (dialect.NewTrinoInjector(deps) etc.).
func BuildInjector(engineKind string, _ InjectorDeps) (Injector, error) {
	switch engineKind {
	case "trino", "spark", "bigquery", "databricks":
		// Dispatch B replaces this branch with the per-dialect
		// constructor. Returning the sentinel here keeps dispatch A
		// compilable on its own; the wiring layer (dispatch C) will
		// not be wired until B lands.
		return nil, fmt.Errorf("sqlproxy: BuildInjector(%q): dispatch B not yet landed: %w", engineKind, ErrEngineNotSupported)
	default:
		return nil, fmt.Errorf("sqlproxy: BuildInjector(%q): %w", engineKind, ErrEngineNotSupported)
	}
}
