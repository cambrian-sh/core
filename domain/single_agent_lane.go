package domain

import "strconv"

// ADR-0150 single-agent lane: the handoff keys the kernel stamps on the request
// of the one agent (Server.executeSingleAgent for Execute callers, the chat turn
// for the merged conversation agent), and the keys the agent's result carries
// back. Kernel-authored, like every "_" key: a caller's copy is overwritten.
const (
	// HandoffKeyLane names the lane the agent runs in ("single_agent"). The SDK
	// reads it to apply the D4 stopping rule and to offer top-level ask-back.
	HandoffKeyLane = "_lane"
	// HandoffKeyLaneMaxToolRounds / HandoffKeyLaneMaxWallSeconds are the D4
	// runaway caps (execution.lane.single_agent.*).
	HandoffKeyLaneMaxToolRounds  = "_lane_max_tool_rounds"
	HandoffKeyLaneMaxWallSeconds = "_lane_max_wall_seconds"
	// HandoffKeyLaneSameResult / HandoffKeyLaneSameFailure are the D4 stuck
	// detector's thresholds (execution.lane.stuck.same_result_repeats and
	// execution.tools.repeated_failure_stop).
	HandoffKeyLaneSameResult  = "_lane_stuck_same_result"
	HandoffKeyLaneSameFailure = "_lane_stuck_same_failure"
	// HandoffKeySubtask carries the subtask id on a run_subtasks worker's request
	// (network.RunSubtask). The SDK reads any "_subtask…" key as "this is a
	// subtask" (stopping.is_subtask: no ask_person), and the agent runtimes leave
	// delegate_to_planner off the menu (the kernel refuses it from a child lease).
	HandoffKeySubtask = "_subtask"

	// HandoffKeyExitReason is the Execute response metadata key naming why a
	// single-agent run ended early: "cap" (tool rounds), "wall", "stuck". With it
	// the response carries _partial_plan=true and HandoffKeyResumable.
	HandoffKeyExitReason = "_exit_reason"
	// HandoffKeyResumable marks a partial answer the caller may continue: the
	// next request in the same session (or the next chat turn) carries on from
	// the ledger and the conversation.
	HandoffKeyResumable = "_resumable"
)

// LaneSingleAgent is the HandoffKeyLane value of the single-agent lane.
const LaneSingleAgent = "single_agent"

// The result keys the SDK sets on a single-agent run's AgentResult.metadata
// (payload metadata), read by the kernel.
const (
	AgentResultPartial    = "partial"
	AgentResultExitReason = "exit_reason"
)

// PayloadTypeInputRequired is the agent result type of a top-level ask-back
// (ADR-0150 D5): the data is JSON {"questions":[{"id","question"}],"reason"}.
const PayloadTypeInputRequired = "input_required"

// LaneLimits are the D4 caps and stuck thresholds of one single-agent run.
type LaneLimits struct {
	MaxToolRounds      int
	MaxWallSeconds     int
	SameResultRepeats  int
	SameFailureRepeats int
}

// Stamp writes the lane keys onto a handoff context (overwriting any copy).
func (l LaneLimits) Stamp(ctx map[string]string) {
	if ctx == nil {
		return
	}
	ctx[HandoffKeyLane] = LaneSingleAgent
	ctx[HandoffKeyLaneMaxToolRounds] = strconv.Itoa(l.MaxToolRounds)
	ctx[HandoffKeyLaneMaxWallSeconds] = strconv.Itoa(l.MaxWallSeconds)
	ctx[HandoffKeyLaneSameResult] = strconv.Itoa(l.SameResultRepeats)
	ctx[HandoffKeyLaneSameFailure] = strconv.Itoa(l.SameFailureRepeats)
}
