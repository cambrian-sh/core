package domain

import (
	"errors"
	"testing"
)

func TestCheckWatchRisk(t *testing.T) {
	t.Parallel()
	llm := func(actions ...WatchAction) WatchConfig {
		c := WatchConfig{ID: "w", ConditionType: ConditionTypeLLM, Condition: "is it urgent?"}
		if len(actions) == 1 {
			c.Action = actions[0]
		} else {
			c.Actions = actions
		}
		return c
	}
	cases := []struct {
		name    string
		cfg     WatchConfig
		refused bool
	}{
		{"llm + start_plan, unapproved", llm(WatchAction{Type: "start_plan"}), true},
		{"llm + dispatch_agent, unapproved", llm(WatchAction{Type: "dispatch_agent", TargetType: "agent_id"}), true},
		{"llm + emit_event", llm(WatchAction{Type: "emit_event"}), false},
		// The multi-arm form: the risky arm is not the first one.
		{"llm + [emit_event, start_plan]", llm(WatchAction{Type: "emit_event"}, WatchAction{Type: "start_plan"}), true},
		{"deterministic + start_plan", WatchConfig{ConditionType: "deterministic", Action: WatchAction{Type: "start_plan"}}, false},
	}
	for _, tc := range cases {
		err := CheckWatchRisk(tc.cfg)
		if tc.refused != (err != nil) {
			t.Errorf("%s: refused=%v, want %v (err=%v)", tc.name, err != nil, tc.refused, err)
		}
		if err != nil && !errors.Is(err, ErrHighRiskUnapproved) {
			t.Errorf("%s: error must wrap ErrHighRiskUnapproved, got %v", tc.name, err)
		}
		approved := tc.cfg
		approved.Approved = true
		if err := CheckWatchRisk(approved); err != nil {
			t.Errorf("%s: approved=true must pass, got %v", tc.name, err)
		}
	}
}
