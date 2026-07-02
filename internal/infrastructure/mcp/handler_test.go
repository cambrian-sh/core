package mcp

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type echoIn struct {
	Value string `json:"value"`
}

type echoOut struct {
	Echo string `json:"echo"`
}

// staticSource is a test SessionSource returning a fixed session per server id.
type staticSource map[string]*mcpsdk.ClientSession

func (s staticSource) Session(id string) (*mcpsdk.ClientSession, bool) {
	sess, ok := s[id]
	return sess, ok
}

// stubEchoSession stands up an in-process MCP server advertising one `echo` tool
// and returns a connected client session — hermetic, no subprocess or network
// (uses the SDK's in-memory transport, the analogue of the Firecrawl httptest stub).
func stubEchoSession(t *testing.T, ctx context.Context) *mcpsdk.ClientSession {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stub", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "echo", Description: "echo the value"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Value}, nil
		})
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// Tracer: a granted agent's tool_call to mcp:stub/echo routes through the
// ToolExecutor → MCPHandler → in-process stub and returns the stub's result; an
// ungranted agent is denied, identically to any native system tool (ADR-0043 D1).
func TestMCPHandler_TracerThroughToolExecutor(t *testing.T) {
	ctx := context.Background()
	sess := stubEchoSession(t, ctx)
	h := &Handler{Sessions: staticSource{"stub": sess}}

	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:stub/echo"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("a", []domain.ToolGrant{{Tool: "mcp:stub/echo"}})
	exec := &domain.ToolExecutor{
		Registry:        reg,
		Grants:          grants,
		MCPHandler:      h,
		InlineThreshold: 1 << 20,
	}

	resp := exec.Execute(ctx, domain.ToolCallRequest{
		AgentID: "a", ToolName: "mcp:stub/echo", ArgsJSON: []byte(`{"value":"hi"}`),
	})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("granted MCP call failed: denied=%v err=%q", resp.Denied, resp.Error)
	}
	if !strings.Contains(string(resp.ResultJSON), "hi") {
		t.Errorf("result should echo the value, got %s", resp.ResultJSON)
	}

	denyResp := exec.Execute(ctx, domain.ToolCallRequest{
		AgentID: "b", ToolName: "mcp:stub/echo", ArgsJSON: []byte(`{"value":"hi"}`),
	})
	if !denyResp.Denied {
		t.Error("ungranted agent must be denied (same authorization as native tools)")
	}
}

// A call to an MCP server that is not connected degrades to a structured error
// (the loop can reason about it), never a panic (ADR-0043 D1 / D8 graceful).
func TestMCPHandler_UnconnectedServerDegradesToError(t *testing.T) {
	ctx := context.Background()
	h := &Handler{Sessions: staticSource{}} // no sessions wired

	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:down/tool"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("a", []domain.ToolGrant{{Tool: "mcp:down/tool"}})
	exec := &domain.ToolExecutor{Registry: reg, Grants: grants, MCPHandler: h, InlineThreshold: 1 << 20}

	resp := exec.Execute(ctx, domain.ToolCallRequest{AgentID: "a", ToolName: "mcp:down/tool"})
	if resp.Error == "" {
		t.Fatal("a call to an unconnected MCP server must return a structured error, not crash")
	}
	if !strings.Contains(resp.Error, "not connected") {
		t.Errorf("error should explain the server is not connected, got %q", resp.Error)
	}
	if resp.Denied {
		t.Error("an unconnected server is an error, not an authorization denial")
	}
}

// A slow MCP tool is cut off by the per-call timeout and returns an error rather
// than hanging the agent (ADR-0043 D8).
func TestMCPHandler_CallTimeout(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "slow", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "slow", Description: "slow"},
		func(c context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			select {
			case <-time.After(2 * time.Second):
				return nil, echoOut{Echo: in.Value}, nil
			case <-c.Done():
				return nil, echoOut{}, c.Err()
			}
		})
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	h := &Handler{Sessions: staticSource{"slow": sess}, CallTimeout: 50 * time.Millisecond}
	start := time.Now()
	_, err = h.Execute(ctx, domain.ToolCall{ToolName: "mcp:slow/slow", ArgsJSON: []byte(`{"value":"x"}`)})
	if err == nil {
		t.Error("a call exceeding CallTimeout must return an error")
	}
	if time.Since(start) > time.Second {
		t.Error("the timeout did not cut the call off promptly")
	}
}

// capturingAuditor records egress events for assertions.
type capturingAuditor struct{ calls []string }

func (c *capturingAuditor) RecordEgress(agentID, toolName string, _ []string) {
	c.calls = append(c.calls, agentID+"|"+toolName)
}

// A remote MCP call emits an egress audit event (ADR-0043 D4) — the forensic
// record that the agent's args left the trust boundary.
func TestMCPHandler_EmitsEgressAudit(t *testing.T) {
	ctx := context.Background()
	sess := stubEchoSession(t, ctx)
	aud := &capturingAuditor{}

	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:stub/echo"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("a", []domain.ToolGrant{{Tool: "mcp:stub/echo"}})
	exec := &domain.ToolExecutor{
		Registry: reg, Grants: grants,
		MCPHandler:      &Handler{Sessions: staticSource{"stub": sess}},
		EgressAuditor:   aud,
		InlineThreshold: 1 << 20,
	}

	exec.Execute(ctx, domain.ToolCallRequest{
		AgentID: "a", ToolName: "mcp:stub/echo", ArgsJSON: []byte(`{"value":"hi"}`),
	})
	if len(aud.calls) != 1 || aud.calls[0] != "a|mcp:stub/echo" {
		t.Errorf("expected one egress audit for a|mcp:stub/echo, got %v", aud.calls)
	}
}

// fixedPricing is a test ToolPricingSource.
type fixedPricing map[string]domain.ToolPricing

func (f fixedPricing) PricingFor(name string) (domain.ToolPricing, bool) {
	p, ok := f[name]
	return p, ok
}

func budgetedExec(t *testing.T, ctx context.Context, cap float64) *domain.ToolExecutor {
	t.Helper()
	sess := stubEchoSession(t, ctx)
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:stub/echo"})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("a", []domain.ToolGrant{{Tool: "mcp:stub/echo"}})
	ledger := domain.NewBudgetLedger()
	ledger.SetCap("mcp:tok-1", cap)
	return &domain.ToolExecutor{
		Registry:        reg,
		Grants:          grants,
		MCPHandler:      &Handler{Sessions: staticSource{"stub": sess}},
		Budget:          ledger,
		Pricing:         fixedPricing{"mcp:stub/echo": {Kind: domain.PricingFlat, UnitCost: 0.03}},
		InlineThreshold: 1 << 20,
	}
}

// A priced MCP call reserves on admission and reconciles the actual cost to the
// session ledger (ADR-0043 D5/D6).
func TestMCPHandler_BudgetedCallReservesAndReconciles(t *testing.T) {
	ctx := context.Background()
	exec := budgetedExec(t, ctx, 0.10)

	for i := range 2 {
		resp := exec.Execute(ctx, domain.ToolCallRequest{
			AgentID: "a", ToolName: "mcp:stub/echo", SessionTokenID: "tok-1",
			ArgsJSON: []byte(`{"value":"hi"}`),
		})
		if resp.Denied || resp.Error != "" {
			t.Fatalf("call %d within budget failed: denied=%v err=%q", i, resp.Denied, resp.Error)
		}
	}
	// Money is float; compare with a tolerance (0.10 − 0.03 − 0.03 ≈ 0.04).
	if got := exec.Budget.Remaining("mcp:tok-1"); math.Abs(got-0.04) > 1e-9 {
		t.Errorf("after 2×0.03 reconciled charges, remaining = %v, want ~0.04", got)
	}
}

// A call whose reservation exceeds the remaining budget is denied budget_exhausted
// before dispatch (ADR-0043 D5).
func TestMCPHandler_OverBudgetCallDenied(t *testing.T) {
	ctx := context.Background()
	exec := budgetedExec(t, ctx, 0.02) // cap below the 0.03 price

	resp := exec.Execute(ctx, domain.ToolCallRequest{
		AgentID: "a", ToolName: "mcp:stub/echo", SessionTokenID: "tok-1",
		ArgsJSON: []byte(`{"value":"hi"}`),
	})
	if !resp.Denied {
		t.Fatal("a call whose reservation exceeds budget must be denied")
	}
	if !strings.Contains(resp.DenyReason, "budget_exhausted") {
		t.Errorf("deny reason should be budget_exhausted, got %q", resp.DenyReason)
	}
}
