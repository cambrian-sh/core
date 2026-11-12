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
func contentEnv(c config.ContentConfig) []string {
	return []string{
		fmt.Sprintf("CAMBRIAN_CONTEXT_TOOL_OUTPUT_MAX_CHARS=%d", c.ToolOutputMax()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_TOOL_OUTPUT_PREVIEW_CHARS=%d", c.Preview()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_RESOLVE_PAGE_CHARS=%d", c.Page()),
		fmt.Sprintf("CAMBRIAN_CONTEXT_BUDGET_TOKENS=%d", c.WorkerPromptBudget()),
	}
}
