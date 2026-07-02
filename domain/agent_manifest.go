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
}
