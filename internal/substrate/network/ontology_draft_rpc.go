package network

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/ontology"
)

// Orchestrator/ProposeOntologyChange (ADR-0149 D2;
// T-20261113-agent-drafted-ontology-changes-have-no-approval): the agent plane's
// door to the ontology declarations, capped at a DRAFT. The drafter is the agent
// the presented lease is bound to and the approval's beneficiary is the lease's
// run principal (leaseProposer) — the request carries no actor field to forge.
// Applying is a person's act: the ontology_change approval's handler applies the
// draft as its approver. A refusal the model can act on (breaking, invalid,
// empty, already decided) is an OK response with accepted=false and the detail;
// infrastructure failures are UNAVAILABLE.

// OntologyDraftProposer is the bridge the RPC drives (ontology.Drafts).
type OntologyDraftProposer interface {
	Propose(ctx context.Context, by, forPerson domain.PrincipalRef, p ontology.DraftProposal) (ontology.DraftResult, error)
}

// ProposeOntologyChange implements the RPC.
func (s *Server) ProposeOntologyChange(ctx context.Context, req *pb.ProposeOntologyChangeRequest) (*pb.ProposeOntologyChangeResponse, error) {
	if s.OntologyDrafts == nil {
		return nil, status.Error(codes.Unimplemented, "ontology drafts are not enabled on this kernel")
	}
	_, by, forPerson, err := s.leaseProposer(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if err := s.requireProposalVerb(ctx, by, req.GetLeaseId(), domain.VerbProposeOntologyChange); err != nil {
		return nil, err
	}
	change, err := domain.ParseOntologyChange(req.GetChangeJson())
	if err != nil {
		return &pb.ProposeOntologyChangeResponse{Accepted: false, Detail: err.Error()}, nil
	}
	res, err := s.OntologyDrafts.Propose(ctx, by, forPerson, ontology.DraftProposal{
		Namespace: req.GetNamespace(), Change: change, Reason: req.GetReason(), ClientRef: req.GetClientRef()})
	if err != nil && !errors.Is(err, domain.ErrOntologyDraftRefused) {
		slog.Warn("ontology draft failed", "agent", by.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "ontology drafts are unavailable")
	}
	out := draftResponse(res)
	if err != nil {
		out.Accepted, out.Detail = false, err.Error()
	}
	return out, nil
}

func draftResponse(res ontology.DraftResult) *pb.ProposeOntologyChangeResponse {
	d := res.Draft
	out := &pb.ProposeOntologyChangeResponse{Accepted: true, DraftId: d.ID, Created: res.Created,
		BaseVersion: d.BaseVersion, Class: string(d.Class), State: string(d.State), Detail: d.Detail,
		ApprovalId: d.ApprovalID, ApprovalState: string(res.ApprovalState)}
	for _, c := range d.Changes {
		out.Changes = append(out.Changes, &pb.OntologyDraftChange{Class: string(c.Class), Subject: c.Subject, Detail: c.Detail})
	}
	return out
}
