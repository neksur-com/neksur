// glueAdapter — AWS Glue Iceberg REST IcebergCatalogClient implementation
// built on top of github.com/apache/iceberg-go's REST catalog client.
//
// 90% clone of internal/iceberg/polaris/adapter.go (per 03-PATTERNS §4 /
// 03-RESEARCH §Pattern 2) with these Glue-specific deltas:
//
//   - REST endpoint:  "https://glue.{region}.amazonaws.com/iceberg"
//   - No OAuth2 token exchange — SigV4 signing replaces OAuth2
//   - SigV4 service name: "glue"
//   - SigV4 region: cfg.Region
//   - Warehouse: cfg.CatalogID (Glue's catalog ID maps to Iceberg warehouse)
//   - Props: rest.sigv4-enabled=true, rest.signing-name=glue
//   - CredentialMode: "vended-credentials" default (Glue SupportsCredVend=true)
//
// Transport chain (inner to outer):
//
//	http.DefaultTransport.Clone() (or cfg.BaseTransportWrap result)
//	    ← sigv4Transport (SigV4 signing: service=glue, region=cfg.Region)
//	        ← iceberg-go.sessionTransport (via rest.WithCustomTransport)
//
// Error translation (verbatim from polaris/adapter.go lines 353-368):
//   - catalog.ErrNoSuchTable             → iceberg.ErrTableNotFound
//   - 401 / 403 in error message         → iceberg.ErrCredentialsExpired
//   - "CommitFailedException" / 409 / "conflict" → iceberg.ErrCommitConflict
//
// Capabilities: Name="glue", SupportsBranches=false, SupportsCredVend=true,
// SupportsWebhooks=false, MaxNamespaceDepth=2 (Glue's catalog.database.table
// is two levels through Iceberg REST: database + table — per plan must_haves).
//
// Pitfall 11: this adapter never logs query bodies or artifact bodies.
// Error returns wrap sentinels only.
//
// Pitfall 3 (Lake Formation interaction): AccessDeniedException shapes from
// Glue are logged at slog.Debug level only (logAccessDeniedException helper
// in sigv4_transport.go). Plan 03-15 runbook documents the Lake Formation
// troubleshooting path using this log key.
//
// B-3 contract (plan 03-04 must_haves §10): builder.go / forwarder.go's
// `case "glue"` arm now points at this package. This plan does NOT modify
// forwarder.go beyond swapping glue_stub for glue — the dispatch arm
// structure was pre-allocated by Plan 03-03.
package glue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	icebergGo "github.com/apache/iceberg-go"
	icebergCatalog "github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	icebergTable "github.com/apache/iceberg-go/table"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// authErrRE matches "401", "403", "Unauthorized", "Forbidden" at WORD
// BOUNDARIES so a body containing "4031" or "User account 4030 not found" is
// NOT misclassified as ErrCredentialsExpired (WR-12 carryover from polaris).
var authErrRE = regexp.MustCompile(`\b(?:401|403|Unauthorized|Forbidden)\b`)

// ErrLakeFormationDenied is the sentinel returned when the Glue response
// contains an AccessDeniedException that indicates a Lake Formation grant
// mismatch. Callers can use errors.Is(err, glue.ErrLakeFormationDenied)
// to detect the Lake Formation interaction path.
var ErrLakeFormationDenied = errors.New("glue: Lake Formation access denied")

// glueAdapter wraps iceberg-go's *rest.Catalog and translates between the
// Phase 1 IcebergCatalogClient surface and iceberg-go's lower-level types.
// The struct is unexported (the only public constructor is New); callers
// obtain an iceberg.IcebergCatalogClient interface, never a typed pointer.
type glueAdapter struct {
	cfg Config
	cat *rest.Catalog
}

// New constructs a Glue-flavored IcebergCatalogClient. Validates cfg, applies
// defaults, loads AWS credentials via config.LoadDefaultConfig, builds the
// iceberg-go REST catalog client with SigV4 transport, and wraps it in a
// glueAdapter.
//
// Returns an error if cfg.Validate() fails OR if AWS credential loading
// fails OR if iceberg-go cannot connect to the Glue endpoint. On success
// the returned client is safe for concurrent use — iceberg-go's REST
// catalog is designed to be shared across goroutines.
func New(ctx context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	// Load AWS credentials from the default credential chain
	// (env vars → ~/.aws/credentials → EC2 IMDS → ECS task role → etc.)
	// The STS-assumed role ARN in cfg.IAMRoleARN is informational only —
	// if the operator wants to assume a specific role, they should configure
	// AWS_ROLE_ARN + AWS_ROLE_SESSION_NAME via the standard AWS SDK env vars
	// BEFORE calling New. We do not assume roles inside the adapter (that
	// would require STS API calls and a separate credential provider chain).
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("glue: load aws config: %w", err)
	}

	// Glue Iceberg REST endpoint per plan must_haves §2.
	endpoint := "https://glue." + cfg.Region + ".amazonaws.com/iceberg"

	// Wire props for Glue Iceberg REST (03-RESEARCH §Pattern 2).
	// SigV4 is indicated via rest.sigv4-enabled + rest.signing-name props
	// (iceberg-go honors these for REST catalogs that support SigV4 natively).
	// We ALSO use WithCustomTransport below so SigV4 signing happens at the
	// transport layer — belt-and-suspenders approach matching the plan spec.
	props := icebergGo.Properties{
		"uri":                                endpoint,
		"warehouse":                          cfg.CatalogID,
		"rest.sigv4-enabled":                 "true",
		"rest.signing-name":                  "glue",
		"header.X-Iceberg-Access-Delegation": cfg.CredentialMode,
	}

	// Build the transport chain: sigv4Transport wraps the base transport.
	// Composition (inner to outer):
	//   BaseTransportWrap(http.DefaultTransport.Clone())
	//     ← sigv4Transport (SigV4 signing)
	//         ← iceberg-go sessionTransport (via WithCustomTransport)
	var baseTransport http.RoundTripper = http.DefaultTransport.(*http.Transport).Clone()
	if cfg.BaseTransportWrap != nil {
		baseTransport = cfg.BaseTransportWrap(baseTransport)
	}
	customTransport := newSigV4Transport(awsCfg.Credentials, cfg.Region, baseTransport)

	cat, err := rest.NewCatalog(ctx, "glue", endpoint,
		rest.WithWarehouseLocation(cfg.CatalogID),
		rest.WithAdditionalProps(props),
		rest.WithCustomTransport(customTransport),
	)
	if err != nil {
		return nil, fmt.Errorf("glue: new catalog: %w", err)
	}
	return &glueAdapter{cfg: cfg, cat: cat}, nil
}

// ListTables enumerates tables under namespace via the Glue Iceberg REST
// catalog. Glue's MaxNamespaceDepth=2 (database + table).
func (g *glueAdapter) ListTables(ctx context.Context, namespace string) ([]iceberg.TableRef, error) {
	ns := icebergTable.Identifier{namespace}
	out := make([]iceberg.TableRef, 0)
	for ident, err := range g.cat.ListTables(ctx, ns) {
		if err != nil {
			return nil, g.translateError(ctx, "glue: list tables", err)
		}
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

// GetTable is a lightweight LoadTable — Glue Iceberg REST has no separate
// HEAD-style endpoint, so the wire call is identical.
func (g *glueAdapter) GetTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return g.LoadTable(ctx, ref)
}

// LoadTable fetches the table's full metadata projection from Glue and
// converts iceberg-go's *table.Table to the Phase 1 shared *iceberg.TableMetadata
// shape. Returns iceberg.ErrTableNotFound (wrapped) when the upstream catalog
// reports the table doesn't exist; iceberg.ErrCredentialsExpired when SigV4
// credentials are rejected; and a generic wrapped error for other failures.
func (g *glueAdapter) LoadTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	ident := toIdentifier(ref)
	tbl, err := g.cat.LoadTable(ctx, ident)
	if err != nil {
		return nil, g.translateError(ctx, "glue: load table", err)
	}
	return convertTable(tbl), nil
}

// CommitTable forwards a CommitRequest to Glue via iceberg-go's CommitTable.
// Returns iceberg.ErrCommitConflict (wrapped) on 409 / CommitFailedException;
// iceberg.ErrTableNotFound when the table disappeared; and generic wrapped
// errors for transport / auth / parse failures.
func (g *glueAdapter) CommitTable(ctx context.Context, ref iceberg.TableRef, req iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	ident := toIdentifier(ref)

	requirements, err := convertRequirements(req.Requirements)
	if err != nil {
		return nil, fmt.Errorf("glue: commit table: convert requirements: %w", err)
	}
	updates, err := convertUpdates(req.Updates)
	if err != nil {
		return nil, fmt.Errorf("glue: commit table: convert updates: %w", err)
	}

	meta, newLoc, err := g.cat.CommitTable(ctx, ident, requirements, updates)
	if err != nil {
		if isCommitConflict(err) {
			return nil, fmt.Errorf("glue: commit table: %w", iceberg.ErrCommitConflict)
		}
		return nil, g.translateError(ctx, "glue: commit table", err)
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
// matching snapshot IDs.
func (g *glueAdapter) ExpireSnapshots(ctx context.Context, ref iceberg.TableRef, olderThan time.Time) error {
	tbl, err := g.cat.LoadTable(ctx, toIdentifier(ref))
	if err != nil {
		return g.translateError(ctx, "glue: expire snapshots: load", err)
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
	if _, _, err := g.cat.CommitTable(ctx, toIdentifier(ref), nil, updates); err != nil {
		if isCommitConflict(err) {
			return fmt.Errorf("glue: expire snapshots: %w", iceberg.ErrCommitConflict)
		}
		return g.translateError(ctx, "glue: expire snapshots", err)
	}
	return nil
}

// Capabilities returns the static facts the gateway / scheduler
// branches on. Per plan must_haves §4:
//   - Name="glue"
//   - SupportsBranches=false (Glue has no branching API)
//   - SupportsCredVend=true (Glue Iceberg REST supports STS credential vending)
//   - SupportsWebhooks=false (Glue uses CloudWatch Events, not Iceberg webhooks)
//   - MaxNamespaceDepth=2 (Glue's catalog.database.table flattens to 2 levels)
func (g *glueAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:              "glue",
		SupportsBranches:  false,
		SupportsCredVend:  true,
		SupportsWebhooks:  false,
		MaxNamespaceDepth: 2,
	}
}

// IssueScopedSTSCredentials implements the L4 credential vending path for
// Glue. Returns iceberg.ErrAdapterStub in Phase 3 — Glue STS vending
// differs from Polaris (different token format, different API) and is
// deferred to a follow-on plan.
//
// SupportsCredVend=true is set in Capabilities because Glue's Iceberg REST
// endpoint supports credential vending in principle; the stub here defers
// the Neksur-side implementation.
func (g *glueAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	return nil, fmt.Errorf("glue: IssueScopedSTSCredentials: %w", iceberg.ErrAdapterStub)
}

// translateError converts an iceberg-go-side error to one of the
// Phase 1 sentinels when the shape matches; otherwise wraps with
// the call-site-supplied prefix.
//
// Pitfall 3 (Lake Formation interaction): AccessDeniedException shapes
// are logged at slog.Debug level only before being mapped to
// ErrCredentialsExpired OR ErrLakeFormationDenied.
func (g *glueAdapter) translateError(ctx context.Context, prefix string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, icebergCatalog.ErrNoSuchTable) {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrTableNotFound)
	}
	// WR-12 + Pitfall 3: word-boundary regex guards against misclassification.
	// Log the AccessDeniedException shape at Debug for LF troubleshooting.
	errMsg := err.Error()
	logAccessDeniedException(ctx, prefix, err)
	if authErrRE.MatchString(errMsg) {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrCredentialsExpired)
	}
	// Specific Lake Formation AccessDeniedException that didn't match the
	// standard auth error regex — return ErrLakeFormationDenied sentinel.
	if contains(errMsg, "AccessDeniedException") {
		return fmt.Errorf("%s: %w", prefix, ErrLakeFormationDenied)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// isCommitConflict pattern-matches the upstream commit-conflict shape.
func isCommitConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "409") ||
		strings.Contains(msg, "CommitFailedException") ||
		strings.Contains(msg, "commit conflict") ||
		strings.Contains(msg, "Conflict")
}

// toIdentifier converts a Phase 1 TableRef to iceberg-go's flat
// table.Identifier ([]string of namespace components followed by the table name).
func toIdentifier(ref iceberg.TableRef) icebergTable.Identifier {
	out := make(icebergTable.Identifier, 0, len(ref.Namespace)+1)
	out = append(out, ref.Namespace...)
	out = append(out, ref.Name)
	return out
}

// convertTable maps iceberg-go's *table.Table to the Phase 1
// shared *iceberg.TableMetadata. Conservative — copies only the
// fields Phase 1 ingestion + the L1 gateway need.
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

// convertSchema flattens iceberg-go's *iceberg.Schema into the string-typed Phase 1 shape.
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

// convertPartitionSpec maps iceberg-go's PartitionSpec to the Phase 1 shared shape.
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

// convertSnapshots maps iceberg-go's []table.Snapshot to the Phase 1 shared []iceberg.Snapshot.
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
		})
	}
	return out
}

// currentSnapshot is a small helper to safely extract the current snapshot.
func currentSnapshot(meta icebergTable.Metadata) *icebergTable.Snapshot {
	if meta == nil {
		return nil
	}
	return meta.CurrentSnapshot()
}

// convertRequirements + convertUpdates take the untyped Phase 1
// maps and translate them to iceberg-go's typed shapes.
// Same Phase 1 scope as polaris — only empty slices supported; non-empty
// rejected with a clear error pointing to the future Phase dispatch.
func convertRequirements(in []iceberg.TableRequirement) ([]icebergTable.Requirement, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("glue: convert requirements: untyped map conversion not implemented in Phase 3 — Plan 01-06 typed dispatch required (got %d requirements)", len(in))
}

func convertUpdates(in []iceberg.TableUpdate) ([]icebergTable.Update, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("glue: convert updates: untyped map conversion not implemented in Phase 3 — Plan 01-06 typed dispatch required (got %d updates)", len(in))
}
