// nessieAdapter — Project Nessie 0.100+ flavored IcebergCatalogClient
// implementation built on top of github.com/apache/iceberg-go's REST
// catalog client (the same library that powers the Polaris adapter —
// Nessie speaks Iceberg REST natively).
//
// Wire layer:
//
//   - Branch selection via the `nessie.commit.ref` property (the
//     canonical iceberg-go mechanism — confirmed by Nessie's spec
//     [projectnessie.org/docs/develop/spec] cited from RESEARCH §132
//     and §1417 "Don't Hand-Roll" table). The string key is visible
//     in source for grep-based audit (Pitfall 2 mitigation: branch
//     selection is auditable in code review, not buried in test
//     setup).
//
//   - Auth: Phase 1 supports `none` (Nessie testcontainer runs
//     unauthenticated) and `bearer` (production — token passed
//     through iceberg-go's `token` property, the Iceberg REST
//     OpenAPI key for static bearer auth). `aws-iam` is rejected
//     at Validate (Phase 3 work).
//
//   - Branching is set GLOBALLY at construction time; the same
//     adapter instance always operates on the configured branch.
//     Per-branch test sub-routines instantiate new adapters or use
//     the testfixture's CreateBranch helper (Plan 01-01).
//
// Error translation (identical to Polaris adapter — same upstream
// library, same error shapes):
//
//   - catalog.ErrNoSuchTable             → iceberg.ErrTableNotFound
//   - 401 / 403 in error message         → iceberg.ErrCredentialsExpired
//   - "CommitFailedException" / 409 / "conflict" → iceberg.ErrCommitConflict
//
// All wrapping uses fmt.Errorf with `%w` so errors.Is keeps working
// up the call chain.
package nessie

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	icebergGo "github.com/apache/iceberg-go"
	icebergCatalog "github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	icebergTable "github.com/apache/iceberg-go/table"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// nessieWarehouse is the default warehouse name passed to iceberg-go.
// Nessie 0.100 requires a non-empty warehouse declared in its
// `nessie.catalog.warehouses.<name>.location` server config; the
// adapter forwards the same name as the `warehouse` Iceberg REST
// property so Nessie's `/iceberg/v1/config?warehouse=<name>` lookup
// succeeds at adapter construction time.
//
// "warehouse" is Nessie's documented default name (matching the
// testcontainer fixture's env-var convention
// `nessie.catalog.default-warehouse=warehouse`). Production
// deployments override this via a future Config.Warehouse field if
// a Nessie deployment ever needs per-warehouse routing.
const nessieWarehouse = "warehouse"

// nessieAdapter wraps iceberg-go's *rest.Catalog and translates
// between the Phase 1 IcebergCatalogClient surface and iceberg-go's
// lower-level types. The struct is unexported (the only public
// constructor is New); callers obtain an iceberg.IcebergCatalogClient
// interface, never a typed pointer to this struct.
type nessieAdapter struct {
	cfg Config
	cat *rest.Catalog
}

// Compile-time interface assertion. If a future refactor removes
// one of the IcebergCatalogClient methods, this declaration fails
// to compile — blocking the change with a clear signal at build
// time (Polaris adapter pattern).
var _ iceberg.IcebergCatalogClient = (*nessieAdapter)(nil)

// New constructs a Nessie-flavored IcebergCatalogClient. Validates
// cfg, applies defaults, builds the iceberg-go REST catalog client
// with `nessie.commit.ref=<branch>` for branch selection (and
// optional bearer-token auth), and wraps it in a nessieAdapter.
//
// Returns an error if cfg.Validate() fails OR if iceberg-go cannot
// connect to the Nessie endpoint (the REST catalog probes
// `<endpoint>/v1/config` at construction; DNS / network / 5xx
// failures surface here). On success the returned client is safe
// to call concurrently — iceberg-go's REST catalog is designed to
// be shared across goroutines.
func New(ctx context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Wire props. The grep-detectable string keys below are the
	// documented Iceberg REST + Nessie surface:
	//
	//   - uri                — iceberg-go's REST catalog base URI.
	//   - warehouse          — Nessie warehouse name; iceberg-go
	//     forwards as `?warehouse=<name>` on the /v1/config probe.
	//   - nessie-commit-ref  — branch selection key (Pitfall 2,
	//     auditable in code review). The literal wire string
	//     `nessie.commit.ref` (see the Properties map below) is
	//     the documented Nessie key per
	//     [projectnessie.org/docs/develop/spec]; it survives across
	//     iceberg-go releases that may add native Nessie-prop
	//     routing.
	//   - prefix             — Iceberg REST `prefix` property; this
	//     is what iceberg-go v0.5.0 actually consumes to route
	//     subsequent calls to /v1/<prefix>/namespaces/... — Nessie
	//     interprets `<prefix>` as the branch reference. Setting
	//     `prefix` to the configured branch is what makes the
	//     adapter's operations land on the correct branch.
	//     (Live-probe-confirmed during Plan 01-03 Task 1: a
	//     namespace created with prefix=neksur-test does NOT show
	//     up under prefix=main and vice versa — the Nessie branch
	//     model is fully isolated through this knob.)
	//   - token              — bearer token, when AuthMode=bearer.
	//
	// We pass props via WithAdditionalProps so the same wire shape
	// works whether the caller is on iceberg-go v0.5.x (current
	// pin) or a future v0.6+. The redundant rest.WithPrefix call
	// belt-and-suspenders the same routing — iceberg-go accepts
	// both the typed option and the props-map form, and the
	// grep-detectable map literal makes the branch routing
	// auditable in code review (Pitfall 2 mitigation: branch
	// selection is in source, not buried in test setup).
	props := icebergGo.Properties{
		"uri":               cfg.Endpoint,
		"warehouse":         nessieWarehouse,
		"nessie.commit.ref": cfg.DefaultBranch,
		"prefix":            cfg.DefaultBranch,
	}
	if cfg.AuthMode == AuthModeBearer {
		props["token"] = cfg.BearerToken
	}

	cat, err := rest.NewCatalog(ctx, "nessie", cfg.Endpoint,
		rest.WithWarehouseLocation(nessieWarehouse),
		rest.WithPrefix(cfg.DefaultBranch),
		rest.WithAdditionalProps(props),
	)
	if err != nil {
		return nil, fmt.Errorf("nessie: new catalog: %w", err)
	}
	return &nessieAdapter{cfg: cfg, cat: cat}, nil
}

// ListTables enumerates tables under namespace via the Nessie REST
// catalog. Phase 1 callers pass a single-segment namespace string;
// the adapter converts it to iceberg-go's table.Identifier
// (`[]string{namespace}`) at the wire boundary.
func (n *nessieAdapter) ListTables(ctx context.Context, namespace string) ([]iceberg.TableRef, error) {
	ns := icebergTable.Identifier{namespace}
	out := make([]iceberg.TableRef, 0)
	for ident, err := range n.cat.ListTables(ctx, ns) {
		if err != nil {
			return nil, n.translateError("nessie: list tables", err)
		}
		// iceberg-go returns identifiers as []string where the
		// last component is the table name and everything prior
		// is the namespace. Phase 1 single-level namespaces mean
		// len == 2 typically.
		if len(ident) == 0 {
			continue
		}
		ref := iceberg.TableRef{Name: ident[len(ident)-1]}
		if len(ident) > 1 {
			ref.Namespace = append([]string(nil), ident[:len(ident)-1]...)
		}
		out = append(out, ref)
	}
	return out, nil
}

// GetTable is a lightweight LoadTable in Phase 1 — Iceberg REST has
// no separate HEAD-style endpoint, so the wire call is identical.
// The distinction in the IcebergCatalogClient interface gives Phase
// 2+ a place to introduce a metadata-only fast path without
// breaking callers.
func (n *nessieAdapter) GetTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return n.LoadTable(ctx, ref)
}

// LoadTable fetches the table's full metadata projection from
// Nessie and converts iceberg-go's *table.Table to the Phase 1
// shared *iceberg.TableMetadata shape. Returns iceberg.ErrTableNotFound
// (wrapped) when the upstream catalog reports the table doesn't
// exist; iceberg.ErrCredentialsExpired when bearer auth is rejected;
// and a generic wrapped error for other failures.
func (n *nessieAdapter) LoadTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	ident := toIdentifier(ref)
	tbl, err := n.cat.LoadTable(ctx, ident)
	if err != nil {
		return nil, n.translateError("nessie: load table", err)
	}
	return convertTable(tbl), nil
}

// CommitTable forwards a CommitRequest to Nessie via iceberg-go's
// CommitTable. Phase 1 only accepts empty Requirements / Updates
// from CommitTable callers — the typed-dispatcher landing in Plan
// 01-06 (gateway commit-proxy) will own non-empty conversions.
// This matches the Polaris adapter's behavior exactly (same iceberg-go
// limitation: ParseRequirement is public, ParseUpdate is not).
//
// Returns iceberg.ErrCommitConflict (wrapped) when the upstream
// catalog reports the table state changed under us (409 /
// CommitFailedException); iceberg.ErrTableNotFound when the table
// disappeared between LoadTable and CommitTable; and generic
// wrapped errors for transport / auth / parse failures.
func (n *nessieAdapter) CommitTable(ctx context.Context, ref iceberg.TableRef, req iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	ident := toIdentifier(ref)

	requirements, err := convertRequirements(req.Requirements)
	if err != nil {
		return nil, fmt.Errorf("nessie: commit table: convert requirements: %w", err)
	}
	updates, err := convertUpdates(req.Updates)
	if err != nil {
		return nil, fmt.Errorf("nessie: commit table: convert updates: %w", err)
	}

	meta, newLoc, err := n.cat.CommitTable(ctx, ident, requirements, updates)
	if err != nil {
		if isCommitConflict(err) {
			return nil, fmt.Errorf("nessie: commit table: %w", iceberg.ErrCommitConflict)
		}
		return nil, n.translateError("nessie: commit table", err)
	}

	res := &iceberg.CommitResult{
		AcceptedAt:          time.Now().UTC(),
		NewMetadataLocation: newLoc,
	}
	if curr := currentSnapshot(meta); curr != nil {
		res.NewSnapshotID = curr.SnapshotID
	}
	return res, nil
}

// ExpireSnapshots removes snapshots committed before olderThan via
// a CommitTable that issues a remove-snapshots Update for the
// matching snapshot IDs. Pitfall 9 — this is the canonical expire
// path through the REST catalog; direct file-rewrite paths bypass
// the gateway entirely (Plan 01-07 L3 detection backstops that).
//
// On Nessie this commits to the configured `nessie.commit.ref`
// branch — expiring snapshots on `neksur-test` does NOT affect
// `main` or other sibling branches. (That is the whole point of
// Nessie's branching model — Plan 01-03 is what proves the
// adapter respects it.)
func (n *nessieAdapter) ExpireSnapshots(ctx context.Context, ref iceberg.TableRef, olderThan time.Time) error {
	tbl, err := n.cat.LoadTable(ctx, toIdentifier(ref))
	if err != nil {
		return n.translateError("nessie: expire snapshots: load", err)
	}
	cutoffMs := olderThan.UnixMilli()
	var doomed []int64
	for _, snap := range tbl.Metadata().Snapshots() {
		if snap.TimestampMs < cutoffMs {
			doomed = append(doomed, snap.SnapshotID)
		}
	}
	if len(doomed) == 0 {
		return nil
	}
	updates := []icebergTable.Update{icebergTable.NewRemoveSnapshotsUpdate(doomed)}
	if _, _, err := n.cat.CommitTable(ctx, toIdentifier(ref), nil, updates); err != nil {
		if isCommitConflict(err) {
			return fmt.Errorf("nessie: expire snapshots: %w", iceberg.ErrCommitConflict)
		}
		return n.translateError("nessie: expire snapshots", err)
	}
	return nil
}

// Capabilities returns the static facts the gateway / scheduler
// branches on. Numbers per RESEARCH lines 583-588:
//
//   - Name=nessie. Identifies the catalog flavor for routing.
//   - SupportsBranches=true. THE Nessie differentiator (Polaris is
//     non-branching; Nessie's branching model is the entire reason
//     Plan 01-03 exists per D-1.02).
//   - SupportsCredVend=false. Nessie does not vend STS subscoped
//     credentials — clients use direct AWS auth (or bearer token).
//   - SupportsWebhooks=false. Nessie does not publish webhook
//     events; Plan 01-07 L3 detection on Nessie deployments uses
//     polling + S3 ObjectCreated events instead.
//   - MaxNamespaceDepth=1. Nessie's REST API supports nested
//     namespaces but Phase 1 ingestion + the L1 gateway only
//     exercise single-level namespaces; raise this in Phase 2 if
//     hierarchical namespaces become necessary.
func (n *nessieAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name: "nessie",

		SupportsBranches: true,
		SupportsCredVend: false,
		SupportsWebhooks: false,

		MaxNamespaceDepth: 1,
	}
}

// translateError converts an iceberg-go-side error to one of the
// Phase 1 sentinels when the shape matches; otherwise wraps with
// the call-site-supplied prefix. Inspecting err.Error() for the
// "401" / "403" / "Unauthorized" markers is the only reliable
// shape detection because iceberg-go does not expose a typed auth
// error (the REST client embeds the upstream HTTP body verbatim).
//
// Identical to Polaris adapter's translateError — same upstream
// library, same error shapes, same translation rules. Kept as a
// per-adapter method (rather than a shared helper) so each adapter
// can grow per-catalog quirks without touching the other (e.g.,
// Phase 3 may need to recognize Nessie-specific reference-conflict
// shapes when commits to a stale branch HEAD trigger a Nessie-side
// rebase).
func (n *nessieAdapter) translateError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, icebergCatalog.ErrNoSuchTable) {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrTableNotFound)
	}
	msg := err.Error()
	if strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "Forbidden") {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrCredentialsExpired)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// isCommitConflict pattern-matches the upstream commit-conflict
// shape. Iceberg REST in general signals these as HTTP 409 with
// a body containing "CommitFailedException" or "commit conflict";
// iceberg-go surfaces both verbatim in the error message, so
// substring matches are the available shape. Nessie additionally
// surfaces "ReferenceConflictException" when a commit lands on a
// stale branch HEAD — included in the substring set.
func isCommitConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "409") ||
		strings.Contains(msg, "CommitFailedException") ||
		strings.Contains(msg, "commit conflict") ||
		strings.Contains(msg, "Conflict") ||
		strings.Contains(msg, "ReferenceConflictException")
}

// toIdentifier converts a Phase 1 TableRef to iceberg-go's flat
// table.Identifier ([]string of namespace components followed by
// the table name).
func toIdentifier(ref iceberg.TableRef) icebergTable.Identifier {
	out := make(icebergTable.Identifier, 0, len(ref.Namespace)+1)
	out = append(out, ref.Namespace...)
	out = append(out, ref.Name)
	return out
}

// convertTable maps iceberg-go's *table.Table to the Phase 1
// shared *iceberg.TableMetadata. Conservative — copies only the
// fields Phase 1 ingestion + the L1 gateway need; richer fields
// (sort orders, snapshot logs, statistics files) are deferred.
func convertTable(tbl *icebergTable.Table) *iceberg.TableMetadata {
	if tbl == nil {
		return nil
	}
	meta := tbl.Metadata()
	out := &iceberg.TableMetadata{
		MetadataLocation: tbl.MetadataLocation(),
	}
	if meta != nil {
		out.UUID = meta.TableUUID().String()
		out.Schema = convertSchema(meta.CurrentSchema())
		spec := meta.PartitionSpec()
		out.PartitionSpec = convertPartitionSpec(&spec)
		if curr := meta.CurrentSnapshot(); curr != nil {
			out.CurrentSnapshotID = curr.SnapshotID
		}
		out.Snapshots = convertSnapshots(meta.Snapshots(), out.MetadataLocation)
		out.Properties = map[string]string(meta.Properties())
	}
	return out
}

// convertSchema flattens iceberg-go's *iceberg.Schema (which uses
// the typed NestedField slice) into the string-typed Phase 1 shape.
// Iceberg type names are taken from the NestedField's String()
// representation — "long" / "string" / "decimal(10,2)" etc.
func convertSchema(sc *icebergGo.Schema) iceberg.Schema {
	if sc == nil {
		return iceberg.Schema{}
	}
	fields := sc.Fields()
	out := iceberg.Schema{Fields: make([]iceberg.SchemaField, 0, len(fields))}
	for _, f := range fields {
		typeName := ""
		if f.Type != nil {
			typeName = f.Type.String()
		}
		out.Fields = append(out.Fields, iceberg.SchemaField{
			ID:       f.ID,
			Name:     f.Name,
			Type:     typeName,
			Required: f.Required,
			Doc:      f.Doc,
		})
	}
	return out
}

// convertPartitionSpec maps iceberg-go's PartitionSpec to the
// Phase 1 shared shape. Transform names come from the upstream
// Transform.String() representation.
func convertPartitionSpec(ps *icebergGo.PartitionSpec) iceberg.PartitionSpec {
	if ps == nil {
		return iceberg.PartitionSpec{}
	}
	out := iceberg.PartitionSpec{SpecID: ps.ID()}
	for f := range ps.Fields() {
		transform := ""
		if f.Transform != nil {
			transform = f.Transform.String()
		}
		out.Fields = append(out.Fields, iceberg.PartitionField{
			SourceColumnID: f.SourceID,
			Transform:      transform,
			Name:           f.Name,
		})
	}
	return out
}

// convertSnapshots maps iceberg-go's []table.Snapshot to the Phase
// 1 shared []iceberg.Snapshot. Per D-1.04 each Snapshot's
// MetadataLocation is its per-snapshot natural key — but iceberg-go
// does not expose this on the Snapshot type itself (it's only on
// the parent table.Table). For Phase 1 we set every snapshot's
// MetadataLocation to the table's current metadataLocation as a
// placeholder; full per-snapshot URLs require a follow-up
// metadata.json read that ingestion (Plan 01-04) will own.
func convertSnapshots(in []icebergTable.Snapshot, fallbackMetaLoc string) []iceberg.Snapshot {
	out := make([]iceberg.Snapshot, 0, len(in))
	for _, s := range in {
		var parent int64
		if s.ParentSnapshotID != nil {
			parent = *s.ParentSnapshotID
		}
		var op string
		var summary map[string]string
		if s.Summary != nil {
			op = string(s.Summary.Operation)
			summary = map[string]string(s.Summary.Properties)
		}
		out = append(out, iceberg.Snapshot{
			SnapshotID:       s.SnapshotID,
			ParentSnapshotID: parent,
			TimestampMs:      s.TimestampMs,
			Operation:        op,
			Summary:          summary,
			MetadataLocation: fallbackMetaLoc,
			// Manifests left empty in Phase 1 — populated by
			// ingestion's manifest-reader pass (Plan 01-04).
		})
	}
	return out
}

// currentSnapshot is a small helper to safely extract the current
// snapshot from a possibly-nil iceberg-go Metadata.
func currentSnapshot(meta icebergTable.Metadata) *icebergTable.Snapshot {
	if meta == nil {
		return nil
	}
	return meta.CurrentSnapshot()
}

// convertRequirements + convertUpdates take the untyped Phase 1
// maps and translate them to iceberg-go's typed shapes for the
// wire call.
//
// Phase 1 scope: the L1 gateway hasn't shipped yet (Plan 01-06),
// so the only callers that hit nessieAdapter.CommitTable are:
//
//	(a) ExpireSnapshots above — bypasses convertRequirements /
//	    convertUpdates entirely by calling n.cat.CommitTable with
//	    directly-constructed iceberg-go updates.
//	(b) Future gateway code (Plan 01-06) — will own the
//	    sophisticated requirement / update conversion using
//	    iceberg-go's typed constructors directly, NOT via this
//	    map-based path.
//
// Until then, the adapter accepts only empty Requirements / Updates
// from CommitTable callers. Non-empty input is rejected with a
// clear error message pointing the caller at the right path,
// because iceberg-go v0.5 exposes ParseRequirement (a streaming
// JSON decoder) but no public ParseUpdate — so a generic untyped-map
// conversion would silently mis-handle Update kinds. Better to fail
// loud than to drop updates on the floor.
//
// Plan 01-06 will replace this stub with a typed dispatcher
// (switch on the "action" key, route to NewAddSnapshotUpdate /
// NewSetPropertiesUpdate / NewRemoveSnapshotsUpdate / etc.).
func convertRequirements(in []iceberg.TableRequirement) ([]icebergTable.Requirement, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("nessie: convert requirements: untyped map conversion not implemented in Phase 1 — Plan 01-06 will land typed dispatch (got %d requirements)", len(in))
}

func convertUpdates(in []iceberg.TableUpdate) ([]icebergTable.Update, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("nessie: convert updates: untyped map conversion not implemented in Phase 1 — Plan 01-06 will land typed dispatch (got %d updates)", len(in))
}
