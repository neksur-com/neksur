//go:build commercial && enterprise

// Package main — import-proof test for private module wiring.
//
// This file proves that the Go toolchain can resolve both private modules at
// compile time when the commercial AND enterprise build tags are active.
//
// EXPECTED BUILD STATUS:
//
//	Until Plans 03-07 and 03-08 land their feature packages in the private repos,
//	this file will FAIL to compile under -tags='commercial enterprise' because the
//	imported packages do not yet exist:
//
//	  - github.com/neksur-com/neksur-commercial/coordination/schemacache  (Plan 03-07)
//	  - github.com/neksur-com/neksur-enterprise/coordination/partitionspec (Plan 03-08)
//
//	The file is committed now (Plan 03-14) so Plan 03-13's CI matrix can reference
//	it as a build target. The test will pass once those plans land.
//
// CI USAGE:
//
//	go build -tags='commercial enterprise' ./cmd/neksur-server/...
//
// This is the sole compile-time gate for private module resolution. If either
// import fails to resolve, the build fails and the CI matrix exits non-zero.
package main

import (
	"testing"

	// Side-effect imports prove that the private module packages are resolvable
	// and that the go.mod require + replace directives in neksur-core are correct.
	_ "github.com/neksur-com/neksur-commercial/coordination/schemacache"
	_ "github.com/neksur-com/neksur-enterprise/coordination/partitionspec"
)

// TestModuleImportsResolve passes trivially at runtime.
// Its value is at compile time: if the imports above fail to resolve,
// the test binary cannot be built and CI exits non-zero.
func TestModuleImportsResolve(t *testing.T) {
	t.Log("private modules resolved at compile time")
}
