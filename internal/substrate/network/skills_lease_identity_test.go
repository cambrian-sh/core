package network

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// leaseKeyedAuthorizer stands in for the premium decision point's ENG-518 rule
// (core may not import premium): an agent-plane read with no task session, no
// conversation owner and no ordering conversation on ctx is refused no_lease.
// It records what the kernel put on ctx for each ReadFilter.
type leaseKeyedAuthorizer struct {
	domain.AllowAllAuthorizer
	sessions []domain.SessionID
	owners   []string
	resolved []domain.LeaseID
	denied   int
}

func (a *leaseKeyedAuthorizer) ReadFilter(ctx context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	sid, _ := domain.SessionIDFromContext(ctx)
	lease, _ := domain.ResolvedLeaseFromContext(ctx)
	owner := domain.ConversationOwnerFromContext(ctx)
	a.sessions = append(a.sessions, sid)
	a.owners = append(a.owners, owner)
	a.resolved = append(a.resolved, lease)
	if p.Kind == domain.PrincipalAgent && sid == "" && owner == "" && domain.ConversationIDFromContext(ctx) == "" {
		a.denied++
		return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonNoLease}
	}
	return a.AllowAllAuthorizer.ReadFilter(ctx, p, s)
}

func leaseSkillServer(t *testing.T) (*Server, *SubstrateLLMGateway, *leaseKeyedAuthorizer) {
	t.Helper()
	s, gw := newLeaseServer(t)
	reg := domain.NewInMemorySkillRegistry()
	reg.Register(domain.Skill{Name: "summarise", Description: "Does summarise.", Instructions: "BODY"})
	s.SkillRegistry = reg
	az := &leaseKeyedAuthorizer{}
	s.Authz = az
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		if b.ConversationID == "conv-of-dana" {
			return "dana"
		}
		return ""
	}
	return s, gw, az
}

func listSkillsAs(t *testing.T, s *Server, agent string, kv ...string) []*pb.SkillDescriptor {
	t.Helper()
	md := metadata.Pairs(append([]string{"x-agent-id", agent}, kv...)...)
	ctx := domain.WithSurface(metadata.NewIncomingContext(context.Background(), md),
		domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
	resp, err := s.ListSkills(domain.WithPrincipal(ctx, domain.AgentPrincipal(agent)), &pb.ListSkillsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetSkills()
}

// T-20261118-chat-agent-and-composed-worker-memory-reads-are: a chat turn's
// skill menu presented with its lease resolves the lease to the turn's
// identity (conversation owner, resolved-lease marker) on the decision point's
// ctx, so it is not refused no_lease; a plan step's lease resolves to its task
// session.
func TestListSkills_ResolvesTheCallersLeaseIdentity(t *testing.T) {
	t.Parallel()
	s, gw, az := leaseSkillServer(t)
	turn := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	if got := listSkillsAs(t, s, "chat_agent", leaseHeader, string(turn)); len(got) != 1 {
		t.Fatalf("chat turn menu = %d skills, want 1 (denied %d)", len(got), az.denied)
	}
	if az.owners[0] != "dana" || az.resolved[0] != turn {
		t.Fatalf("decision ctx: owner %q resolved %q, want dana / %s", az.owners[0], az.resolved[0], turn)
	}

	step := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "composed_worker", SessionID: "sess-7", RunID: "run-7"})
	if got := listSkillsAs(t, s, "composed_worker", leaseHeader, string(step)); len(got) != 1 {
		t.Fatalf("step menu = %d skills, want 1", len(got))
	}
	if az.sessions[1] != "sess-7" || az.resolved[1] != step {
		t.Fatalf("decision ctx: session %q resolved %q, want sess-7 / %s", az.sessions[1], az.resolved[1], step)
	}
}

// An older SDK sends the lease only as x-session-token (a use_skill load): it is
// taken as the presented lease when no x-lease-id is sent.
func TestListSkills_LegacySkillLoadLeaseResolves(t *testing.T) {
	t.Parallel()
	s, gw, az := leaseSkillServer(t)
	turn := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	if got := listSkillsAs(t, s, "chat_agent", skillLoadLeaseHeader, string(turn)); len(got) != 1 || az.owners[0] != "dana" {
		t.Fatalf("legacy load: %d skills, owner %q", len(got), az.owners[0])
	}
}

// The no_lease refusal is untouched: no lease, a foreign agent's lease or a
// garbage value still yields an empty menu with nothing resolved.
func TestListSkills_NoOrForeignLeaseStillRefused(t *testing.T) {
	t.Parallel()
	s, gw, az := leaseSkillServer(t)
	turn := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	cases := []struct {
		agent string
		kv    []string
	}{
		{"chat_agent", nil},
		{"intruder", []string{leaseHeader, string(turn)}},
		{"intruder", []string{skillLoadLeaseHeader, string(turn)}},
		{"chat_agent", []string{leaseHeader, "garbage"}},
	}
	for i, c := range cases {
		if got := listSkillsAs(t, s, c.agent, c.kv...); len(got) != 0 {
			t.Fatalf("case %d: menu served %d skills without a resolvable lease", i, len(got))
		}
		if az.resolved[i] != "" || az.owners[i] != "" {
			t.Fatalf("case %d: resolved %q owner %q", i, az.resolved[i], az.owners[i])
		}
	}
	if az.denied != len(cases) {
		t.Fatalf("denied %d, want %d", az.denied, len(cases))
	}
}
