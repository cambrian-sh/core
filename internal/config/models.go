package config

// ModelConfig defines an LLM provider instance that Cambrian can route to.
type ModelConfig struct {
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
}
