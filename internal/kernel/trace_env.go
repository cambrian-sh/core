package kernel

import (
	"fmt"

	"github.com/cambrian-sh/core/internal/config"
)

// traceEnv is the agent environment that turns on agent-side run tracing
// (ADR-0145 D10): the directory and the content bound of execution.trace. Off ⇒ none.
func traceEnv(c config.TraceConfig) []string {
	if !c.Enabled || c.Dir == "" {
		return nil
	}
	return []string{"CAMBRIAN_TRACE_DIR=" + c.Dir, fmt.Sprintf("CAMBRIAN_TRACE_MAX_CHARS=%d", c.ContentCap())}
}

// ComposedMaxToolRoundsEnv is the variable agents/worker_agent.py's composed runtime
// reads for its per-step tool-call budget (execution.workers.max_tool_rounds).
const ComposedMaxToolRoundsEnv = "CAMBRIAN_COMPOSED_MAX_TOOL_ROUNDS"

// workersEnv is the agent environment derived from execution.workers: the D3
// evidence re-prompt and the composed runtime's tool-call budget. Zero values set
// nothing (the agent's built-in default applies).
func workersEnv(c config.WorkersConfig) []string {
	var env []string
	if c.EvidenceReprompt {
		env = append(env, "CAMBRIAN_EVIDENCE_REPROMPT=1")
	}
	if c.MaxToolRounds > 0 {
		env = append(env, fmt.Sprintf("%s=%d", ComposedMaxToolRoundsEnv, c.MaxToolRounds))
	}
	return env
}
