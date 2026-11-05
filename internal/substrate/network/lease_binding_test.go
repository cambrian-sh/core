package network

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 D8: leases are 128-bit crypto/rand credentials bound to the issuing agent,
// and the raw x-session-id fallback is closed to agent-plane callers.

func agentPlaneCtx(agentID string, pairs ...string) context.Context {
	ctx := domain.WithSurface(ctxWithMD(pairs...), domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
	if agentID != "" {
		ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal(agentID))
	}
	return ctx
}

func operatorPlaneCtx(pairs ...string) context.Context {
	ctx := domain.WithSurface(ctxWithMD(pairs...), domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"})
	return domain.WithPrincipal(ctx, domain.UserPrincipal("alice"))
}

func TestLeaseIDs_AreLongUniqueAndRandom(t *testing.T) {
	t.Parallel()
	_, gw := newLeaseServer(t)
	const n = 2000
	seen := make(map[domain.LeaseID]bool, n)
	for i := 0; i < n; i++ {
		id, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 16, time.Second)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		s := string(id)
		if !strings.HasPrefix(s, "lease-") {
			t.Fatalf("lease id %q lost its prefix", s)
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(s, "lease-"))
		if err != nil || len(raw) != 16 {
			t.Fatalf("lease id %q is not 128 bits of hex (err=%v, bytes=%d)", s, err, len(raw))
		}
		if seen[id] {
			t.Fatalf("duplicate lease id %q after %d mints", s, i)
		}
		seen[id] = true
	}
}

func TestLeaseOfAgentA_PresentedByAgentB_YieldsNoBinding(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "victim-session", AgentID: "agent-a", ConversationID: "conv-a"})

	// Every resolution lane refuses agent B.
	for name, ctx := range map[string]context.Context{
		"lease header":             agentPlaneCtx("agent-b", leaseHeader, string(lease)),
		"legacy header":            agentPlaneCtx("agent-b", sessionHeader, string(lease)),
		"x-agent-id only":          ctxWithMD(leaseHeader, string(lease), "x-agent-id", "agent-b"),
		"agent plane, no identity": agentPlaneCtx("", leaseHeader, string(lease)),
	} {
		if b, ok := s.resolveCallerBinding(ctx); ok || !b.IsZero() {
			t.Errorf("%s: agent B got agent A's binding %+v", name, b)
		}
		if sid := s.resolveCallerSession(ctx); sid != "" {
			t.Errorf("%s: agent B resolved agent A's session %q", name, sid)
		}
		if conv := s.resolveCallerConversation(ctx); conv != "" {
			t.Errorf("%s: agent B resolved agent A's conversation %q", name, conv)
		}
		if !s.leaseForeignToCaller(ctx, lease) {
			t.Errorf("%s: lease must read as foreign to agent B", name)
		}
	}
	// Handoff payload lane.
	if _, ok := s.resolveBindingFromHandoff(agentPlaneCtx("agent-b"), map[string]string{"_session_token_id": string(lease)}); ok {
		t.Error("handoff payload: agent B got agent A's binding")
	}

	// Agent A itself still resolves it.
	ctxA := agentPlaneCtx("agent-a", leaseHeader, string(lease))
	if sid := s.resolveCallerSession(ctxA); sid != "victim-session" {
		t.Errorf("the issuing agent must resolve its own lease, got %q", sid)
	}
	if s.leaseForeignToCaller(ctxA, lease) {
		t.Error("a lease is not foreign to the agent it was issued to")
	}
}

// A lease bound to no agent is refused to every agent (ADR-0139 D8, fail closed;
// T-20261104-leases-minted-with-no-agentid-are-usable-by-any). It used to resolve for
// any agent; lease_unbound_test.go covers every lane.
func TestUnboundAgentLease_RefusedToAgents(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "step-session", StepIndex: 2})
	if sid := s.resolveCallerSession(agentPlaneCtx("whoever", leaseHeader, string(lease))); sid != "" {
		t.Errorf("a lease with no AgentID must not resolve for an agent, got %q", sid)
	}
}

// The operator is not an agent: an agent-bound lease still resolves for it.
func TestOperatorPresentingAgentLease_Resolves(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", AgentID: "agent-a"})
	if sid := s.resolveCallerSession(operatorPlaneCtx(leaseHeader, string(lease))); sid != "s1" {
		t.Errorf("operator lease resolution broke, got %q", sid)
	}
}

func TestAgentPlaneRawSessionID_YieldsEmpty(t *testing.T) {
	t.Parallel()
	s, _ := newLeaseServer(t)
	for name, ctx := range map[string]context.Context{
		"agent surface + principal":    agentPlaneCtx("agent-b", sessionHeader, "someone-elses-session"),
		"agent surface, waived, no id": agentPlaneCtx("", sessionHeader, "someone-elses-session"),
		"in-process with x-agent-id":   ctxWithMD(sessionHeader, "someone-elses-session", "x-agent-id", "agent-b"),
		"chat surface": domain.WithSurface(ctxWithMD(sessionHeader, "someone-elses-session"),
			domain.SurfaceRef{Kind: domain.SurfaceChat}),
	} {
		if sid := s.resolveCallerSession(ctx); sid != "" {
			t.Errorf("%s: raw x-session-id must yield no session for an agent, got %q", name, sid)
		}
	}
}

func TestOperatorRawSessionID_StillPassesThrough(t *testing.T) {
	t.Parallel()
	s, _ := newLeaseServer(t)
	for name, ctx := range map[string]context.Context{
		"operator surface": operatorPlaneCtx(sessionHeader, "real-session"),
		// app.go SendFn / planSubmitter: in-process, context.Background().
		"in-process operator send": ctxWithMD(sessionHeader, "real-session"),
		// The MCP task lane builds x-session-id itself.
		"mcp task lane": domain.WithSurface(
			domain.WithPrincipal(ctxWithMD(sessionHeader, "real-session"), domain.AgentPrincipal("bound-client-owner")),
			domain.SurfaceRef{Kind: domain.SurfaceMCP, ID: "cambrian"}),
	} {
		if sid := s.resolveCallerSession(ctx); sid != "real-session" {
			t.Errorf("%s: operator session id must pass through, got %q", name, sid)
		}
	}
}

// ExecuteTool refuses a stolen lease before the executor sees it, so no skill grant,
// evaluation-lease approval bypass or spend is reachable through it.
func TestExecuteTool_ForeignLeaseIsDenied(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ToolExecutor = &domain.ToolExecutor{Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore()}
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", AgentID: "agent-a"})

	resp, err := s.ExecuteTool(agentPlaneCtx("agent-b", "x-agent-id", "agent-b"),
		&pb.ExecuteToolRequest{ToolName: "anything", LeaseId: string(lease)})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !resp.GetDenied() || !strings.Contains(resp.GetDenyReason(), "lease") {
		t.Errorf("a foreign lease must be denied, got denied=%v reason=%q", resp.GetDenied(), resp.GetDenyReason())
	}
}
