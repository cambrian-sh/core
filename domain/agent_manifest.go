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
}
