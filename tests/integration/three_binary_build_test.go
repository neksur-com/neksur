//go:build integration

// three_binary_build_test.go — Pitfall 7 (build-tag drift) integration test.
//
// TestThreeBinaryBuildIsolation builds all three binary tiers (L1/L2/L3) and
// verifies symbol-level tier isolation:
//
//   - L1 (core): must contain ZERO private coordination module symbols.
//   - L2 (commercial): must contain schemacache / verifier / writeconflict
//     symbols but NOT partitionspec / compaction.
//   - L3 (enterprise): must contain ALL FIVE coordination symbols.
//
// This test depends on Plan 03-14 having bootstrapped the private repos
// (neksur-commercial, neksur-enterprise) as sibling directories to neksur-core.
// With depends_on: [02, 14] (I-2 wave swap), the private modules are guaranteed
// present at Wave 5 execution time.
//
// The test invokes `make build-core build-commercial build-enterprise` and then
// runs `go tool nm` over each binary to enumerate symbols. `go tool nm` is
// preferred over `strings` for deterministic symbol enumeration (nm lists all
// linker symbols; strings may miss symbols stored without adjacent printable
// context).
//
// Run with: go test -tags=integration ./tests/integration/ -run TestThreeBinaryBuildIsolation -v
// Plan 03-13 §Three-binary build matrix.

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestThreeBinaryBuildIsolation builds all three binary tiers and asserts
// per-tier symbol isolation per Plan 03-13 §Pitfall 7.
func TestThreeBinaryBuildIsolation(t *testing.T) {
	// Find the neksur-core root. The test binary runs from the repo root
	// (go test ./tests/integration/...) so the repo root is the cwd of the
	// test process's working directory.
	// We use runtime.Caller to find the test file and walk up to the repo root.
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	// testFile is .../tests/integration/three_binary_build_test.go
	// repo root is two directories up.
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(testFile)))
	t.Logf("repo root: %s", repoRoot)

	// Verify that make is available.
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not found in PATH — skipping binary build test: %v", err)
	}

	// Build all three binaries via make. Each target writes to ./bin/.
	t.Log("Building all three binary tiers via 'make build-all'...")
	buildCmd := exec.Command("make", "build-all")
	buildCmd.Dir = repoRoot
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Logf("make build-all output:\n%s", buildOut)
		require.NoError(t, err, "make build-all failed")
	}
	t.Logf("make build-all succeeded")

	binDir := filepath.Join(repoRoot, "bin")
	coreBin := filepath.Join(binDir, "neksur-server")
	commercialBin := filepath.Join(binDir, "neksur-server-commercial")
	enterpriseBin := filepath.Join(binDir, "neksur-server-enterprise")

	for _, b := range []string{coreBin, commercialBin, enterpriseBin} {
		_, err := os.Stat(b)
		require.NoError(t, err, "binary not found: %s", b)
	}

	// Symbol patterns for each tier's coordination packages.
	l2Symbols := []string{
		"coordination/schemacache",
		"coordination/verifier",
		"coordination/writeconflict",
	}
	l3OnlySymbols := []string{
		"coordination/partitionspec",
		"coordination/compaction",
	}
	allPrivateSymbols := append(l2Symbols, l3OnlySymbols...)

	// --- L1 (core) binary: must contain ZERO private symbols. ---
	t.Run("L1_core_no_private_symbols", func(t *testing.T) {
		for _, sym := range allPrivateSymbols {
			assert.False(t, binaryContainsSymbol(t, coreBin, sym),
				"L1 core binary must NOT contain private symbol: %s", sym)
		}
	})

	// --- L2 (commercial) binary: must contain L2 symbols but NOT L3-only symbols. ---
	t.Run("L2_commercial_symbols", func(t *testing.T) {
		for _, sym := range l2Symbols {
			assert.True(t, binaryContainsSymbol(t, commercialBin, sym),
				"L2 commercial binary must contain L2 symbol: %s", sym)
		}
		for _, sym := range l3OnlySymbols {
			assert.False(t, binaryContainsSymbol(t, commercialBin, sym),
				"L2 commercial binary must NOT contain L3-only symbol: %s", sym)
		}
	})

	// --- L3 (enterprise) binary: must contain ALL FIVE symbols. ---
	t.Run("L3_enterprise_all_symbols", func(t *testing.T) {
		for _, sym := range allPrivateSymbols {
			assert.True(t, binaryContainsSymbol(t, enterpriseBin, sym),
				"L3 enterprise binary must contain private symbol: %s", sym)
		}
	})

	// --- Pitfall 7 gate: run the CI symbol check script against core binary. ---
	t.Run("symbol_check_script", func(t *testing.T) {
		scriptPath := filepath.Join(repoRoot, "scripts/ci/binary-symbol-check.sh")
		if _, err := os.Stat(scriptPath); err != nil {
			t.Skipf("symbol check script not found: %v", err)
		}
		cmd := exec.Command("bash", scriptPath, coreBin)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		assert.NoError(t, err, "binary-symbol-check.sh should exit 0 for L1 core binary:\n%s", out)
	})
}

// binaryContainsSymbol returns true if the binary at path contains the given
// substring in its symbol table (via `go tool nm`). Falls back to `strings`
// if nm fails (e.g., stripped binary). Logs output for debugging.
func binaryContainsSymbol(t *testing.T, binaryPath, symbol string) bool {
	t.Helper()

	// Try go tool nm first (most reliable for Go binaries).
	nmCmd := exec.Command("go", "tool", "nm", binaryPath)
	nmOut, nmErr := nmCmd.Output()
	if nmErr == nil {
		return strings.Contains(string(nmOut), symbol)
	}

	// Fallback: strings (works on any binary, less precise).
	t.Logf("go tool nm failed for %s: %v — falling back to strings", filepath.Base(binaryPath), nmErr)
	strCmd := exec.Command("strings", binaryPath)
	strOut, strErr := strCmd.Output()
	if strErr != nil {
		t.Logf("strings %s also failed: %v", filepath.Base(binaryPath), strErr)
		return false
	}
	return strings.Contains(string(strOut), symbol)
}
