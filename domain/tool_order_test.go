package domain

import (
	"context"
	"testing"
)

// ENG-126: an unrestricted agent's full tool menu becomes its native tool
// definitions, the head of the provider's cached prompt prefix, so the registry
// must return one order on every call (map order changed it per ListTools call).
func TestInMemoryToolRegistryAllIsSortedByName(t *testing.T) {
	r := NewInMemoryToolRegistry()
	for _, n := range []string{"write_file", "mcp:erp/invoices", "read_file", "list_dir", "http_get", "mcp:crm/contacts"} {
		r.Register(SystemTool{Name: n})
	}
	want := []string{"http_get", "list_dir", "mcp:crm/contacts", "mcp:erp/invoices", "read_file", "write_file"}
	for round := 0; round < 50; round++ {
		got := r.All()
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].Name != want[i] {
				t.Fatalf("round %d: All()[%d] = %q, want %q", round, i, got[i].Name, want[i])
			}
		}
	}
}

// The menu an unrestricted agent is served (tools_unrestricted) is the sorted
// registry, call after call.
func TestAvailableToolsUnrestrictedMenuIsStable(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	for _, n := range []string{"zeta", "alpha", "mu", "beta", "omega", "kappa", "delta"} {
		reg.Register(SystemTool{Name: n})
	}
	e, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}, Unrestricted: true}, ToolExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	first := e.AvailableTools(context.Background(), "a1")
	if len(first) != 7 || first[0].Name != "alpha" || first[6].Name != "zeta" {
		t.Fatalf("menu = %v", first)
	}
	for round := 0; round < 50; round++ {
		got := e.AvailableTools(context.Background(), "a1")
		for i := range first {
			if got[i].Name != first[i].Name {
				t.Fatalf("round %d: menu[%d] = %q, want %q", round, i, got[i].Name, first[i].Name)
			}
		}
	}
}
