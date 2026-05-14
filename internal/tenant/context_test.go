package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWithID_RoundTrip(t *testing.T) {
	want := uuid.MustParse("a1b2c3d4-e5f6-4789-8abc-def012345678")
	ctx := WithID(context.Background(), want)
	got, ok := IDFromContext(ctx)
	if !ok {
		t.Fatalf("IDFromContext: ok = false after WithID")
	}
	if got != want {
		t.Errorf("IDFromContext = %s; want %s", got, want)
	}
}

func TestIDFromContext_Empty(t *testing.T) {
	_, ok := IDFromContext(context.Background())
	if ok {
		t.Errorf("IDFromContext on empty ctx: ok = true; want false")
	}
}
