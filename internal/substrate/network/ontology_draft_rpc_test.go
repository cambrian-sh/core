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
	"github.com/cambrian-sh/core/internal/ontology"
)

// Orchestrator/ProposeOntologyChange: the drafter is the lease's agent and the
// approval's beneficiary the lease's run principal — never request fields.

type fakeDrafter struct {
	by, forPerson domain.PrincipalRef
	prop          ontology.DraftProposal
	calls         int
	err           error
}

func (f *fakeDrafter) Propose(_ context.Context, by, forPerson domain.PrincipalRef, p ontology.DraftProposal) (ontology.DraftResult, error) {
	f.calls++
	f.by, f.forPerson, f.prop = by, forPerson, p
	res := ontology.DraftResult{Created: true, ApprovalState: domain.ApprovalPending, Draft: domain.OntologyDraft{
		ID: "od_1", Namespace: p.Namespace, BaseVersion: 3, Class: domain.ChangeAdditive, State: domain.OntologyDraftPending,
		ApprovalID: "apr-1", Changes: []domain.OntologyChange{{Class: domain.ChangeAdditive, Subject: "kind invoice", Detail: "new kind"}}}}
	return res, f.err
}

func draftServer(t *testing.T) (*Server, *SubstrateLLMGateway, *fakeDrafter) {
	t.Helper()
	s, gw := newLeaseServer(t)
	fd := &fakeDrafter{}
	s.OntologyDrafts = fd
	// The verb gate is exercised in proposal_verb_test.go; these tests run unrestricted.
	s.ToolExecutor = &domain.ToolExecutor{Unrestricted: true, Grants: domain.NewInMemoryGrantsStore()}
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	return s, gw, fd
}

const invoiceChangeJSON = `{"kinds":[{"kind":"invoice","predicates":{"amount":{"type":"number"}}}]}`

func TestProposeOntologyChangeRPC_StampsTheLeaseIdentity(t *testing.T) {
	t.Parallel()
	s, gw, fd := draftServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})
	resp, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), &pb.ProposeOntologyChangeRequest{
		LeaseId: string(lease), Namespace: "tenant-a", ChangeJson: invoiceChangeJSON, Reason: "billing", ClientRef: "r1"})
	if err != nil || !resp.GetAccepted() || resp.GetDraftId() != "od_1" || resp.GetApprovalId() != "apr-1" ||
		resp.GetApprovalState() != "pending" || resp.GetState() != "pending" || resp.GetClass() != "additive" ||
		resp.GetBaseVersion() != 3 || len(resp.GetChanges()) != 1 || resp.GetChanges()[0].GetSubject() != "kind invoice" {
		t.Fatalf("resp = %+v %v", resp, err)
	}
	if fd.by != domain.AgentPrincipal("planner") || fd.forPerson != domain.UserPrincipal("alice") {
		t.Fatalf("identity = by %v for %v", fd.by, fd.forPerson)
	}
	if fd.prop.Namespace != "tenant-a" || fd.prop.Reason != "billing" || fd.prop.ClientRef != "r1" ||
		len(fd.prop.Change.Kinds) != 1 || fd.prop.Change.Kinds[0].Kind != "invoice" {
		t.Fatalf("proposal = %+v", fd.prop)
	}
	// Another agent presenting planner's lease, or no lease, is refused before anything is drafted.
	fd.calls = 0
	if _, err := s.ProposeOntologyChange(agentPlaneCtx("intruder"), &pb.ProposeOntologyChangeRequest{
		LeaseId: string(lease), ChangeJson: invoiceChangeJSON}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign lease: %v", err)
	}
	if _, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), &pb.ProposeOntologyChangeRequest{
		ChangeJson: invoiceChangeJSON}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no lease: %v", err)
	}
	if fd.calls != 0 {
		t.Fatalf("a refused caller reached the drafter %d times", fd.calls)
	}
}

func TestProposeOntologyChangeRPC_RefusalsAreData(t *testing.T) {
	t.Parallel()
	s, gw, fd := draftServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner"})
	req := func(js string) *pb.ProposeOntologyChangeRequest {
		return &pb.ProposeOntologyChangeRequest{LeaseId: string(lease), ChangeJson: js}
	}
	// Strict JSON: a misspelt field never reaches the drafter.
	resp, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), req(`{"kinds":[{"kind":"x","predicates":{"n":{"type":"number","requried":true}}}]}`))
	if err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "requried") || fd.calls != 0 {
		t.Fatalf("strict: %+v %v (calls %d)", resp, err, fd.calls)
	}
	fd.err = fmt.Errorf("%w: ontology change refused: breaking changes need an explicit migration", domain.ErrOntologyDraftRefused)
	resp, err = s.ProposeOntologyChange(agentPlaneCtx("planner"), req(invoiceChangeJSON))
	if err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "breaking") {
		t.Fatalf("refusal: %+v %v", resp, err)
	}
	fd.err = fmt.Errorf("db down")
	if _, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), req(invoiceChangeJSON)); status.Code(err) != codes.Unavailable {
		t.Fatalf("infra failure: %v", err)
	}
	s.OntologyDrafts = nil
	if _, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), req(invoiceChangeJSON)); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no drafts: %v", err)
	}
}
