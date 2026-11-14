package kernel

import (
	"fmt"

	"github.com/cambrian-sh/core/internal/config"
)

// contentEnv is the agent environment derived from execution.content: the limits
// the SDK's context engine applies on the agent side (ContextConfig.from_env reads
// CAMBRIAN_CONTEXT_<FIELD>), served by the kernel so a worker and the kernel size
// content by ONE policy — the worker's prompt budget is also what the kernel sizes
// a dependent step's inputs from, and resolve_cid's window is ledger_get's page
// (owner decision 2026-11-12, T-20261112-worker-resolve-cid-inline-has-no-size-cap-a).
// The effective values are always set (defaults included), so an agent never falls
// back to a built-in number that disagrees with the kernel's.
//
// The values are execution.content with execution.lane.context's overrides applied
// (ExecutionConfig.EffectiveContent; the lane block wins), and the two lane-only
// keys are served when set — so this env channel and the handoff the agent-plane
// Transport stamps can never disagree (owner decision 2026-11-14,
// T-20261105-context-engine-budgets-cannot-be-tuned-for).
func contentEnv(e config.ExecutionConfig) []string {
	c := e.EffectiveContent()
	env := []string{
		fmt.Sprintf("CAMBRIAN_CONTEXT_TOOL_OUTPUT_MAX_CHARS=%d", c.ToolOutputMax()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_TOOL_OUTPUT_PREVIEW_CHARS=%d", c.Preview()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_RESOLVE_PAGE_CHARS=%d", c.Page()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_BUDGET_TOKENS=%d", c.WorkerPromptBudget()),
		// ADR-0150 D7: the age trigger (0 = off).
		fmt.Sprintf("CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS=%d", c.KeepRecentToolResults),
	}
	if v := e.Lane.Context.ToolOutputCompactChars; v > 0 {
		env = append(env, fmt.Sprintf("CAMBRIAN_CONTEXT_TOOL_OUTPUT_COMPACT_CHARS=%d", v))
	}
	if v := e.Lane.Context.ProtectTail; v > 0 {
		env = append(env, fmt.Sprintf("CAMBRIAN_CONTEXT_PROTECT_TAIL=%d", v))
	}
	return env
}
