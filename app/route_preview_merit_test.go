package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

type previewFleet struct {
	agents   []domain.AgentDefinition
	profiles map[string]*domain.AgentProfile
}

func (f previewFleet) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return f.agents, nil
}

func (f previewFleet) GetManifest(context.Context, string) (*domain.AgentManifest, error) {
	return nil, nil
}

func (f previewFleet) GetProfile(_ context.Context, agentID, _ string) (*domain.AgentProfile, error) {
	return f.profiles[agentID], nil
}

// PreviewRoute must reflect the ACTIVE merit mode and rank exactly as the live
// Gatekeeper does for the same profiles — including the slate-mean fill of a
// missing latency (T-20261104-previewroute-scores-missing-latency-as-0-while) and,
// under posterior_lcb, the posterior and its tie-breaks (ADR-0139 D6).
func TestPreviewRoute_MatchesLiveGatekeeperPerMode(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"fast":  {AgentID: "fast", SuccessRate: 0.7, TrustScore: 0.7, NetworkLatencyMedianMs: 20, Outcomes: domain.OutcomeEvidence{VerifiedSuccess: 3, VerifiedFailure: 1}},
		"blank": {AgentID: "blank", SuccessRate: 0.7, TrustScore: 0.7},
		"slow":  {AgentID: "slow", SuccessRate: 0.7, TrustScore: 0.7, NetworkLatencyMedianMs: 2000, Outcomes: domain.OutcomeEvidence{VerifiedSuccess: 12, VerifiedFailure: 2}},
	}
	fleet := previewFleet{profiles: profiles}
	var cands []operator.RouteCandidate
	for _, id := range []string{"fast", "blank", "slow"} {
		fleet.agents = append(fleet.agents, domain.AgentDefinition{ID: id})
		cands = append(cands, operator.RouteCandidate{Profile: *profiles[id]})
	}

	for _, tc := range []struct{ mode, arm string }{
		{config.MeritModeLegacy, "hand_weights"},
		{config.MeritModePosteriorLCB, "posterior_lcb"},
	} {
		cfg := config.DefaultConfig().Execution
		cfg.Gatekeeper.MeritMode = tc.mode
		live, err := gatekeeper.NewGatekeeper(fleet, cfg, gatekeeper.WithProfiles(fleet)).
			FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
		if err != nil {
			t.Fatalf("%s: FindCandidates: %v", tc.mode, err)
		}
		ranked, arm := routePreviewAdapter{cfg: cfg}.PreviewRoute(nil, cands)
		if arm != tc.arm {
			t.Errorf("%s: arm = %q, want %q", tc.mode, arm, tc.arm)
		}
		if len(ranked) != len(live) {
			t.Fatalf("%s: preview has %d, live %d", tc.mode, len(ranked), len(live))
		}
		for i := range live {
			if ranked[i].AgentID != live[i].Agent.ID || ranked[i].Score != live[i].Score {
				t.Errorf("%s rank %d: preview %s=%v, live %s=%v", tc.mode, i,
					ranked[i].AgentID, ranked[i].Score, live[i].Agent.ID, live[i].Score)
			}
			if ranked[i].Mode != tc.mode {
				t.Errorf("%s: preview mode = %q", tc.mode, ranked[i].Mode)
			}
		}
	}

	cfg := config.DefaultConfig().Execution
	cfg.Gatekeeper.MeritMode = config.MeritModeThompson
	if _, arm := (routePreviewAdapter{cfg: cfg}).PreviewRoute(nil, cands); arm != "thompson_mean" {
		t.Errorf("thompson arm = %q, want thompson_mean", arm)
	}
}

// The wire carries per-capability success_rate + sample_count but no evidence yet:
// the preview reads them as SR·n successes and (1−SR)·n failures.
func TestPreviewRoute_CapabilityStatsBecomeEvidence(t *testing.T) {
	cfg := config.DefaultConfig().Execution
	cfg.Gatekeeper.MeritMode = config.MeritModePosteriorLCB
	cands := []operator.RouteCandidate{{Profile: domain.AgentProfile{AgentID: "a",
		CapabilityStats: map[string]domain.CapabilityStat{"code": {SuccessRate: 0.75, SampleCount: 8}}}}}
	ranked, _ := routePreviewAdapter{cfg: cfg}.PreviewRoute([]string{"code"}, cands)
	if ranked[0].Alpha != 7 || ranked[0].Beta != 3 {
		t.Fatalf("posterior = Beta(%v,%v), want Beta(7,3)", ranked[0].Alpha, ranked[0].Beta)
	}
}
