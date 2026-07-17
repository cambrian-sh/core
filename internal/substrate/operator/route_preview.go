package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// RouteCandidate is one inline candidate for PreviewRoute (ROUTE-07 / ADR-0077): a
// synthetic agent profile + trait the benchmark supplies so the Gatekeeper's merit
// scoring can be exercised deterministically, no live fleet.
type RouteCandidate struct {
	Profile domain.AgentProfile
	Trait   domain.AgentTrait
}

// RoutePreviewer scores a candidate set for a step's required capabilities and returns
// the merit breakdowns (highest score first) plus the active scorer arm name. Satisfied
// in the kernel by a thin adapter over gatekeeper.ScoreMerit; nil in OSS ⇒ Unimplemented.
type RoutePreviewer interface {
	PreviewRoute(requiredCaps []string, candidates []RouteCandidate) (ranked []domain.MeritResult, arm string)
}

// SetRoutePreviewer wires the ROUTE-07 gatekeeper-preview scorer. nil ⇒ PreviewRoute
// returns Unimplemented.
func (s *Service) SetRoutePreviewer(p RoutePreviewer) { s.routePreview = p }

// PreviewRoute runs the Gatekeeper merit scoring over the request's inline candidates and
// returns the ranked funnel under the active arm (ADR-0077). Read-only, no side effects —
// it neither auctions nor executes; it just scores. Any authenticated role.
func (s *Service) PreviewRoute(_ context.Context, req *pb.PreviewRouteOpRequest) (*pb.PreviewRouteOpResponse, error) {
	if s.routePreview == nil {
		return nil, status.Error(codes.Unimplemented, "route preview is not configured")
	}
	cands := make([]RouteCandidate, 0, len(req.GetCandidates()))
	for _, c := range req.GetCandidates() {
		prof := domain.AgentProfile{
			AgentID:                    c.GetAgentId(),
			SuccessRate:                c.GetSuccessRate(),
			TrustScore:                 c.GetTrustScore(),
			NetworkLatencyMedianMs:     int(c.GetNetworkLatencyMs()),
			ComputationLatencyMedianMs: int(c.GetComputationLatencyMs()),
			ContextGrowthBytesMedian:   int(c.GetContextGrowthBytes()),
			Provisional:                c.GetProvisional(),
		}
		if c.GetAvgCostPerTask() > 0 {
			prof.ModelMetrics = &domain.ModelMetrics{AvgCostPerTask: c.GetAvgCostPerTask()}
		}
		if len(c.GetCapabilityStats()) > 0 {
			prof.CapabilityStats = make(map[string]domain.CapabilityStat, len(c.GetCapabilityStats()))
			for k, v := range c.GetCapabilityStats() {
				prof.CapabilityStats[k] = domain.CapabilityStat{
					SuccessRate: v.GetSuccessRate(),
					TrustScore:  v.GetTrustScore(),
					SampleCount: int(v.GetSampleCount()),
				}
			}
		}
		cands = append(cands, RouteCandidate{Profile: prof, Trait: domain.AgentTrait(c.GetTrait())})
	}
	ranked, arm := s.routePreview.PreviewRoute(req.GetRequiredCapabilities(), cands)
	out := make([]*pb.MeritResultOp, len(ranked))
	for i, m := range ranked {
		out[i] = &pb.MeritResultOp{
			AgentId:     m.AgentID,
			Score:       m.Score,
			SuccessRate: m.SuccessRate,
			TrustScore:  m.TrustScore,
			LatencyTerm: m.LatencyTerm,
			CostTerm:    m.CostTerm,
			Provisional: m.Provisional,
		}
	}
	return &pb.PreviewRouteOpResponse{Ranked: out, Arm: arm}, nil
}
