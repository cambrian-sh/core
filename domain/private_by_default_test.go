package domain

import (
	"context"
	"testing"
)

// ENG-1268: WithPrivateWrites(ctx, "") is a no-op by contract; WithoutPrivateWrites
// is the real clear.
func TestWithoutPrivateWrites_ClearsMarker(t *testing.T) {
	ctx := WithPrivateWrites(context.Background(), "alice")
	if got := PrivateWriteOwnerFromContext(WithPrivateWrites(ctx, "")); got != "alice" {
		t.Fatalf(`WithPrivateWrites(ctx, "") changed the marker: %q`, got)
	}
	cleared := WithoutPrivateWrites(ctx)
	if got := PrivateWriteOwnerFromContext(cleared); got != "" {
		t.Fatalf("marker survived WithoutPrivateWrites: %q", got)
	}
	if got := StampPrivateOwner(cleared, []string{"team_kb"}); len(got) != 1 {
		t.Fatalf("cleared ctx still stamps an owner: %v", got)
	}
	bare := context.Background()
	if WithoutPrivateWrites(bare) != bare {
		t.Fatal("unmarked ctx should be returned unchanged")
	}
}
