package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// The ListTools RPC surfaces an agent's granted system tools (its prompt menu)
// using the non-forgeable x-agent-id principal, and degrades to an empty menu —
// never an error — when no tool registry is configured.
func TestListTools_GrantedAgentSeesItsTools(t *testing.T) {
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{
		Name:        "execute_command",
		Description: "Run a shell command",
		Schema:      []byte(`{"properties":{"command":{"type":"string"}}}`),
		Dangerous:   true,
	})
	reg.Register(domain.SystemTool{Name: "read_file"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("agent-a", []domain.ToolGrant{{Tool: "execute_command"}})

	srv := &Server{ToolExecutor: &domain.ToolExecutor{Registry: reg, Grants: grants}}

	resp, err := srv.ListTools(ctxWithAgentID("agent-a"), &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("agent-a granted 1 tool, menu has %d", len(resp.Tools))
	}
	got := resp.Tools[0]
	if got.Name != "execute_command" || !got.Dangerous || got.SchemaJson == "" {
		t.Errorf("descriptor not faithfully mapped: %+v", got)
	}
}

func TestListTools_AnonymousSeesNothing(t *testing.T) {
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "read_file"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("agent-a", []domain.ToolGrant{{Tool: "read_file"}})
	srv := &Server{ToolExecutor: &domain.ToolExecutor{Registry: reg, Grants: grants}}

	// No x-agent-id metadata ⇒ anonymous ⇒ empty menu (fail-closed).
	resp, err := srv.ListTools(context.Background(), &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(resp.Tools) != 0 {
		t.Errorf("anonymous principal must see no tools, got %d", len(resp.Tools))
	}
}

func TestListTools_NoExecutorReturnsEmptyNotError(t *testing.T) {
	srv := &Server{}
	resp, err := srv.ListTools(ctxWithAgentID("agent-a"), &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("a kernel without a tool registry must yield an empty menu, not an error: %v", err)
	}
	if len(resp.Tools) != 0 {
		t.Errorf("expected empty menu, got %d", len(resp.Tools))
	}
}
