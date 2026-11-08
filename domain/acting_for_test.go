package domain

import "testing"

func TestStampActingFor(t *testing.T) {
	ctx := map[string]string{HandoffKeyActingFor: "stale", HandoffKeyActingForName: "Stale"}
	StampActingFor(ctx, "u1", "Ada Lovelace")
	if ctx[HandoffKeyActingFor] != "u1" || ctx[HandoffKeyActingForName] != "Ada Lovelace" {
		t.Fatalf("stamp: %v", ctx)
	}
	StampActingFor(ctx, "u1", "u1") // a name equal to the id adds nothing
	if _, ok := ctx[HandoffKeyActingForName]; ok {
		t.Fatalf("name equal to id kept: %v", ctx)
	}
	StampActingFor(ctx, "", "Ada")
	if len(ctx) != 0 {
		t.Fatalf("empty principal must clear both keys: %v", ctx)
	}
}
