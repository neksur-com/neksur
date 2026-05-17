// unityAdapter — Databricks Unity Catalog IcebergCatalogClient implementation
// built on top of github.com/apache/iceberg-go's REST catalog client.
//
// 90% clone of internal/iceberg/polaris/adapter.go (per 03-PATTERNS §1) with
// these Unity-specific deltas:
//
//   - REST endpoint:  WorkspaceHost + "/api/2.1/unity-catalog/iceberg"
//   - OAuth token:    WorkspaceHost + "/oidc/v1/token"
//   - OAuth scope:    "all-apis"
//   - credential:     OAuthClientID + ":" + OAuthClientSecret
//   - Header:         X-Databricks-Workspace-Id = WorkspaceID (via transport)
//   - Warehouse:      CatalogName (Unity's catalog name maps to Iceberg warehouse)
//   - CredentialMode: "vended-credentials" default (Unity STS vending enabled)
//
// Transport chain (inner to outer):
//
//   http.DefaultTransport.Clone() (or cfg.BaseTransportWrap result)
//     ← databricksContextTransport   (X-Databricks-Workspace-Id header injection)
//         ← refreshOn401Transport    (Pitfall 2: retry once on 401)
//             ← iceberg-go.sessionTransport (via rest.WithCustomTransport)
//
// Error translation (verbatim from polaris/adapter.go lines 353-368):
//   - catalog.ErrNoSuchTable             → iceberg.ErrTableNotFound
//   - 401 / 403 in error message         → iceberg.ErrCredentialsExpired
//   - "CommitFailedException" / 409 / "conflict" → iceberg.ErrCommitConflict
//
// Capabilities: Name="unity", SupportsBranches=false, SupportsCredVend=true,
// SupportsWebhooks=true, MaxNamespaceDepth=1 (Unity's three-level
// catalog.schema.table is exposed flat through Iceberg REST as a single
// namespace segment per request — 03-PATTERNS §14).
//
// Pitfall 11: this adapter never logs query bodies or artifact bodies.
// Error returns wrap sentinels only.
package unity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	icebergGo "github.com/apache/iceberg-go"
	icebergCatalog "github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	icebergTable "github.com/apache/iceberg-go/table"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// authErrRE matches "401", "403", "Unauthorized", "Forbidden" at WORD
// BOUNDARIES so a body containing "4031" or "User account 4030 not found" is
// NOT misclassified as ErrCredentialsExpired (WR-12 carryover from polaris).
var authErrRE = regexp.MustCompile(`\b(?:401|403|Unauthorized|Forbidden)\b`)

// unityAdapter wraps iceberg-go's *rest.Catalog and translates between the
// Phase 1 IcebergCatalogClient surface and iceberg-go's lower-level types.
// The struct is unexported (the only public constructor is New); callers
// obtain an iceberg.IcebergCatalogClient interface, never a typed pointer.
type unityAdapter struct {
	cfg Config
	cat *rest.Catalog
}

// New constructs a Unity-flavored IcebergCatalogClient. Validates cfg, applies
// defaults, builds the iceberg-go REST catalog client with Databricks OAuth
// client-credentials wiring (Pitfall 2 transport chain), and wraps it in a
// unityAdapter.
//
// Returns an error if cfg.Validate() fails OR if iceberg-go cannot connect to
// the Unity endpoint. On success the returned client is safe for concurrent
// use — iceberg-go's REST catalog is designed to be shared across goroutines.
func New(ctx context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	// Unity-specific wire props (per 03-PATTERNS §1 Unity-specific deltas):
	//   - uri:                 Unity Iceberg REST root
	//   - warehouse:           Unity catalog name
	//   - oauth2-server-uri:   Databricks OIDC token endpoint
	//   - scope:               Databricks all-apis scope
	//   - header.X-Iceberg-Access-Delegation: vended-credentials mode
	//   - header.X-Databricks-Workspace-Id:   injected per request via transport
	//
	// WR-07: OAuthClientSecret is passed ONLY via rest.WithCredential —
	// NOT through the props map. WithAdditionalProps stores the map verbatim
	// and may surface it in debug logs; keeping the secret behind the typed
	// option confines its lifetime to the OAuth token exchange.
	credential := cfg.OAuthClientID + ":" + cfg.OAuthClientSecret
	icebergURI := cfg.WorkspaceHost + "/api/2.1/unity-catalog/iceberg"
	oauthURI := cfg.WorkspaceHost + "/oidc/v1/token"

	props := icebergGo.Properties{
		"uri":                                icebergURI,
		"warehouse":                          cfg.CatalogName,
		"oauth2-server-uri":                  oauthURI,
		"scope":                              "all-apis",
		"header.X-Iceberg-Access-Delegation": cfg.CredentialMode,
		// X-Databricks-Workspace-Id is injected per-request by
		// databricksContextTransport rather than as a static prop — this
		// matches the T-3-unity-workspace-spoof mitigation (operator config
		// drives the header, not the static props map that clients could
		// potentially inspect).
	}

	authURI, err := url.Parse(oauthURI)
	if err != nil {
		return nil, fmt.Errorf("unity: parse oauth2-server-uri: %w", err)
	}

	// Transport chain composition (inner to outer):
	//   BaseTransportWrap(http.DefaultTransport.Clone())
	//     ← databricksContextTransport (X-Databricks-Workspace-Id)
	//         ← refreshOn401Transport  (Pitfall 2 single-retry)
	//             ← iceberg-go sessionTransport (injected via WithCustomTransport)
	var baseTransport http.RoundTripper = http.DefaultTransport.(*http.Transport).Clone()
	if cfg.BaseTransportWrap != nil {
		baseTransport = cfg.BaseTransportWrap(baseTransport)
	}
	dbTransport := &databricksContextTransport{
		next:        baseTransport,
		workspaceID: cfg.WorkspaceID,
	}
	customTransport := &refreshOn401Transport{next: dbTransport}

	cat, err := rest.NewCatalog(ctx, "unity", icebergURI,
		rest.WithCredential(credential),
		rest.WithAuthURI(authURI),
		rest.WithScope("all-apis"),
		rest.WithWarehouseLocation(cfg.CatalogName),
		rest.WithAdditionalProps(props),
		rest.WithCustomTransport(customTransport),
	)
	if err != nil {
		return nil, fmt.Errorf("unity: new catalog: %w", err)
	}
	return &unityAdapter{cfg: cfg, cat: cat}, nil
}

// ListTables enumerates tables under namespace via the Unity Iceberg REST
// catalog. Unity uses a flat namespace model (MaxNamespaceDepth=1).
func (u *unityAdapter) ListTables(ctx context.Context, namespace string) ([]iceberg.TableRef, error) {
	ns := icebergTable.Identifier{namespace}
	out := make([]iceberg.TableRef, 0)
	for ident, err := range u.cat.ListTables(ctx, ns) {
		if err != nil {
			return nil, u.translateError("unity: list tables", err)
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

// GetTable is a lightweight LoadTable — Unity Iceberg REST has no separate
// HEAD-style endpoint, so the wire call is identical to LoadTable.
func (u *unityAdapter) GetTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return u.LoadTable(ctx, ref)
}

// LoadTable fetches the table's full metadata projection from Unity and
// converts iceberg-go's *table.Table to the Phase 1 shared *iceberg.TableMetadata
// shape. Returns iceberg.ErrTableNotFound (wrapped) when the upstream catalog
// reports the table doesn't exist; iceberg.ErrCredentialsExpired when the OAuth
// bearer is rejected; and a generic wrapped error for other failures.
func (u *unityAdapter) LoadTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	ident := toIdentifier(ref)
	tbl, err := u.cat.LoadTable(ctx, ident)
	if err != nil {
		return nil, u.translateError("unity: load table", err)
	}
	return convertTable(tbl), nil
}

// CommitTable forwards a CommitRequest to Unity via iceberg-go's CommitTable.
// Returns iceberg.ErrCommitConflict (wrapped) on 409 / CommitFailedException;
// iceberg.ErrTableNotFound when the table disappeared between LoadTable and
// CommitTable; and generic wrapped errors for transport / auth / parse
// failures.
func (u *unityAdapter) CommitTable(ctx context.Context, ref iceberg.TableRef, req iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	ident := toIdentifier(ref)

	requirements, err := convertRequirements(req.Requirements)
	if err != nil {
		return nil, fmt.Errorf("unity: commit table: convert requirements: %w", err)
	}
	updates, err := convertUpdates(req.Updates)
	if err != nil {
		return nil, fmt.Errorf("unity: commit table: convert updates: %w", err)
	}

	meta, newLoc, err := u.cat.CommitTable(ctx, ident, requirements, updates)
	if err != nil {
		if isCommitConflict(err) {
			return nil, fmt.Errorf("unity: commit table: %w", iceberg.ErrCommitConflict)
		}
		return nil, u.translateError("unity: commit table", err)
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

// ExpireSnapshots removes snapshots committed before olderThan via a
// CommitTable that issues a remove-snapshots Update for the matching snapshot
// IDs.
func (u *unityAdapter) ExpireSnapshots(ctx context.Context, ref iceberg.TableRef, olderThan time.Time) error {
	tbl, err := u.cat.LoadTable(ctx, toIdentifier(ref))
	if err != nil {
		return u.translateError("unity: expire snapshots: load", err)
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
	if _, _, err := u.cat.CommitTable(ctx, toIdentifier(ref), nil, updates); err != nil {
		if isCommitConflict(err) {
			return fmt.Errorf("unity: expire snapshots: %w", iceberg.ErrCommitConflict)
		}
		return u.translateError("unity: expire snapshots", err)
	}
	return nil
}

// Capabilities returns the static facts the gateway / scheduler branches on.
// Per 03-PATTERNS §14 / Unity flat namespace constraint:
//   - Name:              "unity"
//   - SupportsBranches:  false (Unity does not expose Iceberg branches)
//   - SupportsCredVend:  true  (Unity STS token vending per Phase 3 D-3.02)
//   - SupportsWebhooks:  true  (Unity emits Iceberg events via Databricks subscriptions)
//   - MaxNamespaceDepth: 1     (Unity's three-level catalog.schema.table is flat through REST)
func (u *unityAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:              "unity",
		SupportsBranches:  false,
		SupportsCredVend:  true,
		SupportsWebhooks:  true,
		MaxNamespaceDepth: 1,
	}
}

// IssueScopedSTSCredentials is not yet wired for Unity in Phase 3 — Unity's
// STS vending shape differs from Polaris (different token format + Databricks
// credential-vending API). Returns ErrAdapterStub as a placeholder until the
// Unity STS path is implemented.
//
// Phase 3 plans beyond 03-03 will implement the live Unity STS path.
// The CR-03 boot-time guard has been removed (Plan 03-03), so tenants with
// kind=unity will receive this error at runtime rather than at boot.
func (u *unityAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	return nil, fmt.Errorf("unity: IssueScopedSTSCredentials: %w", iceberg.ErrAdapterStub)
}

// translateError converts an iceberg-go-side error to one of the Phase 1
// sentinels when the shape matches; otherwise wraps with the call-site-
// supplied prefix. Verbatim from polaris/adapter.go lines 353-368.
//
// Per Pitfall 11: this method never logs the error body — it wraps sentinels
// only.
func (u *unityAdapter) translateError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, icebergCatalog.ErrNoSuchTable) {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrTableNotFound)
	}
	// WR-12: word-boundary regex so a 404 body like "User account 4030 not
	// found" or "code:4031" is not misclassified as ErrCredentialsExpired.
	if authErrRE.MatchString(err.Error()) {
		return fmt.Errorf("%s: %w", prefix, iceberg.ErrCredentialsExpired)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// isCommitConflict pattern-matches the upstream commit-conflict shape.
// Unity signals these as HTTP 409 with "CommitFailedException" or "conflict".
// Verbatim shape from polaris/adapter.go.
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
// table.Identifier ([]string of namespace components followed by table name).
func toIdentifier(ref iceberg.TableRef) icebergTable.Identifier {
	out := make(icebergTable.Identifier, 0, len(ref.Namespace)+1)
	out = append(out, ref.Namespace...)
	out = append(out, ref.Name)
	return out
}

// convertTable maps iceberg-go's *table.Table to the Phase 1 shared
// *iceberg.TableMetadata. Conservative — copies only the fields Phase 1
// ingestion + the L1 gateway need.
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

// convertSchema flattens iceberg-go's *iceberg.Schema into the string-typed
// Phase 1 shape.
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

// convertPartitionSpec maps iceberg-go's PartitionSpec to the Phase 1 shape.
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

// convertSnapshots maps iceberg-go's []table.Snapshot to the Phase 1 shared
// []iceberg.Snapshot.
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

// currentSnapshot safely extracts the current snapshot from a possibly-nil
// iceberg-go Metadata.
func currentSnapshot(meta icebergTable.Metadata) *icebergTable.Snapshot {
	if meta == nil {
		return nil
	}
	return meta.CurrentSnapshot()
}

// convertRequirements + convertUpdates take the untyped Phase 1 maps and
// translate them to iceberg-go's typed shapes. Phase 1 scope: only empty
// slices are accepted; non-empty input is rejected with a clear error. Same
// pattern as polaris/adapter.go.
func convertRequirements(in []iceberg.TableRequirement) ([]icebergTable.Requirement, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("unity: convert requirements: untyped map conversion not implemented in Phase 1 (got %d requirements)", len(in))
}

func convertUpdates(in []iceberg.TableUpdate) ([]icebergTable.Update, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("unity: convert updates: untyped map conversion not implemented in Phase 1 (got %d updates)", len(in))
}
