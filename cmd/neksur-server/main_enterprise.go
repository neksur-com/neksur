//go:build commercial && enterprise

// main_enterprise.go — L3 (enterprise) module init, gated by build tags.
//
// This file is compiled ONLY when BOTH `commercial` AND `enterprise` build tags
// are set (the L3 enterprise binary). It provides ALL four declarations needed
// for the main package:
//
//   - requiresLicense = true
//   - initCommercialModules (full L2 wiring — mirrors main_commercial.go)
//   - initEnterpriseModules (full L3 wiring)
//   - adapters: writeConflictAdapter, partitionSpecAdapter
//
// Owning ALL symbols in one file avoids symbol conflicts: when enterprise is
// set, main_commercial.go is EXCLUDED (its //go:build commercial && !enterprise
// constraint is false) and this file provides everything.
//
// D-3.04, ADR-002, REQ-deployment-modes.
package main

import (
	"context"
	"log/slog"

	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/license"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/tenant"

	"github.com/neksur-com/neksur-commercial/coordination/schemacache"
	"github.com/neksur-com/neksur-commercial/coordination/verifier"
	"github.com/neksur-com/neksur-commercial/coordination/writeconflict"

	"github.com/neksur-com/neksur-enterprise/coordination/compaction"
	"github.com/neksur-com/neksur-enterprise/coordination/partitionspec"
)

// requiresLicense = true: the L3 enterprise binary must have a valid license
// file at boot (per D-3.04). Only ONE requiresLicense var exists in the
// package — this file provides it for the enterprise build.
var requiresLicense = true

// --------------------------------------------------------------------------
// Adapters.
// --------------------------------------------------------------------------

// writeConflictAdapter adapts writeconflict.Store to iceberggw.WriteConflictStore.
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

// partitionSpecAdapter adapts partitionspec.PartitionSpecStore to
// iceberggw.PartitionSpecStore. The gateway returns *iceberg.PartitionSpec;
// the enterprise store returns *partitionspec.PartitionSpec.
type partitionSpecAdapter struct {
	store *partitionspec.PartitionSpecStore
}

func (a *partitionSpecAdapter) LoadActive(ctx context.Context, ref iceberg.TableRef) (*iceberg.PartitionSpec, error) {
	psRef := partitionspec.TableRef{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}
	ps, err := a.store.LoadActive(ctx, psRef)
	if err != nil {
		return nil, err
	}
	if ps == nil {
		return nil, nil
	}
	// Convert partitionspec.PartitionSpec → iceberg.PartitionSpec.
	// For the gateway write-coordinator, SpecID is the primary signal.
	return &iceberg.PartitionSpec{
		SpecID: ps.SpecID,
	}, nil
}

// tenantIDFromContextFn adapts tenant.IDFromContext to TenantIDFromContextFn
// (writeconflict package's injection type).
func tenantIDFromContextFn(ctx context.Context) (string, bool) {
	id, ok := tenant.IDFromContext(ctx)
	if !ok {
		return "", false
	}
	return id.String(), true
}

// --------------------------------------------------------------------------
// L2 init (mirrors main_commercial.go — included here so both commercial and
// enterprise tags compile cleanly without symbol conflicts).
// --------------------------------------------------------------------------

// initCommercialModules wires all L2 features.
// Called unconditionally by main.go after license verification succeeds.
func initCommercialModules(deps iceberggw.Deps) {
	// 1. Schema-cache broadcaster (feature="schema_cache_broadcaster").
	broadcaster := schemacache.NewBroadcasterWithLicenseCheck(
		deps.Pool,
		nil, // invalidator: nil — gracefully skips dispatch; wire in Plan 03-15
		nil, // tenantRepo: nil — gracefully skips tenant validation; wire in Plan 03-15
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
		nil,                   // graphSyncer: nil — best-effort; wire in Plan 03-15
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
		nil, // picker: nil — deferred to Plan 03-15
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
		nil, // dropMetrics: nil → noopDropMetrics
	)
	if mirror != nil {
		go func() {
			mirror.Run(context.Background())
		}()
		slog.Info("commercial: verifier mirror started")
	}
}

// --------------------------------------------------------------------------
// L3 init.
// --------------------------------------------------------------------------

// requiredLicenseTier returns the license tier required for this binary.
// The L3 enterprise binary requires an "enterprise" tier license.
func requiredLicenseTier() string { return "enterprise" }

// initEnterpriseModules wires all L3 features.
// Called unconditionally by main.go after license verification succeeds.
//
// Feature gates (per D-3.04):
//   - "partition_spec_versioning" → PartitionSpecStore wired into deps
//   - "compaction_coordination"   → CompactionCoordinator ready (full wiring in Plan 03-15)
func initEnterpriseModules(deps iceberggw.Deps) {
	// 1. Partition-spec versioning store (feature="partition_spec_versioning").
	psStore := partitionspec.NewStoreWithLicenseCheck(
		nil, // TxRunner: nil — live AGE graph wired in Plan 03-15
		license.IsFeatureAllowed,
		tenantIDFromContextFn,
	)
	if psStore != nil {
		deps.PartitionSpecStore = &partitionSpecAdapter{store: psStore}
		slog.Info("enterprise: partition-spec versioning store wired")
	}

	// 2. Compaction coordinator (feature="compaction_coordination").
	coord, err := compaction.NewCoordinatorWithLicenseCheck(
		nil,  // PinReader: nil — snapshot.PinStore adapter wired in Plan 03-15
		deps.Pool,
		"",   // tenantID label: empty at boot; set per-request in coordinator
		tenantIDFromContextFn,
		nil,  // CompactionMetrics: nil — observability adapter wired in Plan 03-15
		license.IsFeatureAllowed,
	)
	if err != nil {
		slog.Error("enterprise: compaction coordinator init failed", "err", err)
		return
	}
	if coord != nil {
		// CompactionCoordinator will be stored in a Deps field added in Plan 03-15.
		// Log presence so operators can verify license-gate pass.
		slog.Info("enterprise: compaction coordinator ready (Deps wiring deferred to Plan 03-15)")
	}
}
