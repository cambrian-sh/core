package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// Blast-radius mutation kinds. A closed set: an unknown mutation is REFUSED
// rather than previewed as no-change, because "this affects nothing" and "I do
// not know what this is" must not render identically.
const (
	MutationTagMemory    = "tag_memory"
	MutationSetScope     = "set_scope"
	MutationSetWriteTags = "set_write_tags"
	MutationSetToolGrant = "set_tool_grant"
)

// BlastRadiusMutation describes a proposed change, in the same terms the write
// RPC that would apply it receives.
type BlastRadiusMutation struct {
	Kind      string
	TargetID  string
	Tags      []string
	Required  []string
	AnyOf     []string
	Forbidden []string
}

// BlastRadius is what a mutation would do to agents and in-flight plans.
type BlastRadius struct {
	Agents []AgentImpact
	Plans  []PlanImpact
	// CacheTTL is how long the answer stays meaningful. A blast radius is a
	// statement about LIVE state — agents register, plans finish — so a stale one
	// understates, which is the direction that misleads.
	CacheTTL time.Duration
	// Complete is false when something could not be inspected. A partial radius
	// rendered as total is exactly the understatement this preview exists to
	// prevent.
	Complete         bool
	IncompleteReason string
}

// AgentImpact is one agent whose effective reach would change.
type AgentImpact struct {
	AgentID string
	Before  string
	After   string
	// Direction is DirectionWidened / DirectionNarrowed / DirectionUnchanged.
	Direction string
}

// Impact directions.
const (
	// DirectionWidened is the one that matters. Narrowing an agent breaks a task
	// and someone notices; widening one breaks a boundary and nobody does.
	DirectionWidened   = "widened"
	DirectionNarrowed  = "narrowed"
	DirectionUnchanged = "unchanged"
)

// PlanImpact is one in-flight plan that would need re-evaluating.
type PlanImpact struct {
	SessionID            string
	PlanID               string
	ReEvaluationRequired bool
	Reason               string
}

// BlastRadiusEstimator computes the preview. nil ⇒ Unimplemented, never an empty
// preview: an empty answer understates the radius, and understating is the one
// direction this number must never be wrong in.
type BlastRadiusEstimator interface {
	EstimateBlastRadius(ctx context.Context, m BlastRadiusMutation) (BlastRadius, error)
}

// SetBlastRadiusEstimator wires the preview.
func (s *Service) SetBlastRadiusEstimator(e BlastRadiusEstimator) { s.blastRadius = e }

// HasBlastRadiusEstimator reports whether the preview can be served.
func (s *Service) HasBlastRadiusEstimator() bool { return s.blastRadius != nil }

func validMutation(kind string) bool {
	switch kind {
	case MutationTagMemory, MutationSetScope, MutationSetWriteTags, MutationSetToolGrant:
		return true
	}
	return false
}

// GetBlastRadiusPreview reports what a mutation would do, without applying it.
func (s *Service) GetBlastRadiusPreview(ctx context.Context, req *pb.BlastRadiusPreviewOpRequest) (*pb.BlastRadiusPreviewOp, error) {
	if s.blastRadius == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot preview a blast radius; showing an empty one would understate it")
	}
	if !validMutation(req.GetMutation()) {
		return nil, status.Errorf(codes.InvalidArgument,
			"unknown mutation %q — refusing rather than previewing it as affecting nothing", req.GetMutation())
	}
	if req.GetTargetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_id is required")
	}

	radius, err := s.blastRadius.EstimateBlastRadius(ctx, BlastRadiusMutation{
		Kind:      req.GetMutation(),
		TargetID:  req.GetTargetId(),
		Tags:      req.GetTags(),
		Required:  req.GetRequired(),
		AnyOf:     req.GetAnyOf(),
		Forbidden: req.GetForbidden(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate blast radius: %v", err)
	}

	out := &pb.BlastRadiusPreviewOp{
		ComputedAtUnixMs: time.Now().UnixMilli(),
		CacheTtlMs:       radius.CacheTTL.Milliseconds(),
		Complete:         radius.Complete,
		IncompleteReason: radius.IncompleteReason,
	}
	for _, a := range radius.Agents {
		out.AffectedAgents = append(out.AffectedAgents, &pb.AgentImpactOp{
			AgentId:   a.AgentID,
			Before:    a.Before,
			After:     a.After,
			Direction: a.Direction,
		})
	}
	for _, p := range radius.Plans {
		out.AffectedPlans = append(out.AffectedPlans, &pb.PlanImpactOp{
			SessionId:            p.SessionID,
			PlanId:               p.PlanID,
			ReEvaluationRequired: p.ReEvaluationRequired,
			Reason:               p.Reason,
		})
	}
	return out, nil
}
