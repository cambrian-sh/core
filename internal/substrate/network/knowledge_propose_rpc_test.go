package network

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
)

// Orchestrator/ProposeKnowledge: the asserter is the lease's agent and the commit's
// beneficiary the lease's run principal — nothing in the request names either; a
// refusal is data; a child without a memory-write grant is refused.

type fakeProposer struct {
	by, forPerson domain.PrincipalRef
	prop          knowledgecommit.Proposal
	commit        bool
	link          knowledgecommit.LinkProposal
	err           error
}

func (f *fakeProposer) Propose(_ context.Context, by, forPerson domain.PrincipalRef, p knowledgecommit.Proposal, commit bool, _ string) (knowledgecommit.Result, error) {
	f.by, f.forPerson, f.prop, f.commit = by, forPerson, p, commit
	if f.err != nil {
		return knowledgecommit.Result{}, f.err
	}
	return knowledgecommit.Result{CandidateID: "ki_1", Inserted: true, ApprovalID: "ap", ApprovalState: domain.ApprovalPending}, nil
}

func (f *fakeProposer) ProposeLink(_ context.Context, by domain.PrincipalRef, p knowledgecommit.LinkProposal) (bool, error) {
	f.by, f.link = by, p
	return true, f.err
}

func knowledgeServer(t *testing.T) (*Server, *SubstrateLLMGateway, *fakeProposer) {
	t.Helper()
	s, gw := newLeaseServer(t)
	fp := &fakeProposer{}
	s.Knowledge = fp
	// The verb gate is exercised in proposal_verb_test.go; these tests run unrestricted.
	s.ToolExecutor = &domain.ToolExecutor{Unrestricted: true, Grants: domain.NewInMemoryGrantsStore()}
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	return s, gw, fp
}

func itemReq(lease domain.LeaseID) *pb.ProposeKnowledgeRequest {
	return &pb.ProposeKnowledgeRequest{LeaseId: string(lease), RequestCommit: true,
		Proposal: &pb.ProposeKnowledgeRequest_Item{Item: &pb.ProposedKnowledgeItem{Kind: "case", EntityId: "c1",
			Values: []*pb.KnowledgeValue{{Predicate: "status", Type: "text", Text: "closed"}, {Predicate: "n", Type: "number", Number: 3}}}}}
}

func TestProposeKnowledgeRPC_StampsTheLeaseIdentity(t *testing.T) {
	t.Parallel()
	s, gw, fp := knowledgeServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})
	resp, err := s.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(lease))
	if err != nil || !resp.GetAccepted() || resp.GetCandidateId() != "ki_1" || resp.GetApprovalId() != "ap" || resp.GetApprovalState() != "pending" {
		t.Fatalf("resp = %+v %v", resp, err)
	}
	if fp.by != domain.AgentPrincipal("planner") || fp.forPerson != domain.UserPrincipal("alice") || !fp.commit {
		t.Fatalf("identity = by %v for %v", fp.by, fp.forPerson)
	}
	if len(fp.prop.Values) != 2 || fp.prop.Values[1].Number != 3 || fp.prop.Kind != "case" {
		t.Fatalf("proposal = %+v", fp.prop)
	}
	// Another agent presenting planner's lease is refused before anything is written.
	fp.by = domain.PrincipalRef{}
	if _, err := s.ProposeKnowledge(agentPlaneCtx("intruder"), itemReq(lease)); status.Code(err) != codes.PermissionDenied || !fp.by.IsZero() {
		t.Fatalf("foreign lease: %v (wrote as %v)", err, fp.by)
	}
}

func TestProposeKnowledgeRPC_RefusalsAreData(t *testing.T) {
	t.Parallel()
	s, gw, fp := knowledgeServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner"})
	fp.err = fmt.Errorf("kind %q lifecycle: transition refused: %w", "case", domain.ErrKindRefused)
	resp, err := s.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(lease))
	if err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "transition refused") {
		t.Fatalf("refusal: %+v %v", resp, err)
	}
	fp.err = fmt.Errorf("db down")
	if _, err := s.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(lease)); status.Code(err) != codes.Unavailable {
		t.Fatalf("infra failure: %v", err)
	}
	fp.err = nil
	// A link proposal cannot ask for a commit; a plain one passes through.
	linkReq := &pb.ProposeKnowledgeRequest{LeaseId: string(lease), RequestCommit: true,
		Proposal: &pb.ProposeKnowledgeRequest_Link{Link: &pb.ProposedKnowledgeLink{Relation: "about", FromRef: "entity:a", ToRef: "entity:b", EvidenceId: "ev"}}}
	if resp, _ := s.ProposeKnowledge(agentPlaneCtx("planner"), linkReq); resp.GetAccepted() {
		t.Fatal("a link proposal must not request a commit")
	}
	linkReq.RequestCommit = false
	if resp, err := s.ProposeKnowledge(agentPlaneCtx("planner"), linkReq); err != nil || !resp.GetAccepted() || fp.link.Relation != "about" {
		t.Fatalf("link: %+v %v", resp, err)
	}
	// A bad date value is a refusal, not a crash.
	bad := itemReq(lease)
	bad.GetItem().Values = []*pb.KnowledgeValue{{Predicate: "due", Type: "date"}}
	if resp, err := s.ProposeKnowledge(agentPlaneCtx("planner"), bad); err != nil || resp.GetAccepted() {
		t.Fatalf("bad date: %+v %v", resp, err)
	}
}

func TestProposeKnowledgeRPC_NilIsUnimplemented(t *testing.T) {
	t.Parallel()
	s, gw, _ := knowledgeServer(t)
	parent := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner"})
	s.Knowledge = nil
	if _, err := s.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(parent)); status.Code(err) != codes.Unimplemented {
		t.Fatalf("nil bridge: %v", err)
	}
}
