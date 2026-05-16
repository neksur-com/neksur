// sqlproxy HTTP server — RESEARCH §Pattern 7 + D-2.08. Routes a single
// catch-all endpoint
//
//	POST /v1/sql/{engine}/{prefix}/...
//
// through the per-dialect Injector dispatcher and emits the three
// Wave 2 observability families (sql_proxy_overhead_ms histogram,
// sql_proxy_lookup_total counter, sql_proxy_inject_failures_total
// counter).
//
// HTTP status conventions:
//   - 200 — success; body is a JSON {"rewritten_query": "..."} envelope.
//     (Phase 3 wires real engine forwarding; this dispatch terminates
//     at injection.)
//   - 400 — malformed path identifier (engine / prefix segment failed
//     identifierRegex) OR body parse error.
//   - 413 — request body exceeded 1 MiB cap (http.MaxBytesReader).
//   - 422 — Injector returned ErrInjectionFailed (un-rewritable SQL).
//   - 500 — tenant ctx missing OR unexpected Injector error.
//   - 501 — engine kind not registered (Dremio / Snowflake stubs) OR
//     Injector returned ErrEngineNotSupported.
//   - 503 — Injector returned ErrPolicyEngineUnavailable (fail-closed
//     mirror of the L1 gateway's D-1.09 path).
//
// Per Pitfall 11 the handler logs ONLY metadata (engine, table.Name,
// status_code, latency_ms) — NEVER the query body or rewritten body.

package sqlproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/observability"
	"github.com/neksur-com/neksur/internal/tenant"
)

// maxSQLBodyBytes is the per-request body cap. 1 MiB comfortably
// accommodates the SQL statement + binding metadata; pipelines
// emitting larger bodies should split the query upstream of the proxy.
const maxSQLBodyBytes = 1 << 20

// identifierRegex restricts the {engine} and {prefix} path segments
// to safe characters. Cypher / SQL / URL-traversal attacks via path
// segments (T-1-malformed-path-injection — mirror of the L1 gateway's
// identifierRegex) are blocked here BEFORE the values reach the
// Injector dispatcher.
var identifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Server is the sqlproxy HTTP server. Construct via NewServer; serve
// via ListenAndServeTLS. Thread-safe.
type Server struct {
	deps Deps
	mux  *http.ServeMux
	srv  *http.Server
}

// Deps is the constructor-injected dependency bag for the sqlproxy
// HTTP server. Construct ONCE at neksur-server startup; pass the
// value (NOT a pointer) to NewServer.
//
// All fields except Logger are required:
//   - Injectors — per-dialect Injector implementations, keyed by
//     engine kind ("trino", "spark", "bigquery", "databricks").
//     Missing keys cause the handler to emit 501 + audit log entry.
//   - TLSConfig — assembled via NewTLSConfig; enforces TLS 1.3 +
//     mTLS-required client auth.
//   - AuditLogger — slog.Logger used for audit-row emission
//     (engine name on 501 responses). Distinct from the Logger
//     field so callers can route audit to a separate sink in
//     Phase 3.
//   - Logger — structured logger for handler diagnostics (metadata
//     only, NEVER query body — Pitfall 11). Defaults to slog.Default()
//     when nil.
type Deps struct {
	Injectors   map[string]Injector
	TLSConfig   *tls.Config
	AuditLogger *slog.Logger
	Logger      *slog.Logger
}

// NewServer wires the dispatcher and HTTP mux. Returns an error if
// Deps.Injectors is nil or Deps.TLSConfig is nil — both are required
// for the proxy to start safely.
func NewServer(deps Deps) (*Server, error) {
	if deps.Injectors == nil {
		return nil, fmt.Errorf("sqlproxy: NewServer: Deps.Injectors is required")
	}
	if deps.TLSConfig == nil {
		return nil, fmt.Errorf("sqlproxy: NewServer: Deps.TLSConfig is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.AuditLogger == nil {
		deps.AuditLogger = deps.Logger
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/sql/{engine}/{prefix}/", s.handleInject)
	return s, nil
}

// Handler returns the mounted http.Handler. Exposed for tests that
// want to drive the server via httptest.NewServer without TLS.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServeTLS starts the HTTPS listener on `addr`. Blocking;
// returns http.ErrServerClosed on clean Shutdown.
func (s *Server) ListenAndServeTLS(addr string) error {
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		TLSConfig:         s.deps.TLSConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Empty cert/key paths — TLSConfig.GetCertificate (CertWatcher)
	// supplies the server cert dynamically.
	return s.srv.ListenAndServeTLS("", "")
}

// Shutdown gracefully stops the underlying *http.Server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// sqlRequest is the JSON envelope the proxy accepts on POST /v1/sql.
// The Table identifier travels in the body (NOT the URL path) so the
// namespace + table can carry the dot-joined namespace shape (the
// URL path's {prefix} segment is the catalog nickname, distinct from
// the table namespace).
type sqlRequest struct {
	Query string `json:"query"`
	Table struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"table"`
	// Principal is the typed subset of Claims the proxy forwards to
	// the Injector. Sourced from the request body in dispatch A; the
	// production wiring (dispatch C) will swap this for the ExtractPrincipal
	// chain (mTLS SAN > Authorization bearer > WorkOS session) before
	// the body unmarshal step.
	Principal struct {
		Sub   string   `json:"sub"`
		Email string   `json:"email"`
		Roles []string `json:"roles"`
	} `json:"principal"`
}

// handleInject implements POST /v1/sql/{engine}/{prefix}/...
func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	engine := r.PathValue("engine")
	prefix := r.PathValue("prefix")

	// Latency observation closure — captured at the end of every
	// response path. Pitfall 11: log metadata only.
	finish := func(status int, table string) {
		latencyMs := float64(time.Since(start).Milliseconds())
		observability.SqlProxyOverheadMs.WithLabelValues(engine).Observe(latencyMs)
		s.deps.Logger.Info("sqlproxy: request",
			"engine", engine,
			"prefix", prefix,
			"table", table,
			"status_code", status,
			"latency_ms", latencyMs,
		)
	}

	// Step 1 — tenant ctx (CC1). TenantMiddleware is the wire-layer
	// gate; this assertion is defence-in-depth.
	if _, ok := tenant.IDFromContext(r.Context()); !ok {
		http.Error(w, "tenant missing", http.StatusInternalServerError)
		finish(http.StatusInternalServerError, "")
		return
	}

	// Step 2 — path identifier validation
	// (T-1-malformed-path-injection mirror).
	if !identifierRegex.MatchString(engine) || !identifierRegex.MatchString(prefix) {
		http.Error(w, "malformed path identifier", http.StatusBadRequest)
		finish(http.StatusBadRequest, "")
		return
	}

	// Step 3 — body read with 1 MiB cap.
	limited := http.MaxBytesReader(w, r.Body, maxSQLBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		// http.MaxBytesReader returns a *MaxBytesError; map to 413.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			finish(http.StatusRequestEntityTooLarge, "")
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		finish(http.StatusBadRequest, "")
		return
	}

	// Step 4 — body unmarshal.
	var req sqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		finish(http.StatusBadRequest, "")
		return
	}
	if req.Query == "" || req.Table.Name == "" {
		http.Error(w, "query and table.name are required", http.StatusBadRequest)
		finish(http.StatusBadRequest, req.Table.Name)
		return
	}

	// Step 5 — Injector dispatch.
	injector, ok := s.deps.Injectors[engine]
	if !ok {
		observability.SqlProxyInjectFailuresTotal.
			WithLabelValues(engine, observability.ReasonSqlProxyEngineNotSupported).Inc()
		s.deps.AuditLogger.Warn("sqlproxy: engine not registered",
			"engine", engine,
			"table", req.Table.Name,
		)
		http.Error(w, "engine not supported", http.StatusNotImplemented)
		finish(http.StatusNotImplemented, req.Table.Name)
		return
	}

	// Step 6 — InjectPolicy.
	tableRef := TableRef{Namespace: req.Table.Namespace, Name: req.Table.Name}
	claims := Claims{Sub: req.Principal.Sub, Email: req.Principal.Email, Roles: req.Principal.Roles}
	rewritten, cacheStatus, ierr := injector.InjectPolicy(r.Context(), req.Query, tableRef, claims)
	if ierr != nil {
		// Error → status mapping per package-doc table.
		switch {
		case errors.Is(ierr, ErrPolicyEngineUnavailable):
			// WR-A3: do NOT increment commit_rejected_total here. That
			// counter is documented as "L1 catalog gateway only" — the
			// sqlproxy path emits sql_proxy_inject_failures_total instead
			// so Phase 1 alert rules on commit_rejected_total are not
			// perturbed by sqlproxy traffic (paging semantics stay honest).
			observability.SqlProxyInjectFailuresTotal.
				WithLabelValues(engine, observability.ReasonSqlProxyPolicyEngineUnavailable).Inc()
			s.deps.Logger.Error("sqlproxy: policy engine unavailable",
				"err", ierr, "engine", engine, "table", req.Table.Name)
			http.Error(w, "policy engine unavailable", http.StatusServiceUnavailable)
			finish(http.StatusServiceUnavailable, req.Table.Name)
			return
		case errors.Is(ierr, ErrEngineNotSupported) || errors.Is(ierr, iceberg.ErrAdapterStub):
			observability.SqlProxyInjectFailuresTotal.
				WithLabelValues(engine, observability.ReasonSqlProxyEngineNotSupported).Inc()
			s.deps.AuditLogger.Warn("sqlproxy: engine reported not-supported",
				"engine", engine, "table", req.Table.Name)
			http.Error(w, "engine not supported", http.StatusNotImplemented)
			finish(http.StatusNotImplemented, req.Table.Name)
			return
		case errors.Is(ierr, ErrInjectionFailed):
			observability.SqlProxyInjectFailuresTotal.
				WithLabelValues(engine, observability.ReasonSqlProxyInjectionFailed).Inc()
			s.deps.Logger.Warn("sqlproxy: injection failed",
				"err", ierr, "engine", engine, "table", req.Table.Name)
			http.Error(w, "policy injection failed", http.StatusUnprocessableEntity)
			finish(http.StatusUnprocessableEntity, req.Table.Name)
			return
		default:
			observability.SqlProxyInjectFailuresTotal.
				WithLabelValues(engine, observability.ReasonSqlProxyInjectionFailed).Inc()
			s.deps.Logger.Error("sqlproxy: unexpected injector error",
				"err", ierr, "engine", engine, "table", req.Table.Name)
			http.Error(w, "internal error", http.StatusInternalServerError)
			finish(http.StatusInternalServerError, req.Table.Name)
			return
		}
	}

	// Step 7 — observe cache outcome.
	cs := normalizeCacheStatus(cacheStatus)
	observability.SqlProxyLookupTotal.WithLabelValues(engine, cs).Inc()

	// Step 8 — write success envelope. Phase 3 wires real engine
	// forwarding; this dispatch terminates at injection.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"rewritten_query": rewritten,
	}); err != nil {
		// At this point the status code is already written; just log.
		s.deps.Logger.Error("sqlproxy: response encode failed",
			"err", err, "engine", engine, "table", req.Table.Name)
	}
	finish(http.StatusOK, req.Table.Name)
}

// normalizeCacheStatus coerces the Injector-reported cache status to
// one of the three documented label values. Unknown values surface as
// "error" rather than uncontrolled cardinality (cardinality cap —
// mirrors the CommitRejectedTotal pattern).
func normalizeCacheStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case observability.CacheStatusHit:
		return observability.CacheStatusHit
	case observability.CacheStatusMiss:
		return observability.CacheStatusMiss
	default:
		return observability.CacheStatusError
	}
}
