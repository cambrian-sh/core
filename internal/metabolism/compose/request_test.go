package compose

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0150 D1: with no pinned agent the single-agent lane runs the composed
// runtime with EVERY profession's tools the person's scope admits (never a
// resource-argument tool, never one outside every allowlist), bound to the lease
// and recorded, so the tool executor enforces it like a step composition.
func TestComposeRequest_HoldsThePersonsPermittedTools(t *testing.T) {
	c, led := fixture(t)
	c.Fleet = fakeFleet{profileAgent("researcher"), profileAgent("assistant"), profileAgent("analyst"),
		{ID: "code_agent"}} // a code agent is no profession
	in := &domain.Handoff{Context: map[string]string{}}
	runtime, comp, err := c.ComposeRequest(context.Background(), "lease-r", "tidy my receipts", in)
	if err != nil {
		t.Fatal(err)
	}
	if runtime != "composed_worker" {
		t.Fatalf("runtime = %q", runtime)
	}
	want := []string{"mcp:fs/read_file", "mcp:fs/write_file", "mcp:sandbox/run", "mcp:web/search"}
	if !reflect.DeepEqual(comp.Tools, want) {
		t.Fatalf("tools = %v, want %v (scope-denied, resource-arg and unallowed tools out)", comp.Tools, want)
	}
	if len(comp.Professions) != 3 || comp.Brief != "tidy my receipts" {
		t.Fatalf("composition %+v", comp)
	}
	if got, ok := c.Leases.For("lease-r"); !ok || got.ID() != comp.ID() {
		t.Fatal("the composition is not bound to the lease")
	}
	if !c.Overlay.Granted("lease-r", "mcp:web/search") || c.Overlay.Granted("lease-r", "mcp:mail/send") {
		t.Fatal("run-scoped grants do not match the composition")
	}
	if in.Context[domain.HandoffKeyComposition] == "" || len(led.recs) != 1 {
		t.Fatal("composition not stamped or not recorded")
	}
	// A retried dispatch on the same lease keeps it.
	_, again, _ := c.ComposeRequest(context.Background(), "lease-r", "other words", in)
	if again.ID() != comp.ID() || len(led.recs) != 1 {
		t.Fatal("a retry re-composed the lease")
	}
}

func TestComposeRequest_RefusesWithoutProfessionsOrLease(t *testing.T) {
	c, _ := fixture(t)
	c.Fleet = fakeFleet{{ID: "code_agent"}}
	if _, _, err := c.ComposeRequest(context.Background(), "lease-x", "x", nil); !errors.Is(err, ErrNoProfessions) {
		t.Fatalf("err = %v, want ErrNoProfessions", err)
	}
	if _, _, err := c.ComposeRequest(context.Background(), "", "x", nil); err == nil {
		t.Fatal("no lease must be refused")
	}
}
