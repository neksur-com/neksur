// Package iceberg defines the catalog-agnostic surface every Phase 1
// adapter satisfies — a tiny 6-method interface (D-1.01) plus the
// shared types ingestion + the L1 gateway pass between adapter calls.
//
// Per-catalog adapters (sub-packages: polaris, nessie, unity, glue)
// own their own Config struct (D-1.03 declarative; no
// functional-options pattern, no runtime capability flags) and the
// translation between iceberg-go's lower-level catalog client and
// these shared types.
//
// Sentinel errors live HERE rather than in a separate errors.go file
// because the surface is intentionally narrow (4 sentinels + 1
// interface + a handful of value types) — Phase 0.5's internal/billing
// follows the same one-file convention with ErrBillingDisabled +
// ErrInvalidSignature alongside its Billing interface.
//
// Pattern provenance:
//   - Interface shape: 01-RESEARCH.md §Pattern 1 lines 488-596 (verbatim
//     for the public surface; comments expanded for context).
//   - Sentinel + wrapper: 01-PATTERNS.md CC5 / Phase 0
//     internal/graph/client.go:20 (ErrUnboundedTraversal).
//   - Per-snapshot MetadataLocation: D-1.04 — Iceberg writes a fresh
//     metadata.json at a globally-unique S3 path per snapshot, which
//     is the canonical natural key for ingestion MERGEs.
package iceberg

import (
	"context"
	"errors"
	"time"
)

// IcebergCatalogClient is the 7-method surface every catalog adapter
// satisfies. D-1.01 + D-1.03: tiny interface + per-catalog Config.
// Phase 2 Plan 02-07 extends Phase 1's 6-method interface with
// IssueScopedSTSCredentials (L4 credential vending per D-2.09).
//
// The interface is intentionally narrow — Phase 1 ingestion +
// L1 gateway need exactly these six operations and nothing more.
// Capabilities() is the static-facts escape hatch for callers that
// must branch on per-catalog quirks (Polaris credential vending,
// Nessie branches, Glue/Unity max-namespace-depth) without growing
// the per-method API surface.
//
// All methods accept context.Context for cancellation propagation
// (Phase 0.5 pattern: every external-IO call is context-cancellable
// so the per-tenant pgxpool BeforeAcquire DISCARD ALL hook can
// guarantee no session bleed across requests when a request is
// cancelled mid-flight).
type IcebergCatalogClient interface {
	// ListTables enumerates the tables in a single namespace level.
	// Phase 1 callers pass a single-component namespace string; the
	// adapter is responsible for translating that to whatever the
	// upstream catalog expects (e.g., Polaris's flat-string-list
	// identifier, Glue's database name).
	ListTables(ctx context.Context, namespace string) ([]TableRef, error)

	// GetTable returns the lightweight metadata projection for ref.
	// Phase 1 collapses GetTable and LoadTable to the same wire call
	// — the distinction exists so Phase 2+ can introduce an
	// HTTP-level HEAD-style fast path without breaking callers.
	GetTable(ctx context.Context, ref TableRef) (*TableMetadata, error)

	// LoadTable returns the full metadata projection (including
	// snapshots and the manifest URIs ingestion needs).
	LoadTable(ctx context.Context, ref TableRef) (*TableMetadata, error)

	// CommitTable forwards a CommitRequest (requirements + updates)
	// to the upstream catalog. Adapters MUST translate iceberg-go's
	// 409 / CommitFailedException to ErrCommitConflict so the L1
	// gateway can convert it back to a 409 over the wire.
	CommitTable(ctx context.Context, ref TableRef, req CommitRequest) (*CommitResult, error)

	// ExpireSnapshots removes snapshots committed before olderThan.
	// Phase 1 implements this via a CommitTable that issues a
	// remove-snapshots Update for the matching snapshot IDs (Pitfall
	// 9 — the canonical expire path goes through the REST catalog).
	ExpireSnapshots(ctx context.Context, ref TableRef, olderThan time.Time) error

	// Capabilities returns the static facts the gateway / scheduler
	// branches on (e.g., does this catalog publish Iceberg events?
	// does it support credential vending? what is the max namespace
	// depth so the planner can reject ListTables on too-deep paths?).
	Capabilities() Capabilities

	// IssueScopedSTSCredentials issues short-lived AWS STS credentials
	// scoped to the specified table and region per D-2.09 (L4 credential
	// vending). The Polaris adapter calls Polaris's vended-credentials
	// path (X-Iceberg-Access-Delegation header); Unity and Glue stub
	// adapters return iceberg.ErrAdapterStub (Phase 3 lights these live).
	//
	// Session policy narrows the returned credentials to s3:PutObject
	// only on the table prefix and allowed region (Pitfall 1: Resource
	// must be a JSON array, not a bare string).
	//
	// Returns ErrAdapterStub for catalogs that do not yet support
	// live credential vending (Unity, Glue in Phase 2).
	IssueScopedSTSCredentials(ctx context.Context, table TableRef, region string) (*STSCredentials, error)
}

// STSCredentials holds the short-lived AWS STS credentials returned by
// the L4 credential vending path (D-2.09). All fields are required on
// a successful Polaris vend; Expiration is parsed from the Polaris 1.4
// loadTable response config block (s3.session-expiration — Iceberg REST
// #11118 standardization, Pitfall 7).
type STSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
	Region          string
}

// TableRef is the catalog-agnostic table identifier. Namespace is a
// list because Iceberg REST tables live under multi-segment paths
// (e.g., ["prod","sales","orders"]) — Phase 1 callers typically use
// single-segment namespaces but the type doesn't constrain that.
type TableRef struct {
	Namespace []string
	Name      string
}

// TableMetadata is the subset of Iceberg metadata Phase 1 ingests
// and validates. Larger catalog metadata structures (StatisticsFiles,
// SnapshotLog, MetadataLog, SortOrders) are deferred to Phase 4 when
// the semantic engine / compaction coordinator need them.
type TableMetadata struct {
	UUID              string
	Schema            Schema
	PartitionSpec     PartitionSpec
	CurrentSnapshotID int64
	// MetadataLocation is the canonical natural key per D-1.04 —
	// Iceberg writes a fresh metadata.json at a globally-unique S3
	// path per commit, so this is collision-free and join-free at
	// MERGE time. Ingestion uses this as the Snapshot vlabel's
	// primary key (`MERGE (s:Snapshot { metadata_location: $loc })`).
	MetadataLocation string
	Snapshots        []Snapshot
	Properties       map[string]string
}

// Schema is a flat list of columns. Phase 1 doesn't need nested
// struct / map / list types — the gateway's CEL policies operate on
// column names + Iceberg primitive type names. Nested types come
// online in Phase 4 (semantic engine).
type Schema struct {
	Fields []SchemaField
}

// SchemaField is a single Iceberg column projection. Type is the
// Iceberg type name (e.g., "long", "string", "decimal(10,2)") as a
// string so the gateway / CEL bindings don't have to depend on
// iceberg-go's typed Type interface.
type SchemaField struct {
	ID       int
	Name     string
	Type     string
	Required bool
	Doc      string
}

// PartitionSpec is the table's current partition spec. SpecID is the
// monotonically-increasing version Iceberg assigns to each spec
// revision (used in lineage queries to track partition evolution).
type PartitionSpec struct {
	SpecID int
	Fields []PartitionField
}

// PartitionField represents one transform from a source column to a
// partition value. Transform is the Iceberg transform name as a
// string ("identity", "bucket[16]", "hours", "days", "months",
// "years", "void"). Phase 1 stores these as strings; Phase 4 may
// adopt iceberg-go's typed Transform interface if pushdown needs it.
type PartitionField struct {
	SourceColumnID int
	Transform      string
	Name           string
}

// Snapshot is one Iceberg snapshot. ParentSnapshotID is 0 when the
// snapshot has no parent (the table's bootstrap snapshot). MetadataLocation
// is the per-snapshot metadata.json URL — distinct from
// TableMetadata.MetadataLocation (which is the most-recent commit's
// metadata file). Per D-1.04 each Snapshot row in the graph is
// keyed on this URL.
type Snapshot struct {
	SnapshotID       int64
	ParentSnapshotID int64
	TimestampMs      int64
	Operation        string
	Manifests        []ManifestRef
	Summary          map[string]string
	MetadataLocation string
}

// ManifestRef is one manifest file Iceberg references from a
// snapshot. PartSpecID is the partition-spec version this manifest
// was written under (different from the table's current spec when
// the table has been re-partitioned).
type ManifestRef struct {
	Path            string
	Length          int64
	PartSpecID      int
	AddedSnapshotID int64
}

// CommitRequest mirrors the Iceberg REST OpenAPI commit body —
// requirements assertions + the list of updates to apply. Phase 1
// stores these as untyped maps (TableRequirement / TableUpdate are
// `map[string]any`) so the gateway can intercept and inspect them
// for policy evaluation without depending on iceberg-go's typed
// Update interface; the polaris adapter marshals to iceberg-go's
// typed shape at the wire boundary.
type CommitRequest struct {
	Requirements []TableRequirement
	Updates      []TableUpdate
}

// TableRequirement is one assertion the upstream catalog must
// validate before applying updates (e.g., "current schema id must
// equal 7"). Untyped map for transparent gateway inspection.
type TableRequirement map[string]any

// TableUpdate is one mutation to apply (e.g., "add snapshot",
// "remove snapshots", "set properties"). Untyped map for transparent
// gateway inspection — see Pitfall 9 (P3 retention policy reads
// commit.updates for `remove-snapshots` actions).
type TableUpdate map[string]any

// CommitResult is the catalog's response to a successful commit.
// AcceptedAt is the wall-clock time the adapter received the
// success response (NOT the upstream catalog's commit timestamp;
// some catalogs don't expose a precise commit time).
type CommitResult struct {
	AcceptedAt          time.Time
	NewMetadataLocation string
	NewSnapshotID       int64
}

// Capabilities is the static facts the adapter publishes about its
// backing catalog. Lets the gateway / scheduler skip features the
// catalog doesn't support without rolling per-catalog branches in
// orchestration code. Examples (per RESEARCH lines 583-588):
//
//   - polaris:    SupportsCredVend=true (STS), SupportsWebhooks=true,
//                 MaxNamespaceDepth=100, SupportsBranches=false
//   - nessie:     SupportsBranches=true, SupportsWebhooks=partial,
//                 MaxNamespaceDepth=1
//   - glue:       MaxNamespaceDepth=1, others=false
//   - unity:      SupportsCredVend=true, SupportsWebhooks=true,
//                 MaxNamespaceDepth=1
//
// The zero value (Capabilities{}) is valid — every field is the
// false-y zero default — which keeps test scaffolds and stub
// adapters trivial to construct without panic.
type Capabilities struct {
	Name              string
	SupportsBranches  bool
	SupportsCredVend  bool
	SupportsWebhooks  bool
	MaxNamespaceDepth int
}

// Sentinel errors. Per the PATTERNS CC5 + Phase 0
// internal/graph/client.go (ErrUnboundedTraversal) convention,
// errors flow through the package so callers branch on
// errors.Is(err, iceberg.Err…) rather than string-matching.
//
//   - ErrTableNotFound: returned by GetTable / LoadTable / CommitTable
//     when the upstream catalog reports table-does-not-exist (404 /
//     catalog.ErrNoSuchTable). The L1 gateway maps this to 404 at
//     the HTTP boundary.
//
//   - ErrCommitConflict: returned by CommitTable when the upstream
//     catalog reports the commit was rejected because the table's
//     current state doesn't satisfy the request's Requirements (409
//     / CommitFailedException). Callers re-LOAD the table and
//     reapply updates against the new metadata.
//
//   - ErrCredentialsExpired: returned by any method when the upstream
//     catalog reports the OAuth bearer token is expired and
//     iceberg-go's automatic refresh did not succeed (Pitfall 1
//     mitigation requires `oauth2-server-uri` + `credential` props
//     so this should be rare; left as a sentinel for the case where
//     the OAuth server itself is unreachable).
//
//   - ErrAdapterStub: returned by the live unity adapter's
//     IssueScopedSTSCredentials until Unity STS is wired (Plan 03-03
//     ships the Unity REST adapter live; STS vending is a follow-on).
//     Also returned by the live Glue adapter's IssueScopedSTSCredentials
//     until Glue STS is wired (Plan 03-04 ships live Glue REST adapter;
//     STS vending is a follow-on). Callers detect this via
//     errors.Is(err, ErrAdapterStub).
var (
	ErrTableNotFound      = errors.New("iceberg: table not found")
	ErrCommitConflict     = errors.New("iceberg: commit conflict (rebase required)")
	ErrCredentialsExpired = errors.New("iceberg: credentials expired")
	ErrAdapterStub        = errors.New("iceberg: adapter is a stub (use Polaris or Nessie in Phase 1)")
)
