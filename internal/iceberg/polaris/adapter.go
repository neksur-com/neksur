// polarisAdapter — Apache Polaris-flavored IcebergCatalogClient
// implementation built on top of github.com/apache/iceberg-go's REST
// catalog client.
//
// Wire layer:
//   - OAuth client-credentials grant via iceberg-go's REST catalog
//     OAuth machinery (Pitfall 1 — automatic token refresh). The
//     grep-detectable string keys "oauth2-server-uri" + "credential"
//     in the props map below are the wire keys Iceberg REST OpenAPI
//     reserves for this grant; iceberg-go reads them, exchanges
//     the credential at oauth2-server-uri, and refreshes
//     automatically before each request when the cached token is
//     within ~30s of expiry. The redundant typed options
//     (WithCredential / WithScope / WithAuthURI) belt-and-suspender
//     the same wire shape — iceberg-go v0.5+ accepts both forms,
//     and exposing the keys in source makes Pitfall 1 mitigation
//     visibly auditable in code review (RESEARCH lines 1565-1575
//     verbatim shape).
//
// Error translation:
//   - catalog.ErrNoSuchTable             → iceberg.ErrTableNotFound
//   - 401 / 403 in error message         → iceberg.ErrCredentialsExpired
//   - "CommitFailedException" / 409 / "conflict" → iceberg.ErrCommitConflict
//
// All wrapping uses fmt.Errorf with `%w` so errors.Is keeps working
// up the call chain.
package polaris

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	icebergGo "github.com/apache/iceberg-go"
	icebergCatalog "github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	icebergTable "github.com/apache/iceberg-go/table"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// polarisAdapter wraps iceberg-go's *rest.Catalog and translates
// between the Phase 1 IcebergCatalogClient surface and iceberg-go's
// lower-level types. The struct is unexported (the only public
// constructor is New); callers obtain an iceberg.IcebergCatalogClient
// interface, never a typed pointer to this struct.
type polarisAdapter struct {
	cfg Config
	cat *rest.Catalog
}

// New constructs a Polaris-flavored IcebergCatalogClient. Validates
// cfg, applies defaults, builds the iceberg-go REST catalog client
// with OAuth client-credentials wiring (Pitfall 1), and wraps it in
// a polarisAdapter.
//
// Returns an error if cfg.Validate() fails OR if iceberg-go cannot
// connect to the Polaris endpoint (e.g., DNS failure, unreachable
// network, malformed URL). On success the returned client is
// safe to call concurrently — iceberg-go's REST catalog is designed
// to be shared across goroutines.
func New(ctx context.Context, cfg Config) (iceberg.IcebergCatalogClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	// Wire props per RESEARCH lines 1565-1575. The four string keys
	// below are the documented Iceberg REST OAuth + delegation
	// header surface; iceberg-go honors them automatically and the
	// "oauth2-server-uri" + "credential" pair is what enables
	// Pitfall 1 token refresh. We pass them via WithAdditionalProps
	// (iceberg-go v0.5+ idiom) so the same wire shape works whether
	// the caller is on v0.5.x or a future v0.6+. The redundant
	// typed options below (WithCredential / WithScope / WithAuthURI
	// / WithWarehouseLocation) belt-and-suspender the same wire
	// shape — iceberg-go accepts both forms and applies the typed
	// options first. The grep-detectable string-key form below is
	// the wire shape Pitfall 1 mitigation requires; review code
	// changes against this map.
	// OAuth token endpoint is `<endpoint>/v1/oauth/tokens` —
	// matches both iceberg-go's URL convention (which appends
	// `/v1/...` to the catalog ROOT) and Polaris 1.4.0's actual
	// routing (the catalog ROOT is `/api/catalog`, so OAuth lives
	// at `/api/catalog/v1/oauth/tokens`; the legacy `/api/v1/...`
	// path returns 404 — confirmed by direct curl probe during
	// Plan 01-02 Task 2; see SUMMARY for the deviation note).
	// Production callers configure `cfg.Endpoint` to their Polaris
	// catalog ROOT (e.g., `https://polaris.customer.com/api/catalog`).
	// WR-07: the `credential` (containing the OAuth ClientSecret)
	// is passed ONLY via rest.WithCredential — NOT through the
	// `props` map below. iceberg-go's WithAdditionalProps stores the
	// map verbatim on the catalog, and depending on the version it
	// may surface props in debug logs / String() output. Keeping the
	// secret behind the typed option (which the upstream lib treats
	// as opaque credential material) confines its lifetime to the
	// OAuth token exchange.
	credential := cfg.ClientID + ":" + cfg.ClientSecret
	props := icebergGo.Properties{
		"uri":                                cfg.Endpoint,
		"warehouse":                          cfg.Warehouse,
		"oauth2-server-uri":                  cfg.Endpoint + "/v1/oauth/tokens",
		"scope":                              cfg.Scope,
		"header.X-Iceberg-Access-Delegation": cfg.CredentialMode,
	}

	authURI, err := url.Parse(props["oauth2-server-uri"])
	if err != nil {
		return nil, fmt.Errorf("polaris: parse oauth2-server-uri: %w", err)
	}

	cat, err := rest.NewCatalog(ctx, "polaris", cfg.Endpoint,
		rest.WithCredential(credential),
		rest.WithAuthURI(authURI),
		rest.WithScope(cfg.Scope),
		rest.WithWarehouseLocation(cfg.Warehouse),
		rest.WithAdditionalProps(props),
	)
	if err != nil {
		return nil, fmt.Errorf("polaris: new catalog: %w", err)
	}
	return &polarisAdapter{cfg: cfg, cat: cat}, nil
}

// ListTables enumerates tables under namespace via the Polaris REST
// catalog. Phase 1 callers pass a single-segment namespace string;
// the adapter converts it to iceberg-go's table.Identifier
// (`[]string{namespace}`) at the wire boundary.
func (p *polarisAdapter) ListTables(ctx context.Context, namespace string) ([]iceberg.TableRef, error) {
	ns := icebergTable.Identifier{namespace}
	out := make([]iceberg.TableRef, 0)
	for ident, err := range p.cat.ListTables(ctx, ns) {
		if err != nil {
			return nil, p.translateError("polaris: list tables", err)
		}
		// iceberg-go returns identifiers as []string where the
		// last component is the table name and everything prior
		// is the namespace. Phase 1 single-level namespaces mean
		// len == 2 typically, but be defensive about deeper paths.
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
func (p *polarisAdapter) GetTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	return p.LoadTable(ctx, ref)
}

// LoadTable fetches the table's full metadata projection from
// Polaris and converts iceberg-go's *table.Table to the Phase 1
// shared *iceberg.TableMetadata shape. Returns iceberg.ErrTableNotFound
// (wrapped) when the upstream catalog reports the table doesn't
// exist; iceberg.ErrCredentialsExpired when the OAuth bearer is
// rejected; and a generic wrapped error for other failures.
func (p *polarisAdapter) LoadTable(ctx context.Context, ref iceberg.TableRef) (*iceberg.TableMetadata, error) {
	ident := toIdentifier(ref)
	tbl, err := p.cat.LoadTable(ctx, ident)
	if err != nil {
		return nil, p.translateError("polaris: load table", err)
	}
	return convertTable(tbl), nil
}

// CommitTable forwards a CommitRequest to Polaris via iceberg-go's
// CommitTable. Translates the untyped TableRequirement / TableUpdate
// maps to iceberg-go's typed Requirement / Update slices via JSON
// round-trip — Phase 1 expediency; Phase 2 may refactor to direct
// constructors when the policy gateway needs richer access to
// individual update fields.
//
// Returns iceberg.ErrCommitConflict (wrapped) when the upstream
// catalog reports the table state changed under us (409 /
// CommitFailedException); iceberg.ErrTableNotFound when the table
// disappeared between LoadTable and CommitTable; and generic
// wrapped errors for transport / auth / parse failures.
func (p *polarisAdapter) CommitTable(ctx context.Context, ref iceberg.TableRef, req iceberg.CommitRequest) (*iceberg.CommitResult, error) {
	ident := toIdentifier(ref)

	requirements, err := convertRequirements(req.Requirements)
	if err != nil {
		return nil, fmt.Errorf("polaris: commit table: convert requirements: %w", err)
	}
	updates, err := convertUpdates(req.Updates)
	if err != nil {
		return nil, fmt.Errorf("polaris: commit table: convert updates: %w", err)
	}

	meta, newLoc, err := p.cat.CommitTable(ctx, ident, requirements, updates)
	if err != nil {
		// Translate commit-conflict shape — Polaris signals a
		// rebase-required scenario as 409 / CommitFailedException;
		// callers reload the table and reapply.
		if isCommitConflict(err) {
			return nil, fmt.Errorf("polaris: commit table: %w", iceberg.ErrCommitConflict)
		}
		return nil, p.translateError("polaris: commit table", err)
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
func (p *polarisAdapter) ExpireSnapshots(ctx context.Context, ref iceberg.TableRef, olderThan time.Time) error {
	tbl, err := p.cat.LoadTable(ctx, toIdentifier(ref))
	if err != nil {
		return p.translateError("polaris: expire snapshots: load", err)
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
	if _, _, err := p.cat.CommitTable(ctx, toIdentifier(ref), nil, updates); err != nil {
		if isCommitConflict(err) {
			return fmt.Errorf("polaris: expire snapshots: %w", iceberg.ErrCommitConflict)
		}
		return p.translateError("polaris: expire snapshots", err)
	}
	return nil
}

// Capabilities returns the static facts the gateway / scheduler
// branches on. Numbers per RESEARCH lines 583-588: Polaris
// supports STS credential vending (Phase 2 L4 will use it),
// publishes Iceberg REST events / webhooks, and accepts up to 100
// namespace path components. SupportsBranches=false (Polaris does
// not expose branching as a first-class API — Nessie is the
// branching catalog Plan 01-03 ships).
func (p *polarisAdapter) Capabilities() iceberg.Capabilities {
	return iceberg.Capabilities{
		Name:              "polaris",
		SupportsBranches:  false,
		SupportsCredVend:  true,
		SupportsWebhooks:  true,
		MaxNamespaceDepth: 100,
	}
}

// IssueScopedSTSCredentials implements the L4 credential vending path
// per D-2.09 + RESEARCH §Code Example 6 lines 1025-1047 + PATTERNS
// lines 657-694.
//
// CR-02 (Phase 2): the original implementation attempted to propagate
// the X-Iceberg-Access-Delegation + X-Iceberg-Session-Policy headers
// to iceberg-go's REST catalog via a *file-local* context key
// (restHeadersKey{}). iceberg-go v0.5.0 reads headers from its own
// package-private context key — there is no documented public bridge
// between the two. As a result, the original LoadTable call ran
// WITHOUT the delegation headers, Polaris returned a normal config
// block without STS keys, and parseVendedCreds fail-closed with
// "missing required key 's3.access-key-id'". The failure mode masked
// the broken context-header assumption (Assumption A1 in RESEARCH).
//
// Until the correct iceberg-go v0.5+ per-request-header API
// (rest.WithHeader / LoadTableOption variadic) is wired through the
// adapter — OR the documented fallback (direct AWS STS AssumeRole)
// lands — this method hard-fails with iceberg.ErrAdapterStub so the
// credvend handler 503s loudly rather than appearing to "work" with
// all credentials missing.
//
// Tracked for Plan 02-08 contingency (see RESEARCH §Alternatives
// Considered line 153 for the direct-STS path).
func (p *polarisAdapter) IssueScopedSTSCredentials(_ context.Context, _ iceberg.TableRef, _ string) (*iceberg.STSCredentials, error) {
	return nil, fmt.Errorf(
		"polaris: vended-credentials: context-header propagation not verified " +
			"in iceberg-go v0.5.0 — wire rest.WithHeader / LoadTableOption (or " +
			"the direct AWS STS AssumeRole fallback) before enabling L4 vending: %w",
		iceberg.ErrAdapterStub,
	)
}

// issueScopedSTSCredentialsViaHeaderCtx is the original Phase 2
// implementation, retained for reference while the iceberg-go header
// API is verified. It is NOT wired into the IcebergCatalogClient
// interface (the public IssueScopedSTSCredentials above hard-fails).
//
// Steps (when re-enabled):
//  1. Build inline session policy via buildSessionPolicy.
//  2. Set X-Iceberg-Access-Delegation: vended-credentials.
//  3. Set X-Iceberg-Session-Policy carrying the JSON policy.
//  4. Call LoadTable with the per-request header API (NOT the
//     file-local context-key shim — that's the CR-02 bug).
//  5. parseVendedCreds on tbl.Properties() (Polaris 1.4 #11118 keys).
//
//nolint:unused // retained for the Plan 02-08 re-enable path.
func (p *polarisAdapter) issueScopedSTSCredentialsViaHeaderCtx(ctx context.Context, table iceberg.TableRef, region string) (*iceberg.STSCredentials, error) {
	sessionPolicyJSON, err := buildSessionPolicy(table, region, p.cfg.Warehouse)
	if err != nil {
		return nil, fmt.Errorf("polaris: vended-credentials: build session policy: %w", err)
	}

	delegationCtx := withDelegationHeaders(ctx, string(sessionPolicyJSON))

	ident := toIdentifier(table)
	tbl, err := p.cat.LoadTable(delegationCtx, ident)
	if err != nil {
		return nil, fmt.Errorf("polaris: vended-credentials: load table: %w", p.translateError("polaris: vended-credentials", err))
	}

	configMap := map[string]string(tbl.Properties())
	creds, err := parseVendedCreds(configMap)
	if err != nil {
		return nil, fmt.Errorf("polaris: vended-credentials: parse creds: %w", err)
	}
	creds.Region = region
	return creds, nil
}

// withDelegationHeaders returns a context that will cause iceberg-go's
// REST catalog client to emit the vended-credentials delegation headers.
// iceberg-go v0.5.0 reads these from context key "rest.headers" and
// merges them into every outbound request header map.
//
// If iceberg-go does not honor the context-header mechanism (Assumption
// A1), the fallback is to call AWS STS AssumeRole directly from Neksur
// with an inline session policy (documented alternative in RESEARCH
// §Alternatives Considered line 153). Phase 2 ships the context-header
// path; the fallback is a Plan 02-08 contingency.
func withDelegationHeaders(ctx context.Context, sessionPolicyJSON string) context.Context {
	type restHeadersKey struct{}
	existing, _ := ctx.Value(restHeadersKey{}).(map[string]string)
	merged := make(map[string]string, len(existing)+2)
	for k, v := range existing {
		merged[k] = v
	}
	merged["X-Iceberg-Access-Delegation"] = "vended-credentials"
	merged["X-Iceberg-Session-Policy"] = sessionPolicyJSON
	return context.WithValue(ctx, restHeadersKey{}, merged)
}

// buildSessionPolicy constructs the JSON inline session policy for the
// STS AssumeRole call per D-2.09 + RESEARCH §Code Example 6 lines
// 1051-1066.
//
// CRITICAL — Pitfall 1 + rustfs#1337: Resource MUST be a JSON array
// ([]string) even with a single element. AWS IAM returns an opaque
// InternalError 500 when Resource is a bare string — the error message
// does not hint at the root cause. The integration test
// TestCredvend_SessionPolicy_ResourceIsArray asserts this invariant
// by decoding the JSON and checking reflect.TypeOf(Resource).Kind() ==
// reflect.Slice.
func buildSessionPolicy(table iceberg.TableRef, region, warehouse string) ([]byte, error) {
	// Derive the S3 ARN resource from the warehouse path and table ref.
	// warehouse is typically "s3://bucket/prefix" — we extract the
	// bucket and build the per-table path.
	bucket := extractBucket(warehouse)
	tablePrefix := tableS3Prefix(table)
	resource := fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, tablePrefix)

	policy := sessionPolicyDoc{
		Version: "2012-10-17",
		Statement: []sessionPolicyStatement{{
			Effect: "Allow",
			// s3:PutObject only — least privilege (NOT s3:*).
			Action: "s3:PutObject",
			// MUST be []string (JSON array) — Pitfall 1.
			Resource: []string{resource},
			Condition: map[string]map[string]string{
				"StringEquals": {
					"aws:RequestedRegion": region,
				},
			},
		}},
	}

	data, err := marshalSessionPolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal session policy: %w", err)
	}
	return data, nil
}

// extractBucket extracts the S3 bucket name from a warehouse URI.
// Supports "s3://bucket/prefix" and bare "bucket/prefix" forms.
func extractBucket(warehouse string) string {
	s := warehouse
	if after, ok := strings.CutPrefix(s, "s3://"); ok {
		s = after
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// tableS3Prefix derives the S3 key prefix for a table from its TableRef.
// Joins namespace components and table name with "/".
func tableS3Prefix(ref iceberg.TableRef) string {
	parts := make([]string, 0, len(ref.Namespace)+1)
	parts = append(parts, ref.Namespace...)
	parts = append(parts, ref.Name)
	return strings.Join(parts, "/")
}

// sessionPolicyDoc is the JSON shape for an AWS inline session policy.
// Using explicit structs (not map[string]any) ensures the JSON array
// invariant for Resource is enforced by Go's type system rather than
// by convention.
type sessionPolicyDoc struct {
	Version   string                 `json:"Version"`
	Statement []sessionPolicyStatement `json:"Statement"`
}

type sessionPolicyStatement struct {
	Effect    string                       `json:"Effect"`
	Action    string                       `json:"Action"`
	Resource  []string                     `json:"Resource"` // MUST be []string — Pitfall 1
	Condition map[string]map[string]string `json:"Condition"`
}

// marshalSessionPolicy serialises the session policy document to JSON.
// Kept as a thin wrapper so sts.go tests can import just the shape check
// without depending on the full adapter.
func marshalSessionPolicy(doc sessionPolicyDoc) ([]byte, error) {
	return json.Marshal(doc)
}

// translateError converts an iceberg-go-side error to one of the
// Phase 1 sentinels when the shape matches; otherwise wraps with
// the call-site-supplied prefix. Inspecting err.Error() for the
// "401" / "403" / "Unauthorized" markers is the only reliable
// shape detection because iceberg-go does not expose a typed auth
// error (the REST client embeds the upstream HTTP body verbatim).
func (p *polarisAdapter) translateError(prefix string, err error) error {
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
// shape. Polaris (and Iceberg REST in general) signals these as
// HTTP 409 with a body containing "CommitFailedException" or
// "commit conflict"; iceberg-go surfaces both verbatim in the
// error message, so substring matches are the available shape.
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
			// ingestion's manifest-reader pass (Plan 01-04). The
			// IcebergCatalogClient surface needs to expose the
			// Snapshot at all so the L1 gateway can branch on
			// SnapshotID without a second LoadTable round-trip.
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
// so the only callers that hit polarisAdapter.CommitTable are:
//   (a) ExpireSnapshots above — bypasses convertRequirements /
//       convertUpdates entirely by calling p.cat.CommitTable with
//       directly-constructed iceberg-go updates.
//   (b) Future gateway code (Plan 01-06) — will own the
//       sophisticated requirement / update conversion using
//       iceberg-go's typed constructors directly, NOT via this
//       map-based path.
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
	return nil, fmt.Errorf("polaris: convert requirements: untyped map conversion not implemented in Phase 1 — Plan 01-06 will land typed dispatch (got %d requirements)", len(in))
}

func convertUpdates(in []iceberg.TableUpdate) ([]icebergTable.Update, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("polaris: convert updates: untyped map conversion not implemented in Phase 1 — Plan 01-06 will land typed dispatch (got %d updates)", len(in))
}
