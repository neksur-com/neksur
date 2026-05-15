// regexScanner — the Scanner implementation wired into the dispatch
// pool at neksur-server startup. Implements the L3 detection scan
// pipeline:
//
//   1. Hit arrives → load the table's catalog adapter via per-tenant
//      catalog.Repo.GetCatalogCredentials + gateway/iceberg.BuildAdapter.
//   2. LoadTable to get TableMetadata + Snapshots.
//   3. Stratified-sample the manifest files via detect.StratifiedSample.
//   4. For each sampled file's columns, run regex.RegexClassifier.Classify
//      against the column name + a sample of cell values (read from the
//      Iceberg manifest reader — Phase 1 simplification reads only the
//      schema's column names; full value sampling is Phase 6 work because
//      it requires reading the Parquet/ORC file content).
//   5. Call regex.EmitDetectionResults to land the DetectionRun + Tag +
//      Classification graph nodes + audit row.
//   6. On any finding with confidence >= 0.85, post Slack alert via
//      alerts.Slack.
//
// Phase 1 simplification on value sampling: the full classifier
// pipeline benefits from reading actual cell values to apply the
// combined name+value confidence scoring. For the basic detection
// surface we run the classifier with the column NAME only (so name-
// matches yield 0.65 confidence — below 0.85 — by design). The
// integration tests inject sample values directly into the classifier
// to exercise the combined-match alert path; production callers that
// need value sampling extend regexScanner to wire in
// /internal/iceberg/manifest reader (deferred to Phase 6).

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neksur-com/neksur/internal/alerts"
	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/detect"
	"github.com/neksur-com/neksur/internal/detect/dispatch"
	"github.com/neksur-com/neksur/internal/detect/regex"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"

	"github.com/google/uuid"
)

// regexScanner implements dispatch.Scanner.
type regexScanner struct {
	pool       *pgxpool.Pool // admin pool for catalog.Repo
	gc         *graph.GraphClient
	credStore  *catalog.Repo
	classifier *regex.RegexClassifier
	slack      *alerts.Slack
	threshold  float64
}

// newRegexScanner constructs the scanner with the production wiring.
func newRegexScanner(
	pool *pgxpool.Pool,
	gc *graph.GraphClient,
	credStore *catalog.Repo,
	slack *alerts.Slack,
) *regexScanner {
	return &regexScanner{
		pool:       pool,
		gc:         gc,
		credStore:  credStore,
		classifier: regex.NewRegexClassifier(),
		slack:      slack,
		threshold:  regex.AlertThreshold,
	}
}

// Scan implements dispatch.Scanner. Per-Hit:
//
//  1. Build per-tenant ctx so catalog.Repo + gateway/iceberg.BuildAdapter
//     resolve correctly.
//  2. Load credentials via the per-tenant Repo (the Hit doesn't carry
//     a "nickname" — Phase 1 single-catalog-per-tenant default uses
//     `prod-polaris`).
//  3. Build adapter; LoadTable; iterate columns.
//  4. Classify each column (name-only Phase 1 — see file header).
//  5. Emit results.
//  6. Slack alert on >=threshold findings.
func (r *regexScanner) Scan(ctx context.Context, hit dispatch.Hit) error {
	tenantUUID, err := uuid.Parse(hit.TenantID)
	if err != nil {
		return fmt.Errorf("scanner: parse tenant id: %w", err)
	}
	ctx = tenant.WithID(ctx, tenantUUID)

	creds, err := r.credStore.GetCatalogCredentials(ctx, "prod-polaris")
	if err != nil {
		// Phase 1 multi-catalog support is deferred — if the tenant
		// doesn't have a polaris row, log + skip (the BLOCKING tests
		// inject a custom AdapterFactory; production requires the row).
		if errors.Is(err, catalog.ErrCredentialsNotFound) {
			slog.Warn("scanner: no catalog credentials; skipping",
				"tenant", hit.TenantID, "meta_loc", hit.MetadataLocation)
			return nil
		}
		return fmt.Errorf("scanner: load credentials: %w", err)
	}

	adapter, err := iceberggw.BuildAdapter(ctx, creds)
	if err != nil {
		return fmt.Errorf("scanner: build adapter: %w", err)
	}

	// Phase 1 simplification: we don't yet have the table ref from the
	// Hit (the Hit carries metadata_location but not necessarily
	// namespace+name). In production the webhook + s3-event paths
	// SHOULD include namespace+name in the Hit; the poller resolves it
	// from the Snapshot's owning Table relationship. For Phase 1 if
	// the Hit lacks namespace/name, we skip the LoadTable + classify
	// pass and ONLY emit the DetectionRun audit (which is still
	// useful — proves the trigger fired).
	var schema iceberg.Schema
	if hit.TableName != "" {
		ref := iceberg.TableRef{
			Namespace: hit.TableNamespace,
			Name:      hit.TableName,
		}
		meta, err := adapter.LoadTable(ctx, ref)
		if err != nil {
			slog.Warn("scanner: LoadTable failed; emitting DetectionRun without findings",
				"err", err, "tenant", hit.TenantID, "ref", ref)
		} else if meta != nil {
			schema = meta.Schema
		}
	}

	findings := classifyAllColumns(r.classifier, schema)

	// Stratified sampling per ADR-003 §5.4 — Phase 1 doesn't iterate
	// individual data files (deferred); we apply the sampling concept
	// to the column set: the call records the strategy + sample_size
	// for the audit trail. Full file-level sampling lands when
	// manifest-reader integration ships.
	sampleSize := len(findings)
	strategy := "regex"

	runID, err := regex.EmitDetectionResults(ctx, r.gc, hit.TenantID,
		hit.MetadataLocation, strategy, sampleSize, findings)
	if err != nil {
		return fmt.Errorf("scanner: emit detection results: %w", err)
	}
	if runID == "" {
		// Cross-replica dedup hit — another replica is scanning. OK.
		slog.Debug("scanner: cross-replica dedup; skipped emission",
			"tenant", hit.TenantID, "meta_loc", hit.MetadataLocation)
		return nil
	}

	// Slack alerts on >= threshold findings (D-1.12 + D-OQ.07).
	for _, f := range findings {
		if f.Confidence < r.threshold {
			continue
		}
		summary := fmt.Sprintf("PII detected: %s (tag=%s, confidence=%.2f)",
			f.ColumnName, f.TagID, f.Confidence)
		details := map[string]string{
			"column":     f.ColumnName,
			"tag":        f.TagID,
			"confidence": fmt.Sprintf("%.2f", f.Confidence),
			"meta_loc":   hit.MetadataLocation,
			"source":     hit.Source,
			"run_id":     runID,
		}
		if err := r.slack.Post(ctx, "warning", summary, hit.TenantID, details); err != nil {
			// Slack failures are best-effort — log + continue.
			slog.Error("scanner: slack post failed",
				"err", err, "tenant", hit.TenantID, "tag", f.TagID)
		}
	}

	// Capture detect package reference for the import graph.
	_ = detect.ErrAllSourcesUnavailable

	return nil
}

// classifyAllColumns runs the classifier against every column in the
// schema. Phase 1 limitation: classifies on column name ONLY (no value
// sampling). Combined name+value matches require the manifest reader
// integration deferred to Phase 6; the integration tests exercise the
// combined-match path by calling the classifier directly with both
// args.
func classifyAllColumns(c *regex.RegexClassifier, sc iceberg.Schema) []regex.ColumnFinding {
	var out []regex.ColumnFinding
	for _, f := range sc.Fields {
		out = append(out, c.Classify(f.Name, nil)...)
	}
	return out
}
