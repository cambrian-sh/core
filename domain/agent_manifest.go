package domain

// AgentManifest is the A2A (Agent-to-Agent / Agent-to-OS) capability contract that
// every agent publishes. It declares static competencies (Tools, SupportedFormats)
// and the technical contract (InputSchema, OutputSchema) used by the Gatekeeper's
// Declaration layer for hard compatibility filtering.
type AgentManifest struct {
	Version                   string         `json:"version,omitempty"`
	Trait                     AgentTrait     `json:"trait,omitempty"`
	Tools                     []string       `json:"tools,omitempty"`
	SupportedFormats          []string       `json:"supported_formats,omitempty"`
	InputSchema               map[string]any `json:"input_schema,omitempty"`
	OutputSchema              map[string]any `json:"output_schema,omitempty"`
	ReleaseNotes              string         `json:"release_notes,omitempty"`
	Dependencies              []string       `json:"dependencies,omitempty"`
	CostPer1MTokens           float64        `json:"cost_per_1m_tokens,omitempty"`
	Capabilities              []string       `json:"capabilities,omitempty"`
	RequiredModelCapabilities []string       `json:"required_model_capabilities,omitempty"` // hard floor for TraitModel selection (ADR-0018)
	// MemoryLimitMB is the per-agent memory cap this agent needs (SEC-01). Heavy
	// agents (docling/reranker/torch) declare a generous value; lightweight agents
	// omit it and inherit the operator's global default. 0 = use the global
	// default (execution.agent_memory_limit_mb); the effective cap is the agent's
	// own declared need, not one blunt number for the whole fleet.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`
	// PythonDeps lists the top-level import names this agent needs (PLAT-01), e.g.
	// ["docling", "torch"]. The kernel verifies they resolve in the target Python
	// before spawning, so a missing package is an install-time error naming the
	// dep — not a silent ImportError crash after boot. Empty ⇒ no self-check.
	PythonDeps []string `json:"python_deps,omitempty"`
	// ToolAllowlist is a declarative profile's tool allowlist (globs over kernel
	// tool names). The kernel enforces it as grants plus a ceiling for profile
	// agents (app/profile_agents.go); it is never read as capability tags.
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`
	// Skills is a profile's skill menu (skill names), carried for the agent card.
	Skills []string `json:"skills,omitempty"`
	// Examples are a profile's example tasks (ADR-0139 C2: 3-5 per agent),
	// published on the A2A card's skills (internal/agentcard).
	Examples []string `json:"examples,omitempty"`
	// StreamsOutput declares that the agent pushes its own live step output
	// (agent-plane EmitStepOutput) — an external agent that calls its own model.
	// The kernel then stops forwarding gateway deltas for its steps, so one step
	// never streams twice (ADR-0139 amendment 2026-11-06). Published on the A2A
	// card's Cambrian extension.
	StreamsOutput bool `json:"streams_output,omitempty"`
}
