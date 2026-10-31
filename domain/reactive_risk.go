package domain

import (
	"errors"
	"fmt"
)

// The ADR-0063 layer-3 risk gate, as ONE predicate every registration door calls.
//
// An `llm` condition lets untrusted signal content decide whether a watch fires.
// Pointing that at a consequential, unattended action — starting a plan or
// dispatching an agent — lets a prompt-injected message choose to act, so such a
// registration needs the operator's explicit acknowledgement (`approved`).
//
// It used to live only in the OperatorConsole RegisterWatch handler, so the
// agent-facing Orchestrator RegisterWatch and authored-pipeline arming both
// skipped it (T-20261031-adr-0063-watch-risk-gate-is-bypassed-by). It lives here
// because domain is the one package both doors can reach: core's operator handler
// cannot import premium, and premium's pipeline package converts watches into
// graphs whose types core never sees. Each door describes its registration in
// the two facts the gate needs — "is an LLM deciding?" and "which actions run?" —
// and this decides. A deterministic security gate (ADR-0034), not routing.

// ErrHighRiskUnapproved is returned when a high-risk LLM-gated registration
// lacks the operator's acknowledgement. Callers wrap it; tests match it.
var ErrHighRiskUnapproved = errors.New("a high-risk llm-condition watch (start_plan/dispatch_agent action) requires approved=true")

// IsHighRiskReactiveAction reports whether an action type is consequential and
// unattended enough that an LLM must not trigger it unacknowledged.
func IsHighRiskReactiveAction(actionType string) bool {
	return actionType == "start_plan" || actionType == "dispatch_agent"
}

// CheckReactiveRisk is the gate. llmGated says an LLM decides whether the
// actions run; actionTypes are every action the registration can perform.
func CheckReactiveRisk(llmGated bool, actionTypes []string, approved bool) error {
	if !llmGated || approved {
		return nil
	}
	for _, t := range actionTypes {
		if IsHighRiskReactiveAction(t) {
			return fmt.Errorf("%w (action %q)", ErrHighRiskUnapproved, t)
		}
	}
	return nil
}

// CheckWatchRisk applies the gate to a WatchConfig. It reads EffectiveActions,
// not just Action: a multi-arm watch whose SECOND arm is start_plan is the same
// risk, and checking only the legacy single Action would wave it through.
func CheckWatchRisk(cfg WatchConfig) error {
	arms := cfg.EffectiveActions()
	types := make([]string, 0, len(arms))
	for _, a := range arms {
		types = append(types, a.Type)
	}
	return CheckReactiveRisk(cfg.ConditionType == ConditionTypeLLM, types, cfg.Approved)
}
