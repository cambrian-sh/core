package config

// TraceConfig is execution.trace: the decision-level run trace (ADR-0145 D10, the
// ADR-0139 C6 run event log). On, every model call the kernel makes or brokers
// (planner, thought steps, chat and worker agents through the gateway: system
// prompt, messages, tools offered, response), every kernel-executed tool call
// (arguments, bounded result) and every plan step's output is written as an
// OpenTelemetry-GenAI-shaped span, one JSON line each, to Dir; spawned agents are
// told the same directory (CAMBRIAN_TRACE_DIR) and trace their own model and tool
// calls there. Read at the composition root (app.wireRunTrace) and by the agent
// spawner (kernel.NewMetabolismStack). Default off.
type TraceConfig struct {
	// Enabled turns the trace on. Default false.
	Enabled bool `json:"enabled,omitempty"`
	// Dir is the directory trace files are written to. Required when Enabled.
	Dir string `json:"dir,omitempty"`
	// MaxContentChars bounds every string in a span (a message, a tool result);
	// longer content keeps its head and a "[truncated N of M chars]" marker. A
	// repeated system prompt or tool menu is written once per file and by hash
	// after. 0 ⇒ 20000.
	MaxContentChars int `json:"max_content_chars,omitempty"`
}

// ContentCap is MaxContentChars with its default applied.
func (c TraceConfig) ContentCap() int {
	if c.MaxContentChars > 0 {
		return c.MaxContentChars
	}
	return 20000
}

func (c TraceConfig) validate() []string {
	var errs []string
	if c.Enabled && c.Dir == "" {
		errs = append(errs, "trace.dir is required when trace.enabled is true")
	}
	if c.MaxContentChars < 0 {
		errs = append(errs, "trace.max_content_chars must be >= 0 (0 uses the default, 20000)")
	}
	return errs
}
