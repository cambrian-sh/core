package domain

import (
	"context"
	"testing"
)

// ADR-0051 D6: a restricted principal (the Scout) is confined to its discovery-safe tool
// set as a HARD CEILING — it holds even under Unrestricted, the menu is filtered to it, and
// a non-discovery-safe tool is denied fail-closed. A non-restricted principal is unaffected.
func TestToolExecutor_DiscoverySafeRestriction(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file"})
	reg.Register(SystemTool{Name: "write_file", Dangerous: true})
	ctx := context.Background()

	exec := &ToolExecutor{
		Registry:        reg,
		Grants:          NewInMemoryGrantsStore(),
		Unrestricted:    true, // even in the unrestricted bypass...
		RestrictedTools: map[string]map[string]bool{"scout": {"read_file": true}},
	}

	// ...the Scout sees ONLY its discovery-safe tool in the advisory menu.
	menu := exec.AvailableTools(ctx, "scout")
	if len(menu) != 1 || menu[0].Name != "read_file" {
		t.Fatalf("restricted principal must see only discovery-safe tools; got %v", toolNames(menu))
	}
	// A non-restricted principal under unrestricted still sees everything.
	if all := exec.AvailableTools(ctx, "agentX"); len(all) != 2 {
		t.Errorf("a non-restricted principal must see all tools under unrestricted; got %d", len(all))
	}
	// The Scout cannot INVOKE a non-discovery-safe (write) tool, even under unrestricted.
	if resp := exec.Execute(ctx, ToolCallRequest{AgentID: "scout", ToolName: "write_file", ArgsJSON: []byte("{}")}); !resp.Denied {
		t.Error("the Scout must be denied a non-discovery-safe tool (fail-closed)")
	}
	// The Scout IS granted its discovery-safe tool.
	if _, ok := exec.grantFor(ctx, "scout", "read_file", ""); !ok {
		t.Error("the Scout must be granted its discovery-safe tool")
	}
	// The Scout is denied a tool absent from its allowlist via grantFor directly.
	if _, ok := exec.grantFor(ctx, "scout", "write_file", ""); ok {
		t.Error("the Scout must be denied a non-discovery-safe tool at the grant boundary")
	}
}
