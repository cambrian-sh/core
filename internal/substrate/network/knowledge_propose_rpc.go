package network

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
)

// Orchestrator/ProposeKnowledge (T-20261107-knowledge-substrate-extensions-for-case):
// the agent plane's write door into the knowledge substrate, capped at CANDIDATE.
//
// The asserter is the agent the presented lease is bound to, and the commit's
// beneficiary is the lease's run principal (RunPrincipal) — kernel-held facts, never
// request fields (INV-5). The request carries no actor field to forge. A refusal the
// model can act on (a named lifecycle constraint, a kind that takes no proposals, a
// link without evidence) is an OK response with accepted=false and the detail, the
// shape a tool result shows; infrastructure failures are UNAVAILABLE.

// KnowledgeProposer is the bridge the RPC drives (knowledgecommit.Service).
type KnowledgeProposer interface {
	Propose(ctx context.Context, by, forPerson domain.PrincipalRef, p knowledgecommit.Proposal, commit bool, reason string) (knowledgecommit.Result, error)
	ProposeLink(ctx context.Context, by domain.PrincipalRef, p knowledgecommit.LinkProposal) (bool, error)
	RequestCommit(ctx context.Context, by, forPerson domain.PrincipalRef, candidate domain.KnowledgeItemID, reason string) (domain.ApprovalItem, error)
}

// RequestKnowledgeCommit implements Orchestrator/RequestKnowledgeCommit (ENG-1768,
// owner decision 2026-11-20, option A): ask for an EXISTING candidate to be
// committed. Who asks and for whom are the lease's agent and run principal
// (leaseProposer), exactly as for ProposeKnowledge; the same grant
// (propose_knowledge) and the same Rule-of-Two gate apply. The candidate's
// readability by both is checked inside knowledgecommit.Service.RequestCommit, so
// this transport adds no second, divergent check.
func (s *Server) RequestKnowledgeCommit(ctx context.Context, req *pb.RequestKnowledgeCommitRequest) (*pb.RequestKnowledgeCommitResponse, error) {
	if s.Knowledge == nil {
		return nil, status.Error(codes.Unimplemented, "knowledge proposals are not enabled on this kernel")
	}
	b, by, forPerson, err := s.leaseProposer(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if err := s.requireProposalVerb(ctx, by, req.GetLeaseId(), domain.VerbProposeKnowledge); err != nil {
		return nil, err
	}
	if req.GetCandidateId() == "" {
		return &pb.RequestKnowledgeCommitResponse{Accepted: false, Detail: "candidate_id is required"}, nil
	}
	if why, ok := s.gateKernelWrite(ctx, req.GetLeaseId(), "propose_knowledge",
		"request a knowledge commit: "+previewText(req.GetCandidateId()+" "+req.GetReason(), 200),
		map[string]any{"candidate_id": req.GetCandidateId(), "reason": req.GetReason()}); !ok {
		return &pb.RequestKnowledgeCommitResponse{Accepted: false, Detail: why}, nil
	}
	if s.ConversationOwner != nil {
		if owner := s.ConversationOwner(ctx, b); owner != "" {
			ctx = domain.WithConversationOwner(ctx, owner)
		}
	}
	ap, err := s.Knowledge.RequestCommit(ctx, by, forPerson, domain.KnowledgeItemID(req.GetCandidateId()), req.GetReason())
	if refusal(err) {
		return &pb.RequestKnowledgeCommitResponse{Accepted: false, Detail: err.Error()}, nil
	}
	if err != nil {
		slog.Warn("knowledge commit request failed", "agent", by.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "knowledge commit requests are unavailable")
	}
	return &pb.RequestKnowledgeCommitResponse{Accepted: true, ApprovalId: ap.ID, ApprovalState: string(ap.State)}, nil
}

// ProposeKnowledge implements the RPC.
func (s *Server) ProposeKnowledge(ctx context.Context, req *pb.ProposeKnowledgeRequest) (*pb.ProposeKnowledgeResponse, error) {
	if s.Knowledge == nil {
		return nil, status.Error(codes.Unimplemented, "knowledge proposals are not enabled on this kernel")
	}
	b, by, forPerson, err := s.leaseProposer(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if err := s.requireProposalVerb(ctx, by, req.GetLeaseId(), domain.VerbProposeKnowledge); err != nil {
		return nil, err
	}
	agentID := by.ID
	// ENG-2247: the Rule of Two on a knowledge proposal (item or link) from a
	// tainted session; declined, nothing is proposed.
	if why, ok := s.gateKernelWrite(ctx, req.GetLeaseId(), "propose_knowledge",
		"propose knowledge: "+previewText(req.GetItem().String()+req.GetLink().String(), 200),
		map[string]any{"client_ref": req.GetClientRef(), "request_commit": req.GetRequestCommit(), "reason": req.GetReason()}); !ok {
		return &pb.ProposeKnowledgeResponse{Accepted: false, Detail: why}, nil
	}
	// What the agent writes for a conversation's owner is that owner's (ADR-0142 D12).
	if s.ConversationOwner != nil {
		if owner := s.ConversationOwner(ctx, b); owner != "" {
			ctx = domain.WithConversationOwner(ctx, owner)
			if s.PrivateByDefault {
				ctx = domain.WithPrivateWrites(ctx, owner)
			}
		}
	}

	switch p := req.GetProposal().(type) {
	case *pb.ProposeKnowledgeRequest_Item:
		prop, err := proposalFromPB(p.Item, req.GetClientRef())
		if err != nil {
			return &pb.ProposeKnowledgeResponse{Accepted: false, Detail: err.Error()}, nil
		}
		// ADR-0146 D5: owner tags are kernel-stamped (the D12 mark above), never
		// taken from the proposal.
		prop.Classification = domain.DropCallerOwnerTags(ctx, "ProposeKnowledge.classification", prop.Classification)
		res, err := s.Knowledge.Propose(ctx, by, forPerson, prop, req.GetRequestCommit(), req.GetReason())
		if refusal(err) {
			return &pb.ProposeKnowledgeResponse{Accepted: false, Detail: err.Error(), CandidateId: string(res.CandidateID)}, nil
		}
		if err != nil {
			slog.Warn("knowledge proposal failed", "agent", agentID, "err", err)
			return nil, status.Error(codes.Unavailable, "knowledge proposals are unavailable")
		}
		return &pb.ProposeKnowledgeResponse{
			Accepted: true, CandidateId: string(res.CandidateID), Created: res.Inserted,
			ApprovalId: res.ApprovalID, ApprovalState: string(res.ApprovalState),
		}, nil
	case *pb.ProposeKnowledgeRequest_Link:
		if req.GetRequestCommit() {
			return &pb.ProposeKnowledgeResponse{Accepted: false,
				Detail: "a link proposal cannot request a commit: a candidate link is confirmed by a person through the link review"}, nil
		}
		l := p.Link
		created, err := s.Knowledge.ProposeLink(ctx, by, knowledgecommit.LinkProposal{
			Family: l.GetFamily(), FromRef: l.GetFromRef(), ToRef: l.GetToRef(), Relation: l.GetRelation(),
			EvidenceID: domain.EvidenceID(l.GetEvidenceId()), Confidence: l.GetConfidence(), ClientRef: req.GetClientRef(),
		})
		if refusal(err) {
			return &pb.ProposeKnowledgeResponse{Accepted: false, Detail: err.Error()}, nil
		}
		if err != nil {
			slog.Warn("knowledge link proposal failed", "agent", agentID, "err", err)
			return nil, status.Error(codes.Unavailable, "knowledge proposals are unavailable")
		}
		return &pb.ProposeKnowledgeResponse{Accepted: true, Created: created}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "a proposal (item or link) is required")
}

// leaseProposer resolves who an agent-plane proposal is from and for: the agent
// the presented lease (else x-lease-id) is bound to, and the lease's run
// principal — kernel-held facts, never request fields (INV-5). Shared by every
// proposal RPC (ProposeKnowledge, ProposeOntologyChange) so they cannot disagree
// on who is asking.
func (s *Server) leaseProposer(ctx context.Context, leaseID string) (domain.LeaseBinding, domain.PrincipalRef, domain.PrincipalRef, error) {
	var none domain.PrincipalRef
	lease := domain.LeaseID(leaseID)
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, leaseHeader))
	}
	if lease == "" {
		return domain.LeaseBinding{}, none, none, status.Error(codes.PermissionDenied, "a lease is required")
	}
	b, ok := s.resolveLeaseFor(ctx, lease)
	if !ok {
		return b, none, none, status.Error(codes.PermissionDenied, "lease is missing, foreign or expired")
	}
	agentID := b.AgentID
	if agentID == "" {
		agentID, _ = callerAgentIdentity(ctx)
	}
	if agentID == "" {
		return b, none, none, status.Error(codes.PermissionDenied, "no agent identity is bound to this lease")
	}
	var forPerson domain.PrincipalRef
	if s.RunPrincipal != nil {
		if id := s.RunPrincipal(ctx, b); id != "" {
			forPerson = domain.UserPrincipal(id)
		}
	}
	return b, domain.AgentPrincipal(agentID), forPerson, nil
}

// requireProposalVerb refuses an agent the kernel has not allowed to use a
// proposal verb (owner decision 2026-11-13, option A;
// T-20261113-agent-proposal-rpcs-proposeknowledge): the per-agent tool-grant
// check under the verb's name (domain.ToolExecutor.AgentVerbAllowed). The SDK's
// allow_*_proposals flag only offers the verb to the model; this is the
// enforcement. Fails closed when the kernel has no tool executor.
func (s *Server) requireProposalVerb(ctx context.Context, by domain.PrincipalRef, leaseID, verb string) error {
	lease := leaseID
	if lease == "" {
		lease = mdValue(ctx, leaseHeader)
	}
	if s.ToolExecutor.AgentVerbAllowed(ctx, by.ID, verb, lease) {
		return nil
	}
	slog.Info("agent proposal refused: no grant", "event", "proposal_verb_refused", "agent", by.ID, "verb", verb)
	return status.Errorf(codes.PermissionDenied,
		"agent %q may not call %s: it needs an operator grant of %q (OperatorConsole/SetToolGrant) or a profile `tools:` entry naming it (or a kernel running tools_unrestricted)",
		by.ID, verb, verb)
}

// refusal reports whether err is a permanent, nameable refusal the model should see.
func refusal(err error) bool {
	return err != nil && (errors.Is(err, knowledgecommit.ErrNotProposable) || errors.Is(err, domain.ErrKindRefused) ||
		errors.Is(err, domain.ErrLinkRefused) || errors.Is(err, authz.ErrWriteDenied) || errors.Is(err, authz.ErrForgedAssertion))
}

func proposalFromPB(it *pb.ProposedKnowledgeItem, clientRef string) (knowledgecommit.Proposal, error) {
	p := knowledgecommit.Proposal{
		Kind: it.GetKind(), EntityID: it.GetEntityId(), Negation: it.GetNegation(),
		EvidenceID: domain.EvidenceID(it.GetEvidenceId()), Classification: it.GetClassification(),
		ClientRef: clientRef,
	}
	if it.GetValidFrom() != nil {
		p.ValidFrom = it.GetValidFrom().AsTime()
	}
	if it.GetValidTo() != nil {
		p.ValidTo = it.GetValidTo().AsTime()
	}
	for _, v := range it.GetValues() {
		sv := domain.StatementValue{Predicate: v.GetPredicate(), Type: v.GetType()}
		switch v.GetType() {
		case "text":
			sv.Text = v.GetText()
		case "number":
			sv.Number = v.GetNumber()
		case "date":
			if v.GetDate() == nil {
				return p, errors.New("value " + v.GetPredicate() + ": a date value needs date")
			}
			sv.Date = v.GetDate().AsTime()
		case "entity":
			sv.EntityRef = v.GetEntityRef()
		default:
			return p, errors.New("value " + v.GetPredicate() + ": type must be text|number|date|entity")
		}
		p.Values = append(p.Values, sv)
	}
	return p, nil
}
