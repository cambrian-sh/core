package operator

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
)

type stubRadius struct {
	radius BlastRadius
	seen   BlastRadiusMutation
}

func (s *stubRadius) EstimateBlastRadius(_ context.Context, m BlastRadiusMutation) (BlastRadius, error) {
	s.seen = m
	return s.radius, nil
}

func radiusService(e BlastRadiusEstimator) *Service {
	s := &Service{}
	s.SetBlastRadiusEstimator(e)
	return s
}

// The property this RPC exists for: an EMPTY preview understates the radius, and
// understating is the one direction this number must never be wrong in. A kernel
// that cannot compute it must refuse rather than answer "nothing is affected".
func TestBlastRadius_UnwiredRefusesRatherThanReturningEmpty(t *testing.T) {
	_, err := (&Service{}).GetBlastRadiusPreview(context.Background(), &pb.BlastRadiusPreviewOpRequest{
		Mutation: MutationSetScope, TargetId: "analyst",
	})
	if codeOf(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented — an empty preview would read as 'safe to apply'", codeOf(err))
	}
}

// An unknown mutation is REFUSED, not previewed as no-change. "This affects
// nothing" and "I do not know what this is" must not render the same.
func TestBlastRadius_UnknownMutationIsRefused(t *testing.T) {
	s := radiusService(&stubRadius{})

	_, err := s.GetBlastRadiusPreview(context.Background(), &pb.BlastRadiusPreviewOpRequest{
		Mutation: "delete_everything", TargetId: "analyst",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

func TestBlastRadius_ReportsAgentsAndPlans(t *testing.T) {
	st := &stubRadius{radius: BlastRadius{
		Complete: true,
		CacheTTL: 30 * time.Second,
		Agents: []AgentImpact{{
			AgentID: "analyst", Before: "requires internal", After: "unrestricted",
			Direction: DirectionWidened,
		}},
		Plans: []PlanImpact{{
			SessionID: "s1", PlanID: "p1", ReEvaluationRequired: true,
			Reason: "step 2 runs on analyst and was planned against the old boundary",
		}},
	}}
	s := radiusService(st)

	resp, err := s.GetBlastRadiusPreview(context.Background(), &pb.BlastRadiusPreviewOpRequest{
		Mutation: MutationSetScope, TargetId: "analyst", Required: []string{"internal"},
	})
	if err != nil {
		t.Fatalf("GetBlastRadiusPreview: %v", err)
	}
	if len(resp.GetAffectedAgents()) != 1 {
		t.Fatalf("agents = %d, want 1", len(resp.GetAffectedAgents()))
	}
	// Widened is the consequential direction: narrowing breaks a task and someone
	// notices, widening breaks a boundary and nobody does.
	if resp.GetAffectedAgents()[0].GetDirection() != DirectionWidened {
		t.Fatalf("direction = %q, want %q", resp.GetAffectedAgents()[0].GetDirection(), DirectionWidened)
	}
	if !resp.GetAffectedPlans()[0].GetReEvaluationRequired() {
		t.Fatal("an in-flight plan on the widened agent was not flagged")
	}
	if resp.GetAffectedPlans()[0].GetReason() == "" {
		t.Fatal("a flagged plan with no reason is not actionable")
	}
	if resp.GetCacheTtlMs() == 0 {
		t.Fatal("cache_ttl_ms = 0 — a preview with no expiry invites acting on a stale radius")
	}
	// The mutation must reach the estimator intact, or the preview describes a
	// different change than the one about to be applied.
	if st.seen.Kind != MutationSetScope || st.seen.TargetID != "analyst" {
		t.Fatalf("estimator saw %+v", st.seen)
	}
}

// A partial radius must announce itself. Rendered as total, it is exactly the
// understatement the RPC exists to prevent.
func TestBlastRadius_IncompleteIsReportedWithAReason(t *testing.T) {
	s := radiusService(&stubRadius{radius: BlastRadius{
		Complete:         false,
		IncompleteReason: "this kernel does not track in-flight plans",
	}})

	resp, err := s.GetBlastRadiusPreview(context.Background(), &pb.BlastRadiusPreviewOpRequest{
		Mutation: MutationSetToolGrant, TargetId: "analyst",
	})
	if err != nil {
		t.Fatalf("GetBlastRadiusPreview: %v", err)
	}
	if resp.GetComplete() {
		t.Fatal("complete = true on a partial radius")
	}
	if resp.GetIncompleteReason() == "" {
		t.Fatal("incomplete with no reason is worrying rather than actionable")
	}
}

func TestBlastRadius_RequiresTarget(t *testing.T) {
	s := radiusService(&stubRadius{})
	if _, err := s.GetBlastRadiusPreview(context.Background(), &pb.BlastRadiusPreviewOpRequest{
		Mutation: MutationSetScope,
	}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}
