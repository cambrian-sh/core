package network

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// ENG-1615: the turn's RECORDED surface (ADR-0085 D7) reaches the decision point on
// every agent-plane lane of a chat turn, not only recall: ExecuteTool, the artifact
// reads and memory notes. A policy linked to chat:* must see a chat:telegram turn's
// call on that surface; the same call from a turn with no recorded chat surface is
// decided as the agent plane. Before, the artifact plane and memory notes asked on
// a fixed agent surface, so such a policy never saw them.

// chatSurfaceForbid stands in for a decision point holding one forbid linked to
// chat:* — premium's policy store does this with a surface link; the kernel's job,
// asserted here, is only to present the right surface.
type chatSurfaceForbid struct{ domain.AllowAllAuthorizer }

const chatForbidReason = "forbidden on chat surfaces"

func onChat(s domain.SurfaceRef) bool { return s.Kind == domain.SurfaceChat }

func (chatSurfaceForbid) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if onChat(s) {
		return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonForbiddenTag, Detail: chatForbidReason}
	}
	return &domain.TagPredicate{Bypass: true}, domain.AccessDecision{Allowed: true, Principal: p, Surface: s, Reason: domain.ReasonAllowed}
}

func (chatSurfaceForbid) Authorize(_ context.Context, r domain.AccessRequest) domain.AccessDecision {
	if onChat(r.Surface) {
		return domain.AccessDecision{Principal: r.Principal, Surface: r.Surface, Reason: domain.ReasonForbiddenTag, Detail: chatForbidReason}
	}
	return domain.AccessDecision{Allowed: true, Principal: r.Principal, Surface: r.Surface, Reason: domain.ReasonAllowed}
}

// surfaceLaneServer: a lease server whose conversations record their surface,
// conv-tg on chat:telegram and conv-console on nothing (the transport surface).
func surfaceLaneServer(t *testing.T) (*Server, domain.LeaseID, domain.LeaseID) {
	t.Helper()
	s, gw := newLeaseServer(t)
	s.ConvSurfaces = fixedConvSurfaces{"conv-tg": telegram}
	s.Authz = chatSurfaceForbid{}
	tg := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-tg"})
	plain := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-console"})
	return s, tg, plain
}

func TestSurfaceLanes_ExecuteToolIsDecidedOnTheConversationsSurface(t *testing.T) {
	t.Parallel()
	s, tg, plain := surfaceLaneServer(t)
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:files/read", Effects: []domain.ToolEffect{domain.EffectRead}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("chat_agent", []domain.ToolGrant{{Tool: "mcp:files/read", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	h := &staticCountingHandler{}
	s.ToolExecutor = newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Authz: chatSurfaceForbid{}},
		domain.ToolExecutor{Handler: h, MCPHandler: h})
	s.ExecCfg = config.DefaultConfig().Execution

	call := func(lease domain.LeaseID) *pb.ExecuteToolResponse {
		t.Helper()
		resp, err := s.ExecuteTool(agentPlaneCtx("chat_agent", "x-agent-id", "chat_agent"),
			&pb.ExecuteToolRequest{ToolName: "mcp:files/read", ArgsJson: `{}`, LeaseId: string(lease)})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := call(tg); !r.Denied || !strings.Contains(r.DenyReason, chatForbidReason) {
		t.Fatalf("chat:telegram turn's tool call: denied=%v reason=%q, want the chat:* forbid", r.Denied, r.DenyReason)
	}
	if r := call(plain); r.Denied {
		t.Fatalf("the same call outside a chat surface was denied: %q", r.DenyReason)
	}
}

func TestSurfaceLanes_ArtifactReadsAreDecidedOnTheConversationsSurface(t *testing.T) {
	t.Parallel()
	s, tg, plain := surfaceLaneServer(t)
	led, _ := newArtifactLedger(chatSurfaceForbid{})
	s.Ledger = led

	get := func(lease domain.LeaseID) error {
		_, err := s.GetArtifact(agentPlaneCtx("chat_agent", "x-agent-id", "chat_agent", leaseHeader, string(lease)),
			&pb.GetArtifactRequest{Hash: "led_absent"})
		return err
	}
	if err := get(tg); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("chat:telegram turn's artifact read: %v, want PermissionDenied from the chat:* forbid", err)
	}
	if err := get(plain); err != nil {
		t.Fatalf("the same read outside a chat surface was refused: %v", err)
	}
	if got := artifactSurface(context.Background()); got != (domain.SurfaceRef{Kind: domain.SurfaceAgent}) {
		t.Fatalf("no turn surface on ctx: %+v, want the agent plane", got)
	}
}

func TestSurfaceLanes_MemoryNotesAreDecidedOnTheConversationsSurface(t *testing.T) {
	t.Parallel()
	s, tg, plain := surfaceLaneServer(t)
	svc, err := memorynotes.New(memorynotes.NewMemStore(), memorynotes.ScannerFunc(func(string) (bool, string) { return false, "" }),
		chatSurfaceForbid{}, memorynotes.Config{PersonalBudgetChars: 100, OrgBudgetChars: 100})
	if err != nil {
		t.Fatal(err)
	}
	s.MemoryNotes = svc
	s.MemoryNotesPrincipal = func(context.Context, domain.LeaseBinding) string { return "alice" }
	ctx := agentPlaneCtx("chat_agent")

	propose := func(lease domain.LeaseID) *pb.ProposeMemoryNoteResponse {
		t.Helper()
		r, err := s.ProposeMemoryNote(ctx, &pb.ProposeMemoryNoteRequest{LeaseId: string(lease),
			Op: pb.MemoryNoteOp_MEMORY_NOTE_OP_APPEND, Section: "Prefs", Content: "terse", Reason: "said so"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := propose(tg); r.GetAccepted() || !strings.Contains(r.GetDetail(), "access policy denies") {
		t.Fatalf("chat:telegram turn's note write: %+v, want refused by the chat:* forbid", r)
	}
	if r := propose(plain); !r.GetAccepted() {
		t.Fatalf("the same write outside a chat surface was refused: %+v", r)
	}
	// The read: the chat turn sees no document; the plain turn sees the one written.
	g, err := s.GetMemoryNotes(ctx, &pb.GetMemoryNotesRequest{LeaseId: string(tg)})
	if err != nil || len(g.GetDocuments()) != 0 {
		t.Fatalf("chat:telegram turn's note read: %+v %v, want nothing under the chat:* forbid", g, err)
	}
	g, err = s.GetMemoryNotes(ctx, &pb.GetMemoryNotesRequest{LeaseId: string(plain)})
	if err != nil || len(g.GetDocuments()) != 1 {
		t.Fatalf("plain turn's note read: %+v %v", g, err)
	}
}
