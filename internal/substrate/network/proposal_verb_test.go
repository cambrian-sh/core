package network

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// The kernel enforces the agent's opt-in to the proposal verbs (owner option A,
// T-20261113-agent-proposal-rpcs-proposeknowledge): restricted tool grants and
// no grant → PermissionDenied naming the verb, before anything is proposed; a
// grant of the verb → allowed; no tool executor → refused (fail closed).

func TestProposalRPCs_RefusedWithoutTheVerbGrant(t *testing.T) {
	t.Parallel()
	grants := domain.NewInMemoryGrantsStore()

	ks, kgw, fp := knowledgeServer(t)
	ks.ToolExecutor = &domain.ToolExecutor{Grants: grants}
	klease := bindNotesLease(t, kgw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})

	ds, dgw, fd := draftServer(t)
	ds.ToolExecutor = &domain.ToolExecutor{Grants: grants}
	dlease := bindNotesLease(t, dgw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})
	draftReq := &pb.ProposeOntologyChangeRequest{LeaseId: string(dlease), ChangeJson: invoiceChangeJSON}

	_, err := ks.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(klease))
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), domain.VerbProposeKnowledge) || !fp.by.IsZero() {
		t.Fatalf("knowledge without grant: %v (proposed as %v)", err, fp.by)
	}
	_, err = ds.ProposeOntologyChange(agentPlaneCtx("planner"), draftReq)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), domain.VerbProposeOntologyChange) || !fd.by.IsZero() {
		t.Fatalf("ontology without grant: %v (drafted as %v)", err, fd.by)
	}

	// One verb's grant does not open the other.
	grants.Set("planner", []domain.ToolGrant{{Tool: domain.VerbProposeKnowledge}})
	if resp, err := ks.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(klease)); err != nil || !resp.GetAccepted() {
		t.Fatalf("knowledge with grant: %+v %v", resp, err)
	}
	if _, err := ds.ProposeOntologyChange(agentPlaneCtx("planner"), draftReq); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the knowledge grant opened ontology drafts: %v", err)
	}
	grants.Set("planner", []domain.ToolGrant{{Tool: domain.VerbProposeKnowledge}, {Tool: domain.VerbProposeOntologyChange}})
	if resp, err := ds.ProposeOntologyChange(agentPlaneCtx("planner"), draftReq); err != nil || !resp.GetAccepted() {
		t.Fatalf("ontology with grant: %+v %v", resp, err)
	}

	// A kernel with no tool executor refuses (fail closed).
	ks.ToolExecutor = nil
	if _, err := ks.ProposeKnowledge(agentPlaneCtx("planner"), itemReq(klease)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no executor: %v", err)
	}
}
