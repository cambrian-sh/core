package network

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// allocationCaller plays the retrieval_agent and records the model its lease was
// allocated to while the lease is live.
type allocationCaller struct {
	gw     *SubstrateLLMGateway
	winner string
}

func (c *allocationCaller) CallAgent(_ context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	lease := domain.LeaseID(h.Context["_session_token_id"])
	c.gw.mu.RLock()
	if st, ok := c.gw.sessions[lease]; ok {
		c.winner = st.StepAllocation.Winner.ID
	}
	c.gw.mu.RUnlock()
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`{"sub_questions":["a"],"refs":[]}`)}}, nil
}

// Owner decision 2026-11-14, third round (T-20261102-agentic-planner-model-names-a-generator-id):
// the planner follows the retrieval_planner purpose (falling back to the memory role)
// per call; an explicit generator id overrides; neither ⇒ the gateway default.
func TestRetrievalDispatch_PlannerModelFollowsRoleUnlessExplicit(t *testing.T) {
	t.Parallel()
	gw := meteredGateway(llm.NewSpendMeter(0, nil, nil), &usageStreamer{text: "x"})
	_ = NewServer(nil, nil, nil, config.ExecutionConfig{}, nil, nil, nil, nil, nil, nil, gw, nil, nil)

	role := "gen-memory"
	cases := []struct {
		name     string
		model    string
		roleFn   func() string
		want     string
		mutateTo string // a live role switch between two calls
	}{
		{name: "role", roleFn: func() string { return role }, want: "gen-memory", mutateTo: "gen-switched"},
		{name: "explicit overrides role", model: "llm:haiku-local", roleFn: func() string { return role }, want: "llm:haiku-local"},
		{name: "neither", want: ""},
	}
	for _, c := range cases {
		caller := &allocationCaller{gw: gw}
		d := &RetrievalDispatcher{Caller: caller, Gateway: gw, Model: c.model, RoleModel: c.roleFn}
		if _, _, err := d.Decompose(context.Background(), "q"); err != nil {
			t.Fatalf("%s: Decompose: %v", c.name, err)
		}
		if caller.winner != c.want {
			t.Errorf("%s: planner lease allocated to %q, want %q", c.name, caller.winner, c.want)
		}
		if c.mutateTo != "" {
			role = c.mutateTo
			if _, _, err := d.Decompose(context.Background(), "q"); err != nil {
				t.Fatalf("%s: Decompose: %v", c.name, err)
			}
			if caller.winner != c.mutateTo {
				t.Errorf("%s: after a role switch the planner used %q, want %q", c.name, caller.winner, c.mutateTo)
			}
			role = "gen-memory"
		}
	}
}
