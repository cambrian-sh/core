package domain

import (
	"context"
	"testing"
)

// T-20261104-kernel-listtools-names-ignores-run-scoped-skill: a tool a system skill
// conferred under a lease must be DESCRIBABLE (full schema) by that lease, as well as
// callable — and by nothing else.
func describeExecutor() *ToolExecutor {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "write_file", Schema: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)})
	reg.Register(SystemTool{Name: "read_file", Schema: []byte(`{"type":"object"}`)})
	grants := NewInMemoryGrantsStore()
	grants.Set("agent-a", []ToolGrant{{Tool: "read_file"}})
	return &ToolExecutor{Registry: reg, Grants: grants, Overlay: NewRunGrantOverlay()}
}

func names(ts []SystemTool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestAvailableToolsNamedFor_DescribesOverlayGrantedTool(t *testing.T) {
	t.Parallel()
	e := describeExecutor()
	e.Overlay.Activate("lease-1", []string{"write_file"})
	ctx := context.Background()

	got := e.AvailableToolsNamedFor(ctx, "agent-a", "lease-1", []string{"read_file", "write_file"})
	if n := names(got); len(n) != 2 || n[0] != "read_file" || n[1] != "write_file" {
		t.Fatalf("want [read_file write_file], got %v", n)
	}
	if len(got[1].Schema) == 0 {
		t.Fatalf("overlay-granted tool must carry its registry schema")
	}
}

func TestAvailableToolsNamedFor_NoLeaseOrOtherLeaseDescribesNothingExtra(t *testing.T) {
	t.Parallel()
	e := describeExecutor()
	e.Overlay.Activate("lease-1", []string{"write_file"})
	ctx := context.Background()

	for _, tok := range []string{"", "lease-2"} {
		got := names(e.AvailableToolsNamedFor(ctx, "agent-a", tok, []string{"write_file"}))
		if len(got) != 0 {
			t.Fatalf("token %q: an un-conferred tool must stay absent, got %v", tok, got)
		}
	}
	// The legacy entry point is unchanged: standing grants only.
	if got := names(e.AvailableToolsNamed(ctx, "agent-a", []string{"write_file"})); len(got) != 0 {
		t.Fatalf("AvailableToolsNamed must not consult the overlay, got %v", got)
	}
}

func TestAvailableToolsNamedFor_RestrictedAndAnonymousCeilings(t *testing.T) {
	t.Parallel()
	e := describeExecutor()
	e.Overlay.Activate("lease-1", []string{"write_file"})
	e.RestrictedTools = map[string]map[string]bool{"agent-a": {"read_file": true}}
	ctx := context.Background()

	if got := names(e.AvailableToolsNamedFor(ctx, "agent-a", "lease-1", []string{"write_file"})); len(got) != 0 {
		t.Fatalf("restricted principal must not see a non-allowlisted overlay tool, got %v", got)
	}
	if got := e.AvailableToolsNamedFor(ctx, "", "lease-1", []string{"write_file"}); got != nil {
		t.Fatalf("anonymous principal must get nothing, got %v", names(got))
	}
}
