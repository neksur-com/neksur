//go:build commercial && !enterprise

// main_commercial.go — L2 (commercial-only, not enterprise) module init.
//
// This file is compiled ONLY when `commercial` is set and `enterprise` is NOT.
// It imports neksur-commercial packages and wires L2 features:
//   - schemacache broadcaster
//   - write-conflict store
//   - continuous verifier sampler + mirror
//
// When the `enterprise` tag is also set, main_enterprise.go takes over and
// provides ALL three init functions (commercial + enterprise) in a single file
// so there are no symbol conflicts.
//
// License gate: requiresLicense=true — the L2 binary refuses to boot without a
// valid license file (NEKSUR_LICENSE_PATH). Per D-3.04 and ADR-002.
package main

import (
	"context"
	"log/slog"

	"github.com/neksur-com/neksur/internal/license"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"

	"github.com/neksur-com/neksur-commercial/coordination/schemacache"
	"github.com/neksur-com/neksur-commercial/coordination/verifier"
	"github.com/neksur-com/neksur-commercial/coordination/writeconflict"
)

// requiresLicense = true: the L2 commercial binary must have a valid license
// file at boot (per D-3.04).
var requiresLicense = true

// writeConflictAdapter adapts writeconflict.Store to the iceberggw.WriteConflictStore
// interface. The gateway interface uses iceberg.TableRef; the commercial package
// uses (tableName, tableNamespace string) to avoid importing neksur-core internal/.
type writeConflictAdapter struct {
	store *writeconflict.Store
}

func (a *writeConflictAdapter) LoadForTable(ctx context.Context, ref iceberg.TableRef) (string, error) {
	ns := ""
	if len(ref.Namespace) > 0 {
		ns = ref.Namespace[len(ref.Namespace)-1]
	}
	return a.store.LoadForTable(ctx, ref.Name, ns)
}

// tenantIDFromContextFn adapts tenant.IDFromContext to the
// writeconflict.TenantIDFromContextFn type. Avoids importing neksur-core
// internal/ from the commercial module — the function is injected here.
func tenantIDFromContextFn(ctx context.Context) (string, bool) {
	id, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", false
	}
	return id.String(), true
}

// startCommercialModules is the shared L2 wiring logic called by both
// initCommercialModules (L2-only binary) and initEnterpriseModules
// (L3 binary, via main_enterprise.go which re-declares these functions).
// Exported as a standalone function to avoid duplication in main_enterprise.go.
//
// This function is only present in the commercial-only build (main_commercial.go).
// main_enterprise.go re-implements it inline for the enterprise build.
func startCommercialModules(deps iceberggw.Deps) {
	// 1. Schema-cache broadcaster (feature="schema_cache_broadcaster").
	broadcaster := schemacache.NewBroadcasterWithLicenseCheck(
		deps.Pool,
		nil, // invalidator: nil — gracefully skips dispatch; wire live Invalidator in Plan 03-15
		nil, // tenantRepo: nil — gracefully skips tenant validation; wire *tenant.Repo in Plan 03-15
		license.IsFeatureAllowed,
	)
	if broadcaster != nil {
		go func() {
			if err := broadcaster.Listen(context.Background()); err != nil {
				slog.Error("commercial: schemacache broadcaster exited", "err", err)
			}
		}()
		slog.Info("commercial: schemacache broadcaster started")
	}

	// 2. Write-conflict store (feature="write_conflict").
	wcStore := writeconflict.NewStoreWithLicenseCheck(
		deps.Pool,
		nil,                   // graphSyncer: nil — graph sync is best-effort; wire in Plan 03-15
		tenantIDFromContextFn,
		license.IsFeatureAllowed,
	)
	if wcStore != nil {
		deps.WriteConflictStore = &writeConflictAdapter{store: wcStore}
		slog.Info("commercial: write-conflict store wired")
	}

	// 3. Continuous verifier: Sampler + Mirror (feature="continuous_verifier").
	engines := []string{"trino", "spark", "dremio"}
	sampler := verifier.NewSampler(
		nil, // picker: nil — deferred to Plan 03-15 live DB wiring
		nil, // executor: nil — deferred to Plan 03-15
		nil, // suspendFn: nil — deferred to Plan 03-15
		license.IsFeatureAllowed,
		verifier.DefaultSamplerConfig(),
		engines,
	)
	if sampler != nil {
		go func() {
			if err := sampler.Run(context.Background()); err != nil {
				slog.Error("commercial: verifier sampler exited", "err", err)
			}
		}()
		slog.Info("commercial: verifier sampler started")
	}

	mirror := verifier.NewMirror(
		engines,
		nil, // dispatchFn: nil — deferred to Plan 03-15
		nil, // suspendFn: nil — deferred to Plan 03-15
		license.IsFeatureAllowed,
		nil, // dropMetrics: nil → noopDropMetrics inside NewMirror
	)
	if mirror != nil {
		go func() {
			mirror.Run(context.Background())
		}()
		slog.Info("commercial: verifier mirror started")
	}
}

// initCommercialModules starts all L2 features.
// Called unconditionally by main.go after license verification succeeds.
func initCommercialModules(deps iceberggw.Deps) {
	startCommercialModules(deps)
}

// initEnterpriseModules is a no-op in the L2-only (commercial, not enterprise) binary.
// The L3 implementation is in main_enterprise.go (//go:build commercial && enterprise).
func initEnterpriseModules(_ iceberggw.Deps) {}

// requiredLicenseTier returns the license tier required for this binary.
// The L2 commercial binary requires at least a "commercial" tier license.
func requiredLicenseTier() string { return "commercial" }
