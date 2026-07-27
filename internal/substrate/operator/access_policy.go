package operator

import (
	"context"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SetPolicyAdmin wires the access-policy administration surface (ADR-0085). It is
// nil in OSS — the RPCs then answer Unimplemented rather than pretending an
// unscoped deployment has a policy to explain.
func (s *Service) SetPolicyAdmin(a domain.PolicyAdmin) { s.policy = a }

// ExplainAccess answers "why can / can't this principal reach this resource?"
// without performing the access — the gpresult analogue (ADR-0085 D8).
//
// It is a READ: no mutation, no command_id, no audit entry beyond the normal
// operator access log. Asking the question must never be the thing that changes
// the answer.
func (s *Service) ExplainAccess(ctx context.Context, req *pb.ExplainAccessOpRequest) (*pb.ExplainAccessOpResponse, error) {
	if s.policy == nil {
		return nil, status.Error(codes.Unimplemented, "access policy not configured (unscoped deployment)")
	}
	if req.GetPrincipalId() == "" {
		return nil, status.Error(codes.InvalidArgument, "principal_id is required")
	}
	kind := domain.PrincipalKind(req.GetPrincipalKind())
	if kind == "" {
		kind = domain.PrincipalAgent
	}
	effects := make([]domain.ToolEffect, 0, len(req.GetEffects()))
	for _, e := range req.GetEffects() {
		eff := domain.ToolEffect(e)
		if !domain.ValidToolEffect(eff) {
			return nil, status.Errorf(codes.InvalidArgument, "unknown effect class %q", e)
		}
		effects = append(effects, eff)
	}
	dec := s.policy.ExplainAccess(ctx, domain.AccessRequest{
		Principal: domain.PrincipalRef{ID: req.GetPrincipalId(), Kind: kind},
		Surface:   domain.SurfaceRef{Kind: req.GetSurfaceKind(), ID: req.GetSurfaceId()},
		Resource:  domain.ResourceRef{Kind: domain.ResourceKind(req.GetResourceKind()), ID: req.GetResourceId()},
		Tags:      req.GetTags(),
		Effects:   effects,
	})
	return &pb.ExplainAccessOpResponse{Decision: AccessDecisionToOp(dec)}, nil
}

// ListClassificationTags returns the controlled vocabulary a policy UI selects
// from. A free-text tag field is a defect (ADR-0085 D11).
func (s *Service) ListClassificationTags(ctx context.Context, _ *pb.ListClassificationTagsOpRequest) (*pb.ListClassificationTagsOpResponse, error) {
	if s.policy == nil {
		return nil, status.Error(codes.Unimplemented, "access policy not configured (unscoped deployment)")
	}
	return &pb.ListClassificationTagsOpResponse{Tags: s.policy.Vocabulary(ctx)}, nil
}

// AccessDecisionToOp projects a decision onto the wire. It always carries the
// rendered `explain` sentence alongside the structured fields: a UI that only
// knows how to show a string still shows something an administrator can act on.
func AccessDecisionToOp(d domain.AccessDecision) *pb.AccessDecisionOp {
	out := &pb.AccessDecisionOp{
		Allowed:         d.Allowed,
		Reason:          string(d.Reason),
		Detail:          d.Detail,
		PolicyVersion:   d.PolicyVersion,
		ReportOnly:      d.ReportOnly,
		WouldHaveDenied: d.WouldHaveDenied,
		Explain:         d.Explain(),
	}
	for _, c := range d.DecidedBy {
		out.DecidedBy = append(out.DecidedBy, &pb.PolicyContributionOp{
			PolicyId:   c.PolicyID,
			PolicyName: c.PolicyName,
			LinkedAt:   c.LinkedAt,
			Term:       c.Term,
			Values:     c.Values,
			Enforced:   c.Enforced,
		})
	}
	return out
}

// policyNote projects a decision onto QueryMemoryResponse.policy_note, but ONLY
// when policy actually shaped the outcome. Annotating every response would train
// operators to ignore the field, which defeats the point; annotating none of them
// is the silent-empty failure the whole design exists to prevent (INV-3).
//
// A note is emitted when the decision denied, would have denied under report-only,
// or produced a predicate that can never match anything.
func policyNote(d domain.AccessDecision) *pb.AccessDecisionOp {
	switch {
	case !d.Allowed, d.WouldHaveDenied, d.Reason == domain.ReasonUnsatisfiablePolicy:
		return AccessDecisionToOp(d)
	default:
		return nil
	}
}
