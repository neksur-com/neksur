// service.go — credvend.Service implements the L4 credential vending business
// logic per D-2.09 + RESEARCH PATTERNS lines 749-762.
//
// Service sits between the HTTP handler (handler.go) and the iceberg adapter
// (polaris/adapter.go). It owns the cache read/write and Prometheus counter
// increments so the handler stays thin.
//
// Fail-closed contract (D-1.09 carryover):
//
//	Any error from the adapter's IssueScopedSTSCredentials wraps
//	ErrCredVendUnavailable. The handler maps this to HTTP 503. This matches
//	the L1 gateway's fail-closed semantics for policy engine unavailability.
//
// Per-tenant adapter pattern (matches the gateway handler's approach):
//
//	The credvend handler builds the per-tenant iceberg adapter from the
//	catalog credential store on each request (same as gateway/handler.go).
//	The Service receives the pre-built adapter as a call argument so the
//	caching + counter logic stays decoupled from adapter construction.
package credvend

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// Service implements L4 credential vending per D-2.09. Thread-safe —
// all state (cache, counters) is safe for concurrent use.
type Service struct {
	// cache holds the TTL-based LRU cache for issued STS credentials.
	// Refresh-at-TTL/2 per Anti-Pattern 3 + reactive on-401 invalidation.
	cache *Cache

	// l4TokenIssuedTotal counts successful STS issuances (cache miss path
	// that produces a new credential). Labels: engine, region.
	l4TokenIssuedTotal *prometheus.CounterVec

	// l4TokenRefreshTotal counts cache-miss events that trigger an upstream
	// IssueScopedSTSCredentials call. Labels: engine.
	l4TokenRefreshTotal *prometheus.CounterVec
}

// NewService constructs a Service. cache, l4TokenIssuedTotal, and
// l4TokenRefreshTotal MUST be non-nil. Use observability.L4Token* counters.
func NewService(
	cache *Cache,
	l4TokenIssuedTotal *prometheus.CounterVec,
	l4TokenRefreshTotal *prometheus.CounterVec,
) *Service {
	return &Service{
		cache:               cache,
		l4TokenIssuedTotal:  l4TokenIssuedTotal,
		l4TokenRefreshTotal: l4TokenRefreshTotal,
	}
}

// Issue returns short-lived STS credentials for the given table and region,
// serving from the cache when available or issuing fresh via the adapter.
//
// Per RESEARCH PATTERNS lines 749-762:
//  1. cache.Get — if hit AND not near TTL/2, return immediately.
//  2. cache miss path: call adapter.IssueScopedSTSCredentials.
//  3. On success: cache.Add + emit l4_token_issued_total counter.
//  4. On failure: wrap as ErrCredVendUnavailable (fail-closed → HTTP 503).
//
// tenantID is included in the cache key (T-2-sts-token-cache-cross-tenant).
// namespace is the first namespace component of table.Namespace (empty
// string if table.Namespace is nil — handled defensively).
//
// adapter is the per-tenant IcebergCatalogClient built by the handler
// from the credential store (same pattern as gateway/handler.go's adapterFor).
func (s *Service) Issue(ctx context.Context, tenantID string, adapter iceberg.IcebergCatalogClient, table iceberg.TableRef, region string) (*iceberg.STSCredentials, error) {
	namespace := ""
	if len(table.Namespace) > 0 {
		namespace = table.Namespace[0]
	}

	caps := adapter.Capabilities()
	engineName := caps.Name

	// Cache read — fast path.
	if creds := s.cache.Get(tenantID, namespace, table.Name, region); creds != nil {
		return creds, nil
	}

	// Cache miss — increment refresh counter before the upstream call.
	s.l4TokenRefreshTotal.WithLabelValues(engineName).Inc()

	// Check if the adapter supports credential vending.
	if !caps.SupportsCredVend {
		return nil, fmt.Errorf("credvend: issue: %w: catalog %q does not support STS credential vending",
			ErrEngineNotSupported, engineName)
	}

	// Upstream STS issuance via the catalog adapter.
	creds, err := adapter.IssueScopedSTSCredentials(ctx, table, region)
	if err != nil {
		// Map iceberg.ErrAdapterStub to ErrEngineNotSupported.
		if errors.Is(err, iceberg.ErrAdapterStub) {
			return nil, fmt.Errorf("credvend: issue: %w: %s", ErrEngineNotSupported, err)
		}
		// All other errors: fail-closed → ErrCredVendUnavailable (HTTP 503).
		return nil, fmt.Errorf("credvend: issue: %w: %s", ErrCredVendUnavailable, err)
	}

	// Populate cache and emit counter.
	s.cache.Add(tenantID, namespace, table.Name, region, creds)
	s.l4TokenIssuedTotal.WithLabelValues(engineName, region).Inc()

	return creds, nil
}
