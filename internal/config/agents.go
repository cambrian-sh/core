package config

// AgentPoolConfig holds defaults applied to auto-discovered agents in agents_dir
// whose manifests omit the corresponding field.
type AgentPoolConfig struct {
	DefaultAgentTimeoutMs int `json:"default_agent_timeout_ms"`
}
