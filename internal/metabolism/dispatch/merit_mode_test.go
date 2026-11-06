package dispatch

import (
	"context"
	"math/rand"
	"strconv"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// The Dispatcher stamps the measured wall time of the winner's call on the response
// (T-20261104-step-taskevents-carry-no-latency-so-the-merit), overwriting anything
// the agent echoed, and never below 1 ms (0 would read as "unknown").
func TestExecute_StampsWinnerCallLatency(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.9)}}
	caller := &fakeCaller{response: &domain.Handoff{FromAgent: "a", Context: map[string]string{
		domain.HandoffKeyWinnerLatencyMs: "-5",
	}}}
	res, err := newDispatcher(gk, caller).Execute(context.Background(), &domain.DispatchTask{ID: "t"}, &domain.Handoff{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ms, err := strconv.Atoi(res.Handoff.Context[domain.HandoffKeyWinnerLatencyMs])
	if err != nil || ms < 1 {
		t.Fatalf("latency stamp = %q, want a measured value >= 1", res.Handoff.Context[domain.HandoffKeyWinnerLatencyMs])
	}
}

// ADR-0139 D6: under merit_mode = thompson the Gatekeeper's posterior sample is the
// exploration policy, so the epsilon branch must not fire even at rate 1.0; under
// posterior_lcb it still does.
func TestSelectWinner_ThompsonSkipsEpsilonExploration(t *testing.T) {
	slate := []domain.ScoredCandidate{cand("a", 0.9), cand("b", 0.5), cand("c", 0.1)}
	for _, tc := range []struct {
		mode        string
		wantExplore bool
	}{
		{config.MeritModeThompson, false},
		{config.MeritModePosteriorLCB, true},
		{config.MeritModeLegacy, true},
	} {
		d := newDispatcher(&fakeGatekeeper{candidates: slate}, &fakeCaller{})
		d.ExecCfg.Gatekeeper.MeritMode = tc.mode
		d.ExplorationRate = 1.0
		d.Rand = rand.New(rand.NewSource(3))
		explored := false
		for i := 0; i < 20; i++ {
			if _, reason := d.selectWinner(context.Background(), &domain.DispatchTask{ID: "t"}, slate); reason == ReasonExploration {
				explored = true
			}
		}
		if explored != tc.wantExplore {
			t.Errorf("mode %s: explored = %v, want %v", tc.mode, explored, tc.wantExplore)
		}
	}
}
