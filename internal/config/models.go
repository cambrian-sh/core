package config

// ModelConfig defines an LLM provider instance that Cambrian can route to.
type ModelConfig struct {
	// ID names this generator in the credential store (ADR-0101 D5). Empty for a
	// model configured with no store entry of its own; the environment path
	// still applies.
	ID              string   `json:"id,omitempty"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Endpoint        string   `json:"endpoint"`
	APIKeyEnv       string   `json:"api_key_env,omitempty"`
	CostPer1MInput  float64  `json:"cost_per_1m_input"`
	CostPer1MOutput float64  `json:"cost_per_1m_output"`
	TimeoutMs       int      `json:"timeout_ms"`
	Capabilities    []string `json:"capabilities,omitempty"`
	// DisableThinking sends thinking:{"type":"disabled"} for OpenAI-compat
	// reasoning models (e.g. deepseek-v4-flash), suppressing reasoning tokens.
	DisableThinking bool `json:"disable_thinking,omitempty"`
	// NativeTools declares that this endpoint honours the provider's tool-calling
	// API (ADR-0097 D2). See GeneratorConfig.NativeTools — this struct is a
	// field-by-field copy of it, so a field added there MUST be added here and in
	// EVERY mapping that populates it, or the capability silently never arrives.
	//
	// This has now happened twice. ID was added for credential lookup and threaded
	// through the generator registry but missed on the streaming path, so the lane
	// the chat agent uses built clients with no id, could not find the credential
	// stored against that id, and sent every request unauthenticated —
	// indistinguishable from a wrong key. TestGeneratorID_ReachesEveryClientConstruction
	// covers all of them at once; extend it rather than trusting this comment.
	NativeTools bool `json:"native_tools,omitempty"`
}
