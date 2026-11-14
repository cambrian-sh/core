package config

import "github.com/cambrian-sh/core/domain"

// LaneContextConfig is execution.lane.context (ADR-0150 D10; owner decision
// 2026-11-14, T-20261105-context-engine-budgets-cannot-be-tuned-for): the SDK
// context-engine settings of every agent the kernel spawns, carried to the agent
// on its handoff (domain.AgentContextLimits). Before this block an operator could
// not tune them for a profile agent: procenv strips every CAMBRIAN_* name from a
// spawned agent's inherited environment and a profile has no context field.
//
// ONE precedence, documented once: a key set here (> 0) WINS — it overrides
// execution.content's value of the same setting everywhere the kernel uses it
// (the CAMBRIAN_CONTEXT_* env it serves, the step-dependency budget it sizes from
// the worker's prompt budget, and the handoff), and on the agent side the
// handoff beats the env and the agent's own context_config
// (sdk ContextConfig.resolve). A key left 0 is unset: budget_tokens and
// keep_recent_tool_results then fall back to execution.content
// (worker_prompt_budget_tokens, keep_recent_tool_results), the other two to the
// SDK's defaults. So the env channel and the handoff can never disagree:
// both are served from ExecutionConfig.EffectiveContent / AgentContext.
type LaneContextConfig struct {
	// BudgetTokens is the agent's whole-prompt budget (SDK budget_tokens);
	// overrides execution.content.worker_prompt_budget_tokens.
	BudgetTokens int `json:"budget_tokens,omitempty"`
	// ToolOutputCompactChars: a tool output longer than this, outside the
	// protected tail, is replaced by a placeholder plus cid (SDK default 1200).
	ToolOutputCompactChars int `json:"tool_output_compact_chars,omitempty"`
	// ProtectTail: the last N history steps / turn groups are never compacted
	// (SDK default 3).
	ProtectTail int `json:"protect_tail,omitempty"`
	// KeepRecentToolResults is the ADR-0150 D7 age trigger; overrides
	// execution.content.keep_recent_tool_results. 0 here means "not set" — turn
	// the trigger off with execution.content.keep_recent_tool_results: 0.
	KeepRecentToolResults int `json:"keep_recent_tool_results,omitempty"`
}

// validate checks execution.lane.context.
func (c LaneContextConfig) validate() []string {
	if c.BudgetTokens < 0 || c.ToolOutputCompactChars < 0 || c.ProtectTail < 0 || c.KeepRecentToolResults < 0 {
		return []string{"execution.lane.context.budget_tokens, tool_output_compact_chars, protect_tail and keep_recent_tool_results must be >= 0 (0 = not set)"}
	}
	return nil
}

// EffectiveContent is execution.content with execution.lane.context's overrides
// applied (the lane block wins). Every kernel consumer of the worker prompt
// budget and the age trigger reads this, never Content directly, so the env the
// kernel serves, the dependency budget it sizes and the handoff agree.
func (c ExecutionConfig) EffectiveContent() ContentConfig {
	out := c.Content
	if v := c.Lane.Context.BudgetTokens; v > 0 {
		out.WorkerPromptBudgetTokens = v
	}
	if v := c.Lane.Context.KeepRecentToolResults; v > 0 {
		out.KeepRecentToolResults = v
	}
	return out
}

// AgentContext is what the kernel stamps on a spawned agent's handoff: only the
// keys execution.lane.context sets (a zero field is not stamped, so the agent's
// own resolution — env from execution.content, then its context_config — stands).
func (c ExecutionConfig) AgentContext() domain.AgentContextLimits {
	l := c.Lane.Context
	return domain.AgentContextLimits{
		BudgetTokens:           l.BudgetTokens,
		ToolOutputCompactChars: l.ToolOutputCompactChars,
		ProtectTail:            l.ProtectTail,
		KeepRecentToolResults:  l.KeepRecentToolResults,
	}
}
