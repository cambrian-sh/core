package network

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// T-20261104-leases-minted-with-no-agentid-are-usable-by-any (ADR-0139 D8, fail closed):
// an agent-plane caller may use ONLY a lease bound to itself. A lease bound to no agent
// is refused to every agent on every lane, and stays usable by non-agent principals
// (operator, in-process kernel, MCP task lane). The E2E run that found this generated
// text with a forged agent principal on a live unbound lease captured from the operator
// feed; these tests are that attack, lane by lane, against the REAL gateway.

// textModel answers every stream with fixed text, so an accepted lease visibly generates.
type textModel struct{}

func (textModel) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 1)
	ch <- domain.StreamChunk{Text: "generated", IsFinal: true}
	close(ch)
	return ch, nil
}

// streamRecorder is the server side of GenerateViaModelStream: it carries the caller's
// ctx and records what was sent.
type streamRecorder struct {
	grpc.ServerStream
	ctx  context.Context
	text strings.Builder
}

func (r *streamRecorder) Context() context.Context { return r.ctx }
func (r *streamRecorder) Send(c *pb.GenerateChunk) error {
	r.text.WriteString(c.GetText())
	return nil
}

// unboundLeaseFixture mints a live lease bound to a session but to NO agent — what every
// unbound mint site produced — and, for contrast, one bound to agent-owner.
func unboundLeaseFixture(t *testing.T) (s *Server, gw *SubstrateLLMGateway, unbound, owned domain.LeaseID) {
	t.Helper()
	s, gw = newLeaseServer(t)
	useFakeModel(gw, "llm:fake", textModel{})
	gw.SetDefaultModelID("llm:fake")
	unbound, _ = gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(unbound, domain.LeaseBinding{SessionID: "victim-session", StepIndex: -1})
	owned, _ = gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(owned, domain.LeaseBinding{SessionID: "owner-session", StepIndex: -1, AgentID: "agent-owner"})
	return s, gw, unbound, owned
}

// agentCallers is every way an agent can present itself: an authenticated agent principal
// on the agent plane, the agent plane with no identity (ADR-0130 D2 waiver, no id), and an
// unsurfaced in-process call asserting x-agent-id.
func agentCallers(pairs ...string) map[string]context.Context {
	return map[string]context.Context{
		"agent principal":          agentPlaneCtx("e2e-forger", pairs...),
		"agent plane, no identity": agentPlaneCtx("", pairs...),
		"in-process x-agent-id":    ctxWithMD(append(append([]string{}, pairs...), "x-agent-id", "e2e-forger")...),
	}
}

func TestUnboundLease_RefusedToAgents_OnEveryResolutionLane(t *testing.T) {
	t.Parallel()
	s, _, unbound, _ := unboundLeaseFixture(t)

	for name, ctx := range agentCallers(leaseHeader, string(unbound)) {
		if b, ok := s.resolveLeaseFor(ctx, unbound); ok || !b.IsZero() {
			t.Errorf("%s: resolveLeaseFor accepted an unbound lease: %+v", name, b)
		}
		if !s.leaseForeignToCaller(ctx, unbound) {
			t.Errorf("%s: an unbound lease must read as foreign to an agent", name)
		}
		// QueryMemory / every RPC's session resolution.
		if sid := s.resolveCallerSession(ctx); sid != "" {
			t.Errorf("%s: an agent resolved an unbound lease to session %q", name, sid)
		}
		if got, ok := domain.SessionIDFromContext(s.withCallerSession(ctx)); ok && got != "" {
			t.Errorf("%s: withCallerSession seeded session %q from an unbound lease", name, got)
		}
		// ListTools overlay key.
		if tok := s.callerOverlayToken(ctx); tok != "" {
			t.Errorf("%s: ListTools overlay keyed by an unbound lease %q", name, tok)
		}
	}
	// Legacy header and the handoff-payload lane.
	for name, ctx := range agentCallers(sessionHeader, string(unbound)) {
		if sid := s.resolveCallerSession(ctx); sid != "" {
			t.Errorf("%s (legacy header): resolved %q", name, sid)
		}
	}
	if _, ok := s.resolveBindingFromHandoff(agentPlaneCtx("e2e-forger"),
		map[string]string{"_session_token_id": string(unbound)}); ok {
		t.Error("handoff payload: an agent resolved an unbound lease")
	}
}

func TestUnboundLease_StillResolvesForNonAgentPrincipals(t *testing.T) {
	t.Parallel()
	s, _, unbound, owned := unboundLeaseFixture(t)
	for name, ctx := range map[string]context.Context{
		"operator":         operatorPlaneCtx(leaseHeader, string(unbound)),
		"in-process":       ctxWithMD(leaseHeader, string(unbound)),
		"mcp machine":      domain.WithSurface(domain.WithPrincipal(ctxWithMD(leaseHeader, string(unbound)), domain.MachinePrincipal("worker-1")), domain.SurfaceRef{Kind: domain.SurfaceMCP}),
		"system principal": domain.WithPrincipal(ctxWithMD(leaseHeader, string(unbound)), domain.SystemPrincipal),
	} {
		if sid := s.resolveCallerSession(ctx); sid != "victim-session" {
			t.Errorf("%s: a non-agent principal must still resolve an unbound lease, got %q", name, sid)
		}
		if s.leaseForeignToCaller(ctx, unbound) {
			t.Errorf("%s: an unbound lease is not foreign to a non-agent principal", name)
		}
		if tok := s.callerOverlayToken(ctx); tok != string(unbound) {
			t.Errorf("%s: overlay token = %q, want the lease", name, tok)
		}
	}
	// The operator is unaffected for an agent-bound lease too.
	if sid := s.resolveCallerSession(operatorPlaneCtx(leaseHeader, string(owned))); sid != "owner-session" {
		t.Errorf("operator resolution of an agent-bound lease broke, got %q", sid)
	}
}

func TestBoundLease_OwnerStillResolves(t *testing.T) {
	t.Parallel()
	s, _, _, owned := unboundLeaseFixture(t)
	ctx := agentPlaneCtx("agent-owner", leaseHeader, string(owned))
	if sid := s.resolveCallerSession(ctx); sid != "owner-session" {
		t.Errorf("the owning agent must resolve its lease, got %q", sid)
	}
	if s.leaseForeignToCaller(ctx, owned) {
		t.Error("a lease is not foreign to its owner")
	}
	if tok := s.callerOverlayToken(ctx); tok != string(owned) {
		t.Errorf("owner overlay token = %q, want the lease", tok)
	}
}

// The E2E E10 attack itself: GenerateViaModelStream on a live unbound lease.
func TestGenerateViaModelStream_UnboundLeaseRefusedToAgent(t *testing.T) {
	t.Parallel()
	s, _, unbound, owned := unboundLeaseFixture(t)

	for name, ctx := range agentCallers() {
		rec := &streamRecorder{ctx: ctx}
		err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(unbound), Prompt: "hi"}, rec)
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: want PermissionDenied on an unbound lease, got %v", name, err)
		}
		if rec.text.Len() != 0 {
			t.Errorf("%s: generated %q on an unbound lease", name, rec.text.String())
		}
	}

	// The owner generates on its own lease; the operator on the unbound one.
	for name, tc := range map[string]struct {
		ctx   context.Context
		lease domain.LeaseID
	}{
		"owner":    {agentPlaneCtx("agent-owner"), owned},
		"operator": {operatorPlaneCtx(), unbound},
	} {
		rec := &streamRecorder{ctx: tc.ctx}
		if err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(tc.lease), Prompt: "hi"}, rec); err != nil {
			t.Errorf("%s: stream failed: %v", name, err)
		}
		if !strings.Contains(rec.text.String(), "generated") {
			t.Errorf("%s: expected generated text, got %q", name, rec.text.String())
		}
	}
}

func TestGenerateWithTools_UnboundLeaseRefusedToAgent(t *testing.T) {
	t.Parallel()
	s, _, unbound, owned := unboundLeaseFixture(t)
	req := func(l domain.LeaseID) *pb.GenerateWithToolsRequest {
		return &pb.GenerateWithToolsRequest{LeaseId: string(l),
			Messages: []*pb.ModelMessageProto{{Role: "user", Content: "hi"}}}
	}
	for name, ctx := range agentCallers() {
		if _, err := s.GenerateWithTools(ctx, req(unbound)); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: want PermissionDenied on an unbound lease, got %v", name, err)
		}
	}
	// A lease bound to another agent is refused too (this lane had no check at all).
	if _, err := s.GenerateWithTools(agentPlaneCtx("e2e-forger"), req(owned)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another agent's lease: want PermissionDenied, got %v", err)
	}
	// The owner and the operator pass the ownership check; whatever the fake model then
	// answers, it is not a lease refusal.
	for name, tc := range map[string]struct {
		ctx   context.Context
		lease domain.LeaseID
	}{
		"owner":    {agentPlaneCtx("agent-owner"), owned},
		"operator": {operatorPlaneCtx(), unbound},
	} {
		if _, err := s.GenerateWithTools(tc.ctx, req(tc.lease)); status.Code(err) == codes.PermissionDenied {
			t.Errorf("%s: refused its lease: %v", name, err)
		}
	}
}

func TestExecuteTool_UnboundLeaseDeniedToAgent(t *testing.T) {
	t.Parallel()
	s, _, unbound, owned := unboundLeaseFixture(t)
	s.ToolExecutor = &domain.ToolExecutor{Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore()}

	resp, err := s.ExecuteTool(agentPlaneCtx("e2e-forger", "x-agent-id", "e2e-forger"),
		&pb.ExecuteToolRequest{ToolName: "anything", LeaseId: string(unbound)})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !resp.GetDenied() || !strings.Contains(resp.GetDenyReason(), "lease is not valid") {
		t.Errorf("an unbound lease must be denied as a lease refusal, got denied=%v reason=%q",
			resp.GetDenied(), resp.GetDenyReason())
	}

	// The owner is not refused on the lease (the unknown tool may still be refused for
	// its own reason).
	resp, err = s.ExecuteTool(agentPlaneCtx("agent-owner", "x-agent-id", "agent-owner"),
		&pb.ExecuteToolRequest{ToolName: "anything", LeaseId: string(owned)})
	if err == nil && strings.Contains(resp.GetDenyReason(), "lease is not valid") {
		t.Errorf("the owner's own lease was refused: %q", resp.GetDenyReason())
	}
}

// ENG-2171: GenerateViaModelStream with no lease at all is refused PermissionDenied,
// like the unbound and foreign leases above — not Unauthenticated, which the chat
// lane reads as "the AI service rejected our credentials".
func TestGenerateViaModelStream_NoLeaseIsPermissionDenied(t *testing.T) {
	t.Parallel()
	s, _, _, _ := unboundLeaseFixture(t)
	rec := &streamRecorder{ctx: agentPlaneCtx("agent-owner")}
	err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{Prompt: "hi"}, rec)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no lease: want PermissionDenied, got %v", err)
	}
	if rec.text.Len() != 0 {
		t.Errorf("generated %q with no lease", rec.text.String())
	}
}
