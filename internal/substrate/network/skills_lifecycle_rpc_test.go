package network

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

type fakeSkillLifecycle struct {
	proposed []lifecycle.ProposeInput
	uses     [][2]string
	outcomes map[string]bool
	result   lifecycle.ProposeResult
}

func (f *fakeSkillLifecycle) Propose(_ context.Context, in lifecycle.ProposeInput) (lifecycle.ProposeResult, error) {
	f.proposed = append(f.proposed, in)
	return f.result, nil
}
func (f *fakeSkillLifecycle) RecordUse(_ context.Context, name, runID string) error {
	f.uses = append(f.uses, [2]string{name, runID})
	return nil
}
func (f *fakeSkillLifecycle) RecordRunOutcome(_ context.Context, runID string, success bool) error {
	if f.outcomes == nil {
		f.outcomes = map[string]bool{}
	}
	f.outcomes[runID] = success
	return nil
}

func skillAgentCtx(agent string, md metadata.MD) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), md)
	return domain.WithPrincipal(ctx, domain.AgentPrincipal(agent))
}

func lifecycleServer(fake *fakeSkillLifecycle) *Server {
	return &Server{
		LLMGateway: &stubLeaseResolver{leaseID: "lease-1",
			binding: domain.LeaseBinding{AgentID: "agent-a", RunID: "run-1", SessionID: "sess-1", StepIndex: 0}},
		SkillLifecycle: fake,
	}
}

// The source run is the lease's, never the agent's word.
func TestProposeSkill_ProvenanceFromLease(t *testing.T) {
	fake := &fakeSkillLifecycle{result: lifecycle.ProposeResult{Accepted: true, ProposalID: "sp-1"}}
	s := lifecycleServer(fake)
	s.MemoryNotesPrincipal = func(_ context.Context, b domain.LeaseBinding) string { return "owner-of-" + string(b.SessionID) }
	resp, err := s.ProposeSkill(skillAgentCtx("agent-a", nil), &pb.ProposeSkillRequest{
		LeaseId: "lease-1", Name: "tidy", SkillMd: "---\nname: tidy\n---\nbody", Rationale: "r", BaseVersion: "",
	})
	if err != nil || !resp.GetAccepted() || resp.GetProposalId() != "sp-1" {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(fake.proposed) != 1 {
		t.Fatal("lifecycle not called")
	}
	in := fake.proposed[0]
	if in.AgentID != "agent-a" || in.RunID != "run-1" || in.SessionID != "sess-1" || in.PrincipalID != "owner-of-sess-1" {
		t.Fatalf("provenance = %+v", in)
	}
}

func TestProposeSkill_ForeignLeaseDenied(t *testing.T) {
	fake := &fakeSkillLifecycle{}
	s := lifecycleServer(fake)
	resp, err := s.ProposeSkill(skillAgentCtx("agent-b", nil), &pb.ProposeSkillRequest{LeaseId: "lease-1", Name: "tidy", SkillMd: "x"})
	if err != nil || resp.GetAccepted() || resp.GetRejection() != pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_DENIED {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(fake.proposed) != 0 {
		t.Fatal("a foreign lease reached the lifecycle")
	}
}

func TestProposeSkill_RejectionMappingAndShapeErrors(t *testing.T) {
	fake := &fakeSkillLifecycle{result: lifecycle.ProposeResult{Rejection: lifecycle.RejectLocked, Detail: "locked"}}
	s := lifecycleServer(fake)
	resp, _ := s.ProposeSkill(skillAgentCtx("agent-a", nil), &pb.ProposeSkillRequest{LeaseId: "lease-1", Name: "t", SkillMd: "x"})
	if resp.GetRejection() != pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_LOCKED || resp.GetAccepted() {
		t.Fatalf("resp = %v", resp)
	}
	if _, err := s.ProposeSkill(skillAgentCtx("agent-a", nil), &pb.ProposeSkillRequest{Name: "t", SkillMd: "x"}); status.Code(err) != codes.PermissionDenied { // ENG-2171
		t.Fatalf("missing lease: %v", err)
	}
	if _, err := (&Server{}).ProposeSkill(context.Background(), &pb.ProposeSkillRequest{LeaseId: "l"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no lifecycle: %v", err)
	}
	for r, want := range map[lifecycle.Rejection]pb.SkillProposalRejection{
		lifecycle.RejectInvalidFormat: pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_INVALID_FORMAT,
		lifecycle.RejectNameTaken:     pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_NAME_TAKEN,
		lifecycle.RejectScanDangerous: pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_SCAN_DANGEROUS,
		lifecycle.RejectQuota:         pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_QUOTA,
	} {
		if rejectionToProto[r] != want {
			t.Errorf("rejection %d maps to %v", r, rejectionToProto[r])
		}
	}
}

// use_skill (a Tier-2 named load) counts a use, attributed to the lease's run;
// the menu listing does not.
func TestListSkills_FullLoadRecordsUseWithRun(t *testing.T) {
	fake := &fakeSkillLifecycle{}
	s := lifecycleServer(fake)
	reg := domain.NewInMemorySkillRegistry()
	reg.Register(domain.Skill{Name: "tidy", Description: "Tidy notes.", Instructions: "Do it."})
	s.SkillRegistry = reg

	menu := skillAgentCtx("agent-a", metadata.Pairs("x-agent-id", "agent-a"))
	if _, err := s.ListSkills(menu, &pb.ListSkillsRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.uses) != 0 {
		t.Fatalf("a menu listing is not a use: %v", fake.uses)
	}
	load := skillAgentCtx("agent-a", metadata.Pairs("x-agent-id", "agent-a", "x-skill-names", "tidy", "x-skill-full", "true", "x-session-token", "lease-1"))
	resp, err := s.ListSkills(load, &pb.ListSkillsRequest{})
	if err != nil || len(resp.GetSkills()) != 1 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(fake.uses) != 1 || fake.uses[0] != [2]string{"tidy", "run-1"} {
		t.Fatalf("uses = %v", fake.uses)
	}
	s.recordSkillRunOutcome(context.Background(), "run-1", true)
	if !fake.outcomes["run-1"] {
		t.Fatalf("outcomes = %v", fake.outcomes)
	}
}
