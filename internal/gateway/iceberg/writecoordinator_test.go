// Package iceberg — write-coordinator pre-commit hook tests.
//
// TDD RED phase for Plan 03-09 Task 1.
// Covers the 11 behaviors specified in the plan.
package iceberg

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/neksur-com/neksur/internal/iceberg"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// mockPartitionSpecStore implements PartitionSpecStore for unit tests.
type mockPartitionSpecStore struct {
	spec *iceberg.PartitionSpec
	err  error
}

func (m *mockPartitionSpecStore) LoadActive(_ context.Context, _ iceberg.TableRef) (*iceberg.PartitionSpec, error) {
	return m.spec, m.err
}

// mockWriteConflictStore implements WriteConflictStore for unit tests.
type mockWriteConflictStore struct {
	policy string
	err    error
}

func (m *mockWriteConflictStore) LoadForTable(_ context.Context, _ iceberg.TableRef) (string, error) {
	return m.policy, m.err
}

// ---------------------------------------------------------------------------
// detectEngineKind tests
// ---------------------------------------------------------------------------

func TestDetectEngineKind_Trino(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", "trino-client/420 (trino go driver)")
	got := detectEngineKind(r)
	if got != "trino" {
		t.Errorf("expected trino, got %q", got)
	}
}

func TestDetectEngineKind_Dremio(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", "Dremio/25.0 (Iceberg REST)")
	got := detectEngineKind(r)
	if got != "dremio" {
		t.Errorf("expected dremio, got %q", got)
	}
}

func TestDetectEngineKind_Spark(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", "iceberg-spark/3.5")
	got := detectEngineKind(r)
	if got != "spark" {
		t.Errorf("expected spark, got %q", got)
	}
}

func TestDetectEngineKind_SparkPlain(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", "spark-core/3.4.1")
	got := detectEngineKind(r)
	if got != "spark" {
		t.Errorf("expected spark for plain spark UA, got %q", got)
	}
}

func TestDetectEngineKind_Unknown(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", "curl/8.0")
	got := detectEngineKind(r)
	if got != "" {
		t.Errorf("expected empty string for unknown UA, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// WriteCoordinatorPreCommit tests
// ---------------------------------------------------------------------------

func TestWriteCoordinator_EngineKindSpark_Passthrough(t *testing.T) {
	deps := Deps{} // all Phase 3 stores nil
	ctx := context.Background()
	newCtx, err := WriteCoordinatorPreCommit(ctx, deps, "spark", iceberg.TableRef{}, nil, iceberg.CommitRequest{})
	if err != nil {
		t.Errorf("expected nil error for spark passthrough, got %v", err)
	}
	if newCtx == nil {
		t.Error("returned context must not be nil")
	}
}

func TestWriteCoordinator_EngineKindEmpty_Passthrough(t *testing.T) {
	deps := Deps{} // all Phase 3 stores nil
	ctx := context.Background()
	newCtx, err := WriteCoordinatorPreCommit(ctx, deps, "", iceberg.TableRef{}, nil, iceberg.CommitRequest{})
	if err != nil {
		t.Errorf("expected nil error for empty engine passthrough, got %v", err)
	}
	if newCtx == nil {
		t.Error("returned context must not be nil")
	}
}

func TestWriteCoordinator_NilStoresPassthrough(t *testing.T) {
	// L1-only binary path: all Phase 3 stores are nil; hook skips all checks.
	deps := Deps{
		PartitionSpecStore: nil,
		WriteConflictStore: nil,
		// PinStore is always non-nil in production, but the hook itself
		// does not consult PinStore directly — the pin HTTP route does.
	}
	ctx := context.Background()
	newCtx, err := WriteCoordinatorPreCommit(ctx, deps, "trino", iceberg.TableRef{}, nil, iceberg.CommitRequest{})
	if err != nil {
		t.Errorf("expected nil when all stores nil (L1-only path), got %v", err)
	}
	if newCtx == nil {
		t.Error("returned context must not be nil")
	}
}

func TestWriteCoordinator_PartitionSpecMismatch(t *testing.T) {
	// PartitionSpecStore returns spec_id=2; commit has spec_id=1 → ErrPartitionSpecMismatch.
	activeSpec := &iceberg.PartitionSpec{SpecID: 2}
	deps := Deps{
		PartitionSpecStore: &mockPartitionSpecStore{spec: activeSpec},
	}
	req := iceberg.CommitRequest{}
	// The commit's PartitionSpec has SpecID=1 (default zero value is 0, use explicit 1).
	// We pass current metadata with SpecID=1.
	current := &iceberg.TableMetadata{
		PartitionSpec: iceberg.PartitionSpec{SpecID: 1},
	}
	ctx := context.Background()
	_, err := WriteCoordinatorPreCommit(ctx, deps, "trino", iceberg.TableRef{}, current, req)
	if !errors.Is(err, iceberg.ErrPartitionSpecMismatch) {
		t.Errorf("expected ErrPartitionSpecMismatch, got %v", err)
	}
}

func TestWriteCoordinator_PartitionSpecMatch(t *testing.T) {
	// active spec_id=2; current meta spec_id=2 → match → nil.
	activeSpec := &iceberg.PartitionSpec{SpecID: 2}
	deps := Deps{
		PartitionSpecStore: &mockPartitionSpecStore{spec: activeSpec},
	}
	current := &iceberg.TableMetadata{
		PartitionSpec: iceberg.PartitionSpec{SpecID: 2},
	}
	ctx := context.Background()
	_, err := WriteCoordinatorPreCommit(ctx, deps, "trino", iceberg.TableRef{}, current, iceberg.CommitRequest{})
	if err != nil {
		t.Errorf("expected nil on spec match, got %v", err)
	}
}

func TestWriteCoordinator_WriteConflictPolicyAttached(t *testing.T) {
	// WriteConflictStore returns "abort" → returned ctx has policy attached.
	deps := Deps{
		WriteConflictStore: &mockWriteConflictStore{policy: "abort"},
	}
	ctx := context.Background()
	newCtx, err := WriteCoordinatorPreCommit(ctx, deps, "trino", iceberg.TableRef{}, nil, iceberg.CommitRequest{})
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	policy, ok := WriteConflictPolicyFromContext(newCtx)
	if !ok {
		t.Error("expected conflict policy to be attached to context")
	}
	if policy != "abort" {
		t.Errorf("expected policy=abort, got %q", policy)
	}
}

// ---------------------------------------------------------------------------
// B-2: polaris.IsCommitConflict exported symbol test
// ---------------------------------------------------------------------------

func TestPolaris_IsCommitConflict_Exported(t *testing.T) {
	// Smoke test for B-2 rename: polaris.IsCommitConflict must be exported.
	// We call the exported wrapper which delegates to polaris.IsCommitConflict —
	// if the symbol is unexported this package would fail to compile.
	err := iceberg.ErrCommitConflict
	result := isCommitConflictForTest(err)
	if !result {
		t.Error("IsCommitConflict(ErrCommitConflict) should return true")
	}
	// Also verify a nil error returns false.
	if isCommitConflictForTest(nil) {
		t.Error("IsCommitConflict(nil) should return false")
	}
}
