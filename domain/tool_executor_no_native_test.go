package domain

import (
	"context"
	"testing"
)

// ENG-1421: the production executor has no default Handler — the native
// tools/*tool.py process handler was deleted (MCP tools only,
// decisions/agent-stack-v2-mcp-tools-and-profiles.md). An mcp: identity still
// dispatches to the MCP handler; any other identity that somehow registered is
// refused, never run as a local subprocess.
func TestExecutor_WithoutANativeHandlerOnlyMCPDispatches(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "mcp:crm/find_customer", Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: "web_search", Effects: []ToolEffect{EffectRead}})
	mcp := &fqHandler{}
	e, err := NewToolExecutor(
		ToolExecutorControls{Registry: reg, Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}, Unrestricted: true},
		ToolExecutor{MCPHandler: mcp, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	call := func(tool string) ToolCallResponse {
		return e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: tool, ArgsJSON: []byte(`{}`)})
	}
	if r := call("mcp:crm/find_customer"); r.Denied || r.Error != "" {
		t.Fatalf("an MCP tool must dispatch: %+v", r)
	}
	if r := call("web_search"); r.Error != "no tool handler configured" {
		t.Fatalf("a native identity must be refused with no handler, got %+v", r)
	}
	if len(mcp.calls) != 1 || mcp.calls[0] != "mcp:crm/find_customer" {
		t.Fatalf("MCP handler saw %v; the native identity must not reach it", mcp.calls)
	}
}
