package config

import "fmt"

// StepOutputConfig is execution.step_output: the live step-output stream (ADR-0139
// amendment 2026-11-06; it supersedes ADR-0047 D12's token-level tier). Every field
// is read at the composition root (app.buildStepOutput) and by the network plane;
// zero values take the documented defaults through the methods below, so an
// existing tuning.json needs no new keys.
type StepOutputConfig struct {
	// GatewayDeltasDisabled stops the gateway producer: no model deltas are
	// forwarded from either lane (GenerateViaModelStream, GenerateWithTools), and
	// GenerateWithTools is served unary exactly as before. Agent-pushed output
	// (EmitStepOutput) and the outward channels are unaffected. Default false (on).
	GatewayDeltasDisabled bool `json:"gateway_deltas_disabled,omitempty"`
	// FinalAnswerTool is the tool whose answer argument is the step's deliverable on
	// the native tool lane — the SDK's control vocabulary, so it is configuration,
	// not code (Zero-Hardcode Rule). Its argument is extracted incrementally from the
	// streamed tool-call JSON and forwarded live, and it is the deliverable the
	// ADR-0139 D11 post_model hooks see and may rewrite or refuse (llm.Provider,
	// WithFinalAnswer). Default "final_answer".
	FinalAnswerTool string `json:"final_answer_tool,omitempty"`
	// FinalAnswerArg is that tool's argument holding the deliverable. Default "answer".
	FinalAnswerArg string `json:"final_answer_arg,omitempty"`
	// MaxChunkBytes caps one chunk's data (cut at a UTF-8 boundary). 0 ⇒ 8192.
	MaxChunkBytes int `json:"max_chunk_bytes,omitempty"`
	// MaxStepBytes caps one step's (lease's) total live output. Past it the stream
	// says so once (a status chunk) and drops the rest; the step's final output is
	// unaffected. 0 ⇒ 262144.
	MaxStepBytes int `json:"max_step_bytes,omitempty"`
	// MCPProgressWaitSeconds is how long an MCP submit_task / get_task_status call
	// that carries a progressToken (and no explicit wait_seconds) holds open while
	// streaming the task's step output as notifications/progress. 0 ⇒ 60.
	MCPProgressWaitSeconds int `json:"mcp_progress_wait_seconds,omitempty"`
	// MCPMaxWaitSeconds bounds any wait (explicit wait_seconds included). 0 ⇒ 600.
	MCPMaxWaitSeconds int `json:"mcp_max_wait_seconds,omitempty"`
}

// FinalAnswerToolName is FinalAnswerTool with its default applied.
func (c StepOutputConfig) FinalAnswerToolName() string {
	if c.FinalAnswerTool != "" {
		return c.FinalAnswerTool
	}
	return "final_answer"
}

// FinalAnswerArgName is FinalAnswerArg with its default applied.
func (c StepOutputConfig) FinalAnswerArgName() string {
	if c.FinalAnswerArg != "" {
		return c.FinalAnswerArg
	}
	return "answer"
}

// ChunkBytes is MaxChunkBytes with its default applied.
func (c StepOutputConfig) ChunkBytes() int {
	if c.MaxChunkBytes > 0 {
		return c.MaxChunkBytes
	}
	return 8192
}

// StepBytes is MaxStepBytes with its default applied.
func (c StepOutputConfig) StepBytes() int {
	if c.MaxStepBytes > 0 {
		return c.MaxStepBytes
	}
	return 262144
}

// MCPProgressWait is MCPProgressWaitSeconds with its default applied, never above
// MCPMaxWait.
func (c StepOutputConfig) MCPProgressWait() int {
	w := c.MCPProgressWaitSeconds
	if w <= 0 {
		w = 60
	}
	return min(w, c.MCPMaxWait())
}

// MCPMaxWait is MCPMaxWaitSeconds with its default applied.
func (c StepOutputConfig) MCPMaxWait() int {
	if c.MCPMaxWaitSeconds > 0 {
		return c.MCPMaxWaitSeconds
	}
	return 600
}

// validate checks execution.step_output.
func (c StepOutputConfig) validate() []string {
	var errs []string
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_chunk_bytes", c.MaxChunkBytes},
		{"max_step_bytes", c.MaxStepBytes},
		{"mcp_progress_wait_seconds", c.MCPProgressWaitSeconds},
		{"mcp_max_wait_seconds", c.MCPMaxWaitSeconds},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.step_output.%s must be >= 0 (got %d)", f.name, f.v))
		}
	}
	if c.MaxChunkBytes > 0 && c.MaxStepBytes > 0 && c.MaxChunkBytes > c.MaxStepBytes {
		errs = append(errs, fmt.Sprintf("execution.step_output.max_chunk_bytes (%d) must not exceed max_step_bytes (%d)",
			c.MaxChunkBytes, c.MaxStepBytes))
	}
	return errs
}
