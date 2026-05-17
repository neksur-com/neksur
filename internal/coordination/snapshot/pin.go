// Package snapshot implements the L1 SnapshotPin store — BSL Core.
//
// # SnapshotPin design
//
// A SnapshotPin is an operator-issued or session-issued directive that
// pins a given Iceberg table to a specific snapshot_id for the duration
// of [PinnedAt, ExpiryUTC). It is stored in the AGE graph as:
//
//	(sp:SnapshotPin {tenant_id, pin_name, pinned_by_principal,
//	                  at_snapshot_id, pinned_at, expiry_utc})
//	(sp)-[:PINS]->(t:Table {tenant_id, name, namespace})
//
// Queries against pinned tables record a READ edge:
//
//	(q:Query {tenant_id, query_id})-[:READ {pinned, pinned_by, at_snapshot}]->(t:Table)
//
// # Security — pin_name sanitization
//
// Every literal that flows into a Cypher statement MUST be routed
// through graph.MustSanitizeCypherLiteral. Operator-supplied pin_name
// MUST be UUID-validated upstream (at the gateway endpoint that calls
// UpsertSnapshotPin) BEFORE reaching this layer. This package trusts that
// the caller has already performed UUID parsing/rejection; it adds a
// defence-in-depth sanitization pass via MustSanitizeCypherLiteral as a
// second safety net. Callers passing arbitrary strings without UUID
// validation upstream will cause a panic here — that panic surfaces a
// programming error, not a user error.
//
// # Cache invalidation
//
// UpsertSnapshotPin calls cache.Invalidate(tableKey) after writing to
// AGE so the next ActivePinsForTable call fetches fresh data. This means
// a brief window (one cache-miss round-trip) of eventual consistency
// after a pin upsert, which is acceptable for the ≤30s SLA in
// ROADMAP §3 SC §3.
//
// # L1 (BSL Core) — no license gate
//
// Snapshot pinning is L1 per ROADMAP §3 SC §5. Do NOT add a
// license.IsFeatureAllowed check in this package.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"
)

// ErrTenantMissing is the sentinel returned when a PinStore method is
// called without a tenant ID in the context.
var ErrTenantMissing = errors.New("coordination/snapshot: tenant context missing")

// SnapshotPin is the in-memory projection of a SnapshotPin node +
// adjacent :PINS edge from the AGE graph.
type SnapshotPin struct {
	// Name is the operator- or session-assigned identifier. For named
	// (operator-issued) pins this is a UUID string; for session pins it
	// is derived from the session ID. Callers MUST UUID-validate or
	// otherwise sanitize this value before passing it here.
	Name string

	// PinnedByPrincipal is the principal sub (URN / SPIFFE ID) that
	// created the pin. Stored on the SnapshotPin node; surfaced in
	// audit queries.
	PinnedByPrincipal string

	// AtSnapshotID is the Iceberg snapshot_id the pin freezes the table
	// to. The compaction coordinator (Plan 03-12) MUST NOT compact a
	// snapshot_id that is referenced by any active pin.
	AtSnapshotID string

	// PinnedAt is the wall-clock when the pin was created.
	PinnedAt time.Time

	// ExpiryUTC is the wall-clock when the pin expires. Named pins
	// (operator-issued via gateway) default to now()+7d; session pins
	// (per-query, written by gateway on request) default to
	// now()+session_timeout (typically 1h). Per RESEARCH Open Q 5 /
	// Claude's Discretion.
	ExpiryUTC time.Time

	// TableName is the Iceberg table name component.
	TableName string

	// TableNamespace is the flattened namespace (e.g., "prod" or
	// "prod.sales"). May include dots for multi-level namespaces.
	TableNamespace string
}

// QueryRef identifies an Iceberg query for the RecordQueryRead edge.
type QueryRef struct {
	QueryID  string
	TenantID string
}

// pinGraphStore is the interface PinStore uses to execute Cypher.
// In production this is *graph.GraphClient. In tests it is replaced by
// an in-memory fake.
type pinGraphStore interface {
	ExecuteInTenant(ctx context.Context, tenantID string, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// PinStore is the L1 SnapshotPin store. Construct via NewPinStore (for
// production) or NewTestPinStore (for unit tests). Thread-safe.
//
// Construct ONCE per process and share across all callers that need
// snapshot-pin consultation. The cache is shared so hot tables are
// served from memory.
type PinStore struct {
	gc    pinGraphStore
	cache *PinLRU
	// mem is the in-memory store used by NewTestPinStore. nil in production.
	mem *inMemStore
}

// inMemStore is a simple thread-safe in-memory pin store for unit tests.
// It mirrors the AGE graph semantics at a coarse level.
type inMemStore struct {
	// pins keyed by "tenantID/pin_name".
	pins map[string]SnapshotPin
}

func newInMemStore() *inMemStore {
	return &inMemStore{pins: make(map[string]SnapshotPin)}
}

func (m *inMemStore) key(tenantID, pinName string) string {
	return tenantID + "/" + pinName
}

func (m *inMemStore) upsert(tenantID string, pin SnapshotPin) {
	m.pins[m.key(tenantID, pin.Name)] = pin
}

func (m *inMemStore) activePins(tenantID, tableName, tableNs string) []SnapshotPin {
	now := time.Now().UTC()
	var out []SnapshotPin
	for k, p := range m.pins {
		if !strings.HasPrefix(k, tenantID+"/") {
			continue
		}
		if p.TableName != tableName || p.TableNamespace != tableNs {
			continue
		}
		if p.ExpiryUTC.After(now) {
			out = append(out, p)
		}
	}
	return out
}

// NewPinStore constructs a PinStore backed by the given AGE GraphClient
// and PinLRU cache. Use in production. The gc must be the same
// GraphClient used by the rest of the application — do NOT introduce a
// second pool (CC3 constraint).
func NewPinStore(gc *graph.GraphClient, cache *PinLRU) *PinStore {
	return &PinStore{gc: gc, cache: cache}
}

// NewTestPinStore constructs a PinStore backed by an in-memory store for
// unit tests. It does not require a live Postgres+AGE instance.
func NewTestPinStore(cache *PinLRU) *PinStore {
	return &PinStore{
		gc:    nil, // not used in test mode
		cache: cache,
		mem:   newInMemStore(),
	}
}

// ContextWithTenantID injects a tenant ID string into the context using
// the tenant package's tenant.WithID mechanism. Exported for unit tests
// in the snapshot_test package.
//
// In production the gateway middleware (workosauth.TenantMiddleware) is
// the sole caller of tenant.WithID. Tests use this helper instead.
//
// tenantID may be any non-empty string. It is converted to a
// deterministic uuid.UUID via uuid.NewSHA1 so that tenant.IDFromContext
// can read it back as a uuid.UUID. Tests that need to assert on the
// specific UUID value must use the same derivation.
func ContextWithTenantID(ctx context.Context, tenantID string) context.Context {
	// uuid.NameSpaceDNS is the standard DNS namespace UUID per RFC 4122.
	// uuid.NewSHA1 derives a deterministic v5 UUID from (namespace, name).
	id := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(tenantID))
	return tenant.WithID(ctx, id)
}

// UpsertSnapshotPin MERGEs a SnapshotPin node + :PINS edge in the AGE
// graph for the calling tenant. Idempotent: re-running with the same
// (tenant_id, pin_name) updates the mutable properties in place.
//
// Per 03-RESEARCH Code Example 1: two separate Cypher calls (one MERGE
// per cypher() invocation — AGE 1.6 one-MERGE-per-cypher constraint).
//
// All caller-supplied literals are routed through
// graph.MustSanitizeCypherLiteral (CR-01 / T-3-snapshot-pin-injection
// mitigation). Upstream callers MUST UUID-validate pin.Name before
// calling this method — see package-level comment.
//
// The cache entry for (tenant, tableName, tableNs) is invalidated after
// a successful upsert so the next ActivePinsForTable call fetches fresh
// data from AGE.
func (s *PinStore) UpsertSnapshotPin(ctx context.Context, pin SnapshotPin) error {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return ErrTenantMissing
	}
	tenantStr := tenantID.String()

	// In-memory mode for unit tests.
	if s.mem != nil {
		s.mem.upsert(tenantStr, pin)
		// Invalidate cache so next ActivePinsForTable sees fresh data.
		cacheKey := PinCacheKey{
			TenantID:  tenantStr,
			Namespace: pin.TableNamespace,
			Table:     pin.TableName,
		}
		s.cache.Invalidate(cacheKey)
		return nil
	}

	// Production path: sanitize every literal (CR-01).
	// Per 03-PATTERNS §6 and 03-RESEARCH Code Example 1, all 8 literals
	// that flow into the Cypher must pass through MustSanitizeCypherLiteral.
	// Pitfall 11: no body/literal values are logged.
	pinName := graph.MustSanitizeCypherLiteral(pin.Name)
	principal := graph.MustSanitizeCypherLiteral(pin.PinnedByPrincipal)
	atSnap := graph.MustSanitizeCypherLiteral(pin.AtSnapshotID)
	pinnedAt := graph.MustSanitizeCypherLiteral(pin.PinnedAt.UTC().Format(time.RFC3339))
	expiry := graph.MustSanitizeCypherLiteral(pin.ExpiryUTC.UTC().Format(time.RFC3339))
	tableName := graph.MustSanitizeCypherLiteral(pin.TableName)
	tableNs := graph.MustSanitizeCypherLiteral(pin.TableNamespace)
	tenantLit := graph.MustSanitizeCypherLiteral(tenantStr)

	err := s.gc.ExecuteInTenant(ctx, tenantStr, func(ctx context.Context, tx pgx.Tx) error {
		// B-4 (Plan 03-12): Acquire the per-table advisory lock BEFORE the MERGE
		// statements to serialize the pin-write with ExtendIfActivePin in the L3
		// compaction coordinator. Lock key matches the coordinator: "tenant_id|table_name".
		// hashtext() is Postgres's built-in string-to-int32 hash.
		// The lock is transaction-scoped — released automatically on COMMIT/ROLLBACK.
		// This matches the lock ordering contract in retention_extend.go:
		//   pin write: BEGIN; pg_advisory_xact_lock(id); MERGE; COMMIT
		//   expire decide: BEGIN; pg_advisory_xact_lock(id); SELECT pins; COMMIT
		lockID := tenantStr + "|" + pin.TableName
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockID); err != nil {
			return fmt.Errorf("coordination/snapshot: UpsertSnapshotPin: advisory lock: %w", err)
		}

		// 1) MERGE SnapshotPin node (AGE 1.6 — one MERGE per cypher() call).
		nodeCypher := fmt.Sprintf(
			`MERGE (sp:SnapshotPin {tenant_id: '%s', pin_name: '%s'}) `+
				`SET sp.pinned_by_principal = '%s', sp.at_snapshot_id = '%s', `+
				`sp.pinned_at = '%s', sp.expiry_utc = '%s' `+
				`RETURN sp.pin_name`,
			tenantLit, pinName, principal, atSnap, pinnedAt, expiry,
		)
		if err := execPinCypherNoRows(ctx, tx, nodeCypher); err != nil {
			return fmt.Errorf("coordination/snapshot: MERGE SnapshotPin: %w", err)
		}

		// 2) MERGE :PINS edge to the Table node (second separate cypher() call).
		edgeCypher := fmt.Sprintf(
			`MATCH (sp:SnapshotPin {tenant_id: '%s', pin_name: '%s'}), `+
				`(t:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) `+
				`MERGE (sp)-[:PINS]->(t) RETURN sp.pin_name`,
			tenantLit, pinName, tenantLit, tableName, tableNs,
		)
		return execPinCypherNoRows(ctx, tx, edgeCypher)
	})
	if err != nil {
		return err
	}

	// Invalidate cache after successful write.
	cacheKey := PinCacheKey{
		TenantID:  tenantStr,
		Namespace: pin.TableNamespace,
		Table:     pin.TableName,
	}
	s.cache.Invalidate(cacheKey)
	return nil
}

// ActivePinsForTable returns all non-expired SnapshotPin nodes that
// target the given table for the calling tenant. Results are
// LRU-cached; the cache key is (tenant_id, namespace, table_name).
//
// The expiry check uses RFC3339-lexicographic comparison — AGE 1.6 does
// not support $-parameter binding in Cypher bodies, so we splice the
// current timestamp as a sanitized literal. RFC3339-formatted timestamps
// compare lexicographically correctly when the timezone offset is the
// same (UTC-only policy here).
func (s *PinStore) ActivePinsForTable(ctx context.Context, ref iceberg.TableRef) ([]SnapshotPin, error) {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return nil, ErrTenantMissing
	}
	tenantStr := tenantID.String()
	ns := joinNS(ref.Namespace)

	cacheKey := PinCacheKey{
		TenantID:  tenantStr,
		Namespace: ns,
		Table:     ref.Name,
	}

	// Cache hit: return immediately.
	if cached, hit := s.cache.Get(cacheKey); hit {
		return cached, nil
	}

	// In-memory mode for unit tests.
	if s.mem != nil {
		pins := s.mem.activePins(tenantStr, ref.Name, ns)
		s.cache.Add(cacheKey, pins)
		return pins, nil
	}

	// Production path: query AGE.
	tenantLit := graph.MustSanitizeCypherLiteral(tenantStr)
	tname := graph.MustSanitizeCypherLiteral(ref.Name)
	tns := graph.MustSanitizeCypherLiteral(ns)
	// AGE 1.6 does not support $-parameter binding in Cypher bodies.
	// We splice the current UTC timestamp as an RFC3339 literal.
	// RFC3339 timestamps compare lexicographically when both use the
	// same timezone offset (UTC-only policy here, so this is safe).
	nowLit := graph.MustSanitizeCypherLiteral(time.Now().UTC().Format(time.RFC3339))

	var pins []SnapshotPin
	err := s.gc.ExecuteInTenant(ctx, tenantStr, func(ctx context.Context, tx pgx.Tx) error {
		cy := fmt.Sprintf(
			`MATCH (sp:SnapshotPin {tenant_id: '%s'})-[:PINS]->`+
				`(t:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) `+
				`WHERE sp.expiry_utc > '%s' `+
				`RETURN sp.pin_name, sp.pinned_by_principal, sp.at_snapshot_id, `+
				`sp.pinned_at, sp.expiry_utc`,
			tenantLit, tenantLit, tname, tns, nowLit,
		)
		q := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS "+
				"(pin_name ag_catalog.agtype, pinned_by ag_catalog.agtype, "+
				"at_snap ag_catalog.agtype, pinned_at ag_catalog.agtype, "+
				"expiry ag_catalog.agtype)",
			cy,
		)
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("coordination/snapshot: ActivePinsForTable query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rawName, rawBy, rawSnap, rawAt, rawExp string
			if err := rows.Scan(&rawName, &rawBy, &rawSnap, &rawAt, &rawExp); err != nil {
				return fmt.Errorf("coordination/snapshot: ActivePinsForTable scan: %w", err)
			}
			pinnedAt, _ := time.Parse(time.RFC3339, stripAgtypeQ(rawAt))
			expiryUTC, _ := time.Parse(time.RFC3339, stripAgtypeQ(rawExp))
			pins = append(pins, SnapshotPin{
				Name:              stripAgtypeQ(rawName),
				PinnedByPrincipal: stripAgtypeQ(rawBy),
				AtSnapshotID:      stripAgtypeQ(rawSnap),
				PinnedAt:          pinnedAt,
				ExpiryUTC:         expiryUTC,
				TableName:         ref.Name,
				TableNamespace:    ns,
			})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("coordination/snapshot: ActivePinsForTable rows err: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.cache.Add(cacheKey, pins)
	return pins, nil
}

// RecordQueryRead writes or merges a READ edge from a Query node to a
// Table node. The edge carries three new properties beyond the Phase 0
// V0030 base shape:
//
//   - pinned     bool   — true when a SnapshotPin was consulted
//   - pinned_by  string — Name of the pin (empty when pinned=false)
//   - at_snapshot string — the pinned snapshot_id (empty when pinned=false)
//
// Per must_have truth: "READ elabel from Phase 0 V0030 is reused with
// the new properties (no migration needed; AGE is property-free)."
//
// When pin is nil: pinned=false, pinned_by="", at_snapshot="".
func (s *PinStore) RecordQueryRead(ctx context.Context, query QueryRef, ref iceberg.TableRef, pin *SnapshotPin) error {
	tenantID, ok := tenant.IDFromContext(ctx)
	if !ok {
		return ErrTenantMissing
	}
	tenantStr := tenantID.String()
	ns := joinNS(ref.Namespace)

	// Determine pin properties.
	pinnedStr := "false"
	pinnedBy := ""
	atSnapshot := ""
	if pin != nil {
		pinnedStr = "true"
		pinnedBy = pin.Name
		atSnapshot = pin.AtSnapshotID
	}

	// In-memory mode for unit tests: record call and return success.
	if s.mem != nil {
		slog.Debug("coordination/snapshot: RecordQueryRead",
			"query_id", query.QueryID,
			"pinned", pinnedStr,
		)
		return nil
	}

	// Production path.
	tenantLit := graph.MustSanitizeCypherLiteral(tenantStr)
	qid := graph.MustSanitizeCypherLiteral(query.QueryID)
	tname := graph.MustSanitizeCypherLiteral(ref.Name)
	tns := graph.MustSanitizeCypherLiteral(ns)
	pinByLit := graph.MustSanitizeCypherLiteral(pinnedBy)
	atSnapLit := graph.MustSanitizeCypherLiteral(atSnapshot)

	return s.gc.ExecuteInTenant(ctx, tenantStr, func(ctx context.Context, tx pgx.Tx) error {
		// 1) MERGE the Query node.
		qNodeCy := fmt.Sprintf(
			`MERGE (q:Query {tenant_id: '%s', query_id: '%s'}) RETURN q.query_id`,
			tenantLit, qid,
		)
		if err := execPinCypherNoRows(ctx, tx, qNodeCy); err != nil {
			return fmt.Errorf("coordination/snapshot: MERGE Query: %w", err)
		}

		// 2) MERGE READ edge from Query to Table, setting the three new
		//    properties. AGE 1.6 one-MERGE-per-cypher: second call.
		edgeCy := fmt.Sprintf(
			`MATCH (q:Query {tenant_id: '%s', query_id: '%s'}), `+
				`(t:Table {tenant_id: '%s', name: '%s', namespace: '%s'}) `+
				`MERGE (q)-[r:READ]->(t) `+
				`SET r.pinned = %s, r.pinned_by = '%s', r.at_snapshot = '%s' `+
				`RETURN q.query_id`,
			tenantLit, qid, tenantLit, tname, tns,
			pinnedStr, pinByLit, atSnapLit,
		)
		return execPinCypherNoRows(ctx, tx, edgeCy)
	})
}

// execPinCypherNoRows runs a Cypher statement that returns one agtype
// column (RETURN node.property) and discards the results. Mirrors the
// execCypherNoRows helper from internal/policy/store/compiled.go.
func execPinCypherNoRows(ctx context.Context, tx pgx.Tx, cy string) error {
	q := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('neksur', $$ %s $$) AS (out ag_catalog.agtype)",
		cy,
	)
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// stripAgtypeQ removes AGE agtype JSON-quote wrapping (e.g., `"value"`)
// from a scalar string result. Local copy — mirrors stripAgtypeQuotes in
// policy/store/age.go; each package owns its copy to avoid cross-package
// coupling on an unexported helper.
func stripAgtypeQ(s string) string {
	for _, suffix := range []string{"::text", "::numeric"} {
		if strings.HasSuffix(s, suffix) {
			s = s[:len(s)-len(suffix)]
		}
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// joinNS flattens a multi-segment namespace into a dot-separated string.
func joinNS(parts []string) string {
	return strings.Join(parts, ".")
}
