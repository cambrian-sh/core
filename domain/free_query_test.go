package domain

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// ENG-2232 (owner ruling 2026-11-19 on ENG-1400): the LLM never writes SQL
// (ADR-0149 D6). A tool flagged FreeQuery reaches an agent only through a grant
// naming it exactly — never the tools_unrestricted bypass, a skill's run-scoped
// overlay or a profile glob — unless the executor was built with FreeQueryOpen.

const (
	fqSQL   = "mcp:erp-db/execute_sql"
	fqTyped = "mcp:erp-db/list_tables"
)

type fqHandler struct{ calls []string }

func (h *fqHandler) Execute(_ context.Context, c ToolCall) ([]byte, error) {
	h.calls = append(h.calls, c.ToolName)
	return []byte(`{"ok":true}`), nil
}

func fqRegistry() *InMemoryToolRegistry {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: fqSQL, FreeQuery: true, Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: fqTyped, Effects: []ToolEffect{EffectRead}})
	return reg
}

func fqExecutor(t *testing.T, grants GrantsProvider, unrestricted, open bool) (*ToolExecutor, *fqHandler) {
	t.Helper()
	h := &fqHandler{}
	e, err := NewToolExecutor(
		ToolExecutorControls{Registry: fqRegistry(), Grants: grants, Authz: AllowAllAuthorizer{},
			Unrestricted: unrestricted, FreeQueryOpen: open},
		ToolExecutor{MCPHandler: h, Overlay: NewRunGrantOverlay()})
	if err != nil {
		t.Fatal(err)
	}
	return e, h
}

func fqCall(e *ToolExecutor, tool string) ToolCallResponse {
	return e.Execute(context.Background(), ToolCallRequest{AgentID: "analyst", ToolName: tool, ArgsJSON: []byte(`{}`), SessionTokenID: "sess-1"})
}

func fqMenuNames(ts []SystemTool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestFreeQuery_UnrestrictedBypassDoesNotReachIt(t *testing.T) {
	e, h := fqExecutor(t, NewInMemoryGrantsStore(), true, false)
	resp := fqCall(e, fqSQL)
	if !resp.Denied || resp.DenyReason != FreeQueryDenyReason {
		t.Fatalf("free-SQL tool under tools_unrestricted: want denied with the free-query reason, got %+v", resp)
	}
	if typed := fqCall(e, fqTyped); typed.Denied || typed.Error != "" {
		t.Fatalf("typed tool must still work: %+v", typed)
	}
	if !slices.Equal(h.calls, []string{fqTyped}) {
		t.Fatalf("handler saw %v; the free-SQL call must never dispatch", h.calls)
	}
	if got := fqMenuNames(e.AvailableTools(context.Background(), "analyst")); !slices.Equal(got, []string{fqTyped}) {
		t.Fatalf("menu = %v, want only the typed tool", got)
	}
}

func TestFreeQuery_ExactOperatorGrantAllowsIt(t *testing.T) {
	ctx := context.Background()
	for _, unrestricted := range []bool{false, true} {
		grants := NewInMemoryGrantsStore()
		e, h := fqExecutor(t, grants, unrestricted, false)
		if resp := fqCall(e, fqSQL); !resp.Denied {
			t.Fatalf("unrestricted=%v: ungranted free-SQL call ran", unrestricted)
		}
		if err := grants.ReplaceGrants(ctx, "analyst", []ToolGrant{{Tool: fqSQL, Policy: ToolResourcePolicy{AllowAll: true}}}); err != nil {
			t.Fatal(err)
		}
		if resp := fqCall(e, fqSQL); resp.Denied || resp.Error != "" {
			t.Fatalf("unrestricted=%v: an exact operator grant must allow it: %+v", unrestricted, resp)
		}
		if len(h.calls) != 1 || h.calls[0] != fqSQL {
			t.Fatalf("handler calls %v", h.calls)
		}
		if got := fqMenuNames(e.AvailableTools(ctx, "analyst")); !slices.Contains(got, fqSQL) {
			t.Fatalf("unrestricted=%v: granted free-SQL tool missing from the menu %v", unrestricted, got)
		}
	}
}

func TestFreeQuery_SkillOverlayDoesNotGrantIt(t *testing.T) {
	e, _ := fqExecutor(t, NewInMemoryGrantsStore(), false, false)
	granted, _ := e.ConferSkillGrants(context.Background(), "sess-1", "analyst", []string{fqSQL, fqTyped})
	if len(granted) == 0 {
		t.Fatal("precondition: the overlay should have conferred something")
	}
	if resp := fqCall(e, fqSQL); !resp.Denied || resp.DenyReason != FreeQueryDenyReason {
		t.Fatalf("a skill-conferred grant must not reach a free-SQL tool: %+v", resp)
	}
	if resp := fqCall(e, fqTyped); resp.Denied {
		t.Fatalf("the skill-conferred typed tool must work: %+v", resp)
	}
}

type fqFailingGrants struct{}

func (fqFailingGrants) GrantsFor(context.Context, string) ([]ToolGrant, error) {
	return nil, errors.New("grant store down")
}

func TestFreeQuery_GrantStoreErrorDenies(t *testing.T) {
	e, _ := fqExecutor(t, fqFailingGrants{}, true, false)
	if resp := fqCall(e, fqSQL); !resp.Denied {
		t.Fatalf("a grant-store error must deny a free-SQL call even under the bypass: %+v", resp)
	}
}

func TestFreeQuery_OperatorSystemExecutionAndOpenGuard(t *testing.T) {
	e, _ := fqExecutor(t, NewInMemoryGrantsStore(), false, false)
	sys := e.Execute(context.Background(), ToolCallRequest{ToolName: fqSQL, ArgsJSON: []byte(`{}`), System: true})
	if sys.Denied {
		t.Fatalf("an operator ScopeSystem execution is not an agent and is not guarded: %+v", sys)
	}
	open, _ := fqExecutor(t, NewInMemoryGrantsStore(), true, true)
	if resp := fqCall(open, fqSQL); resp.Denied {
		t.Fatalf("FreeQueryOpen: the bypass reaches it like any tool: %+v", resp)
	}
}

func TestToolAllowlist_ExpandGrantsFreeQueryNeedsALiteral(t *testing.T) {
	tools := fqRegistry().All()
	glob := ToolAllowlist{"mcp:erp-db/*"}
	if got := glob.ExpandGrants(tools, true); !slices.Equal(got, []string{fqTyped}) {
		t.Fatalf("guarded glob expansion = %v, want only the typed tool", got)
	}
	if got := glob.ExpandGrants(tools, false); !slices.Equal(got, []string{fqSQL, fqTyped}) {
		t.Fatalf("open glob expansion = %v", got)
	}
	literal := ToolAllowlist{"mcp:erp-db/list_*", fqSQL}
	if got := literal.ExpandGrants(tools, true); !slices.Equal(got, []string{fqSQL, fqTyped}) {
		t.Fatalf("a literal entry is an explicit grant: %v", got)
	}
}
