// TrinoInjector — sqlproxy.Injector implementation for the Trino
// dialect. Wave 2 Plan 02-05 dispatch B + Plan 02-12 CR-A3 splicer.
//
// Plan 03-09 extension: pin-aware FROM rewrite using Trino's native
// `FOR VERSION AS OF <snapshot_id>` time-travel syntax. When a named
// snapshot pin is active for the table, InjectPolicy rewrites the FROM
// clause to target the pinned snapshot BEFORE calling SpliceArtifact
// so the row filter applies to the correct snapshot's data.
//
// Tiebreaker rule for multiple active pins (see ActivePinsForTable):
// the pin with the latest ExpiryUTC is preferred (longest remaining
// lifetime → most intentional / freshest operator directive). In the
// rare case of an exact tie, the pin with the lexicographically
// largest pin_name is chosen (deterministic, no random element).

package dialect

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	"github.com/neksur-com/neksur/internal/tenant"
)

// SnapshotPinReader is the narrow interface the Trino + Dremio injectors
// need from the L1 SnapshotPin store (coordination/snapshot.PinStore).
// Defined here in the dialect package to avoid a circular import between
// the sqlproxy/dialect package and the coordination/snapshot package.
//
// *snapshot.PinStore satisfies this interface via an adapter wrapper
// (provided by the wiring layer in cmd/neksur-server). Tests inject a
// fake implementation. nil means "no pin store configured" — injectors
// skip the pin-rewrite step when this field is nil.
type SnapshotPinReader interface {
	// ActiveSnapshotIDForTable returns the highest-priority active snapshot
	// pin for the table in the calling tenant's context. Returns ("", nil)
	// when no active pin exists. Returns an error only when the store
	// query fails (not when the pin is absent).
	//
	// Tiebreaker (when multiple pins are active): the pin with the latest
	// ExpiryUTC wins; on an exact ExpiryUTC tie, the lexicographically
	// largest pin_name wins. This is deterministic and documented in the
	// trino.go package comment above.
	ActiveSnapshotIDForTable(ctx context.Context, ref iceberg.TableRef) (snapshotID string, err error)
}

// TrinoInjector serves the "trino" engine kind. Thread-safe: the
// CompiledStore and LRU cache are both safe for concurrent use, and
// InjectPolicy holds no per-request mutable state.
type TrinoInjector struct {
	store    *store.CompiledStore
	cache    *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]
	pinStore SnapshotPinReader // nil → pin rewrite skipped (L1 + no-pin path)
}

// NewTrinoInjector constructs a TrinoInjector bound to the given
// CompiledStore + LRU cache (shared across all dialect implementations
// — the CacheKey carries the Engine field so per-dialect entries never
// collide). The pinStore field defaults to nil (pin rewrite skipped).
// Use NewTrinoInjectorWithPin to enable pin-aware FROM rewriting.
func NewTrinoInjector(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry]) *TrinoInjector {
	return &TrinoInjector{store: s, cache: cache}
}

// NewTrinoInjectorWithPin constructs a TrinoInjector with pin-aware FROM
// rewriting enabled. The pinStore must satisfy SnapshotPinReader; when a
// named snapshot pin is active for the table, InjectPolicy will rewrite
// the FROM clause to `FROM <table> FOR VERSION AS OF <snapshot_id>` before
// applying the row-filter / column-mask splice.
//
// Passing nil for pinStore is equivalent to calling NewTrinoInjector.
func NewTrinoInjectorWithPin(s *store.CompiledStore, cache *lru.Cache[sqlproxy.CacheKey, sqlproxy.ArtifactEntry], pinStore SnapshotPinReader) *TrinoInjector {
	return &TrinoInjector{store: s, cache: cache, pinStore: pinStore}
}

// InjectPolicy fetches the active Trino CompiledPolicy artifact for
// (tenant=ctx, table) and splices it into `query` via
// dialect.SpliceArtifact (Plan 02-12 — replaces the iter-1 no-op
// comment-appending rewrite). The artifact kind discriminator drives
// row-filter WHERE-conjunction vs column-mask projection rewriting.
//
// Plan 03-09 pin-aware pre-splice: when pinStore is non-nil and an
// active snapshot pin exists for the table, the query's FROM clause is
// rewritten to `FROM <table> FOR VERSION AS OF <snapshot_id>` BEFORE
// SpliceArtifact is called. This ensures the row filter applies to the
// pinned snapshot's data, not the current head snapshot.
//
// Per Pitfall 11: this method never logs the query body or the
// artifact body — error returns wrap sqlproxy sentinels only.
func (i *TrinoInjector) InjectPolicy(ctx context.Context, query string, table sqlproxy.TableRef, principal sqlproxy.Claims) (string, string, error) {
	tid, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: tenant missing: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	// Pin-aware FROM rewrite: applies BEFORE the artifact splice so row
	// filters target the pinned snapshot. Skipped when pinStore is nil.
	if i.pinStore != nil {
		ref := iceberg.TableRef{Namespace: []string{table.Namespace}, Name: table.Name}
		snapID, perr := i.pinStore.ActiveSnapshotIDForTable(ctx, ref)
		if perr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: pin lookup: %w", sqlproxy.ErrPolicyEngineUnavailable)
		}
		if snapID != "" {
			pinned, rerr := RewriteFromForSnapshotPin(query, snapID, PinDialectTrino)
			if rerr != nil {
				return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: pin rewrite: %w", rerr)
			}
			query = pinned
		}
	}

	cacheKey := sqlproxy.CacheKey{
		TenantID:  tid.String(),
		Namespace: table.Namespace,
		Table:     table.Name,
		Engine:    "trino",
	}

	if entry, hit := i.cache.Get(cacheKey); hit {
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusHit, nil
	}

	compiled, err := i.store.LoadCompiledForTable(ctx, iceberg.TableRef{
		Namespace: []string{table.Namespace},
		Name:      table.Name,
	})
	if err != nil {
		return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: load: %w", sqlproxy.ErrPolicyEngineUnavailable)
	}

	for _, cp := range compiled {
		if cp.EngineKind != "trino" || cp.Status != store.CompiledPolicyStatusActive {
			continue
		}
		// Predicate-kind artifacts are gateway-handled (Layer 1 commit
		// validation) — they must NOT reach the sqlproxy path. Skip
		// silently so a tenant with mixed kinds gets enforcement on the
		// splice-eligible artifacts and the gateway handles the rest.
		if cp.ArtifactKind == store.KindPredicate {
			continue
		}
		entry := sqlproxy.ArtifactEntry{Body: []byte(cp.ArtifactBody), Kind: cp.ArtifactKind}
		i.cache.Add(cacheKey, entry)
		rewritten, rerr := SpliceArtifact(query, entry.Body, entry.Kind, principal)
		if rerr != nil {
			return "", sqlproxy.CacheStatusError, fmt.Errorf("dialect/trino: %w", rerr)
		}
		return rewritten, sqlproxy.CacheStatusMiss, nil
	}

	return "", sqlproxy.CacheStatusMiss, fmt.Errorf("dialect/trino: no active policy: %w", sqlproxy.ErrPolicyEngineUnavailable)
}
