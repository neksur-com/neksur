//go:build !commercial && !enterprise

// main_core.go — L1-only (BSL Core) build stubs.
//
// When compiled WITHOUT the `commercial` or `enterprise` build tags this file
// provides no-op implementations of initCommercialModules and
// initEnterpriseModules so that main.go can call them unconditionally without
// breaking the L1 build.
//
// requiresLicense = false: the BSL Core binary is free-to-use; no license
// file is required at boot. See D-3.04 and ADR-002.
package main

import iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"

// requiresLicense signals whether this binary requires a valid license file at
// boot. Set to true in main_commercial.go and main_enterprise.go; false here
// so the L1 BSL-Core binary boots without NEKSUR_LICENSE_PATH.
var requiresLicense = false

// initCommercialModules is a no-op in the L1-only binary.
// The L2 implementation lives in main_commercial.go (//go:build commercial).
func initCommercialModules(_ iceberggw.Deps) {}

// initEnterpriseModules is a no-op in the L1-only binary.
// The L3 implementation lives in main_enterprise.go (//go:build commercial && enterprise).
func initEnterpriseModules(_ iceberggw.Deps) {}

// requiredLicenseTier returns the license tier required for this binary.
// L1 core has no license requirement.
func requiredLicenseTier() string { return "" }
