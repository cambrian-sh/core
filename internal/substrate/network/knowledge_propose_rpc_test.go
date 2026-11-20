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
	candidate     domain.KnowledgeItemID
	reason        string
	err           error
}

func (f *fakeProposer) RequestCommit(_ context.Context, by, forPerson domain.PrincipalRef, candidate domain.KnowledgeItemID, reason string) (domain.ApprovalItem, error) {
	f.by, f.forPerson, f.candidate, f.reason = by, forPerson, candidate, reason
	if f.err != nil {
		return domain.ApprovalItem{}, f.err
	}
	return domain.ApprovalItem{ID: "ap-rc", State: domain.ApprovalPending, Object: string(candidate)}, nil
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
	s.ToolExecutor = newTestToolExecutor(t, domain.ToolExecutorControls{Unrestricted: true}, domain.ToolExecutor{})
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

// Orchestrator/RequestKnowledgeCommit (ENG-1768): who asks and for whom come from
// the lease; the candidate and the reason from the request; refusals are data.
func TestRequestKnowledgeCommitRPC_StampsTheLeaseIdentityAndRefusalsAreData(t *testing.T) {
	t.Parallel()
	s, gw, fp := knowledgeServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})
	req := &pb.RequestKnowledgeCommitRequest{LeaseId: string(lease), CandidateId: "ki_7", Reason: "confirm"}
	resp, err := s.RequestKnowledgeCommit(agentPlaneCtx("planner"), req)
	if err != nil || !resp.GetAccepted() || resp.GetApprovalId() != "ap-rc" || resp.GetApprovalState() != "pending" {
		t.Fatalf("resp = %+v %v", resp, err)
	}
	if fp.by != domain.AgentPrincipal("planner") || fp.forPerson != domain.UserPrincipal("alice") || fp.candidate != "ki_7" || fp.reason != "confirm" {
		t.Fatalf("call = by %v for %v candidate %q reason %q", fp.by, fp.forPerson, fp.candidate, fp.reason)
	}
	// A foreign lease is refused before the bridge is reached.
	fp.by = domain.PrincipalRef{}
	if _, err := s.RequestKnowledgeCommit(agentPlaneCtx("intruder"), req); status.Code(err) != codes.PermissionDenied || !fp.by.IsZero() {
		t.Fatalf("foreign lease: %v (reached as %v)", err, fp.by)
	}
	// No candidate: a refusal, the bridge untouched.
	if resp, err := s.RequestKnowledgeCommit(agentPlaneCtx("planner"), &pb.RequestKnowledgeCommitRequest{LeaseId: string(lease)}); err != nil || resp.GetAccepted() || !fp.by.IsZero() {
		t.Fatalf("no candidate: %+v %v", resp, err)
	}
	// The bridge's named refusal (an unreadable or unknown candidate) is data.
	fp.err = fmt.Errorf("%w: candidate \"ki_7\" is unknown or not readable by agent:planner", knowledgecommit.ErrNotProposable)
	if resp, err := s.RequestKnowledgeCommit(agentPlaneCtx("planner"), req); err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "not readable") {
		t.Fatalf("refusal: %+v %v", resp, err)
	}
	fp.err = fmt.Errorf("hub down")
	if _, err := s.RequestKnowledgeCommit(agentPlaneCtx("planner"), req); status.Code(err) != codes.Unavailable {
		t.Fatalf("infra failure: %v", err)
	}
	s.Knowledge = nil
	if _, err := s.RequestKnowledgeCommit(agentPlaneCtx("planner"), req); status.Code(err) != codes.Unimplemented {
		t.Fatalf("nil bridge: %v", err)
	}
}
