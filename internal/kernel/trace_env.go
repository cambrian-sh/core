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
