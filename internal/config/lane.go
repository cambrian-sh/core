package config

import (
	"fmt"
	"strings"
)

// LaneConfig is execution.lane (ADR-0150 D10): which execution lane serves a
// request, and the single-agent lane's runaway caps and stuck detector.
//
//   - single_agent (the default, owner decision 2026-11-13): one agent in a plain
//     loop holds the person's tools and answers itself. For Execute callers it is
//     the ADR-0050 D1 branch of Server.Execute promoted to the product lane
//     (executeSingleAgent); for chat, the agent the person talks to IS that agent
//     (ADR-0150 D3, internal/chat TurnService.Lane) and is not offered
//     delegate_to_planner.
//   - planner: the ADR-0145 pipeline, unchanged (ADR-0150 D8).
//
// It replaces execution.routing.bypass_selection and single_agent_id (the ADR-0050
// D1 control-arm keys): a layer that still sets them loads as default:
// single_agent and single_agent.agent_id (execution_migrate.go laneMigration).
//
// The subtask keys of D10 (execution.lane.subtasks.*) are LaneSubtasksConfig
// (lane_subtasks.go, ADR-0150 slice 2, read where run_subtasks is wired).
type LaneConfig struct {
	// Default is the lane every request runs in: "single_agent" or "planner".
	// The zero value "" is the pre-ADR-0150 planner path, so an ExecutionConfig
	// built by hand (an embedder, a test) keeps its behaviour; DefaultConfig ships
	// "single_agent".
	Default string `json:"default"`

	// SingleAgent configures the single-agent lane.
	SingleAgent SingleAgentLaneConfig `json:"single_agent"`

	// Stuck is the D4 stuck detector. Its failing half is
	// execution.tools.repeated_failure_stop (one setting read by both the
	// kernel's per-step stop and this lane's agent loop, not two).
	Stuck LaneStuckConfig `json:"stuck"`

	// Subtasks is run_subtasks (ADR-0150 D2/D6; lane_subtasks.go).
	Subtasks LaneSubtasksConfig `json:"subtasks"`

	// Context is the context-engine settings carried to every spawned agent on
	// its handoff; a key set here wins over execution.content (lane_context.go).
	Context LaneContextConfig `json:"context"`
}

// SingleAgentLaneConfig is execution.lane.single_agent.
type SingleAgentLaneConfig struct {
	// AgentID pins the agent the lane dispatches Execute requests to (a profile
	// agent such as uc-personal-assistant). "" = the composed runtime: the kernel
	// composes the request's worker from every registered profession, within the
	// person's access (needs execution.workers.composed). Migrated from
	// execution.routing.single_agent_id.
	AgentID string `json:"agent_id,omitempty"`
	// MaxToolRounds is the D4 runaway cap on the agent's tool rounds. At it the
	// agent makes one tool-less "summarise what you have" call and the result is
	// marked partial. Not a budget: normal work never reaches it. 0 ⇒ 500.
	MaxToolRounds int `json:"max_tool_rounds"`
	// MaxWallSeconds is the D4 runaway cap on the agent's wall time, ending the
	// same way. 0 ⇒ 3600. The plan ceiling (execution.plan.plan_max_duration_ms)
	// and the chat turn timeout stay outer guards.
	MaxWallSeconds int `json:"max_wall_seconds"`
}

// LaneStuckConfig is execution.lane.stuck.
type LaneStuckConfig struct {
	// SameResultRepeats: the same action (tool and arguments) returning the same
	// result this many times is no progress (OpenHands' threshold): one
	// corrective message, then the run ends as partial with exit_reason stuck.
	// 0 ⇒ 4; 1 disables it.
	SameResultRepeats int `json:"same_result_repeats"`
}

// The execution lanes (execution.lane.default).
const (
	LaneSingleAgent = "single_agent"
	LanePlanner     = "planner"
)

// Mode is Default normalised: "" (a hand-built config) is the planner lane.
func (c LaneConfig) Mode() string {
	m := strings.ToLower(strings.TrimSpace(c.Default))
	if m == "" {
		return LanePlanner
	}
	return m
}

// IsSingleAgent reports whether requests run in the single-agent lane.
func (c LaneConfig) IsSingleAgent() bool { return c.Mode() == LaneSingleAgent }

// ToolRounds is SingleAgent.MaxToolRounds with its default applied.
func (c LaneConfig) ToolRounds() int { return orDefault(c.SingleAgent.MaxToolRounds, 500) }

// WallSeconds is SingleAgent.MaxWallSeconds with its default applied.
func (c LaneConfig) WallSeconds() int { return orDefault(c.SingleAgent.MaxWallSeconds, 3600) }

// SameResultRepeats is Stuck.SameResultRepeats with its default applied.
func (c LaneConfig) SameResultRepeats() int { return orDefault(c.Stuck.SameResultRepeats, 4) }

// validate checks execution.lane.
func (c LaneConfig) validate() []string {
	var errs []string
	switch c.Mode() {
	case LaneSingleAgent, LanePlanner:
	default:
		errs = append(errs, fmt.Sprintf("execution.lane.default must be %q or %q (got %q)", LaneSingleAgent, LanePlanner, c.Default))
	}
	if c.SingleAgent.MaxToolRounds < 0 || c.SingleAgent.MaxWallSeconds < 0 || c.Stuck.SameResultRepeats < 0 {
		errs = append(errs, "execution.lane.single_agent.max_tool_rounds, max_wall_seconds and execution.lane.stuck.same_result_repeats must be >= 0")
	}
	errs = append(errs, c.Subtasks.validate()...)
	errs = append(errs, c.Context.validate()...)
	return errs
}
