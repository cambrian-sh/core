package config

import "fmt"

// EmbedderConfig declares the single embedding model. ADR-0042: the embedder is
// standalone (not brokered/failed-over) and owns its own dimensions, replacing
// the split between the legacy llm.dimensions field and the models[] entry.
type EmbedderConfig struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint"`
	Dimensions int    `json:"dimensions"`
	TimeoutMs  int    `json:"timeout_ms"`
	// QueryPrefix is prepended to QUERY text only (never to stored documents)
	// before embedding. Asymmetric-retrieval models need it: bge-large-en-v1.5
	// wants "Represent this sentence for searching relevant passages: " on the
	// query side and nothing on the document side (ADR-0048). Empty = no prefix
	// (e.g. nomic, which is symmetric in our setup). The document/store path uses
	// the plain Embed; only the recall path applies this.
	QueryPrefix string `json:"query_prefix,omitempty"`
	SupportsLongContext bool `json:"supports_long_context,omitempty"`
}

// GeneratorConfig declares one LLM generator the Provider can hand out. ADR-0042:
// identity is the stable `id` (used as the registry key, the auction agent
// `llm:<id>`, the belief ResourceID, and the price-ledger key) — not the provider.
type GeneratorConfig struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Endpoint        string   `json:"endpoint"`
	APIKeyEnv       string   `json:"api_key_env,omitempty"`
	CostPer1MInput  float64  `json:"cost_per_1m_input"`
	CostPer1MOutput float64  `json:"cost_per_1m_output"`
	TimeoutMs       int      `json:"timeout_ms"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// HealthConfig tunes the per-id circuit-breaker (ADR-0042 D4). Zero values are
// replaced with safe defaults by LoadConfig.
type HealthConfig struct {
	FailureThreshold int `json:"failure_threshold"`
	CooldownMs       int `json:"cooldown_ms"`
}

// LLMProviderConfig is the centralized model-provisioning block (ADR-0042). It
// replaces the flat models[] array and the duplicated top-level llm block.
type LLMProviderConfig struct {
	// Default is the id of the global default generator (failover step 3 +
	// interview-session base + default cost).
	Default string `json:"default"`
	// Generators is the set of LLMs the Provider can route to, keyed by id.
	Generators []GeneratorConfig `json:"generators"`
	// Roles maps a system-organ role (planner/verifier/interview/router/memory)
	// to a generator id. Deterministic and Zero-Hardcode-legal: roles are not
	// agents bidding for tasks.
	Roles map[string]string `json:"roles"`
	// Health tunes the circuit-breaker.
	Health HealthConfig `json:"health"`
}

// DefaultGenerator returns the generator marked as the global default, or nil
// if unset/unknown. Used for default cost (metabolism) and the interview base.
func (c LLMProviderConfig) DefaultGenerator() *GeneratorConfig {
	for i := range c.Generators {
		if c.Generators[i].ID == c.Default {
			return &c.Generators[i]
		}
	}
	return nil
}

// OllamaGenerator returns the first ollama-provider generator, or nil. The
// streaming gateway + interview grading need a local streaming-capable model.
func (c LLMProviderConfig) OllamaGenerator() *GeneratorConfig {
	for i := range c.Generators {
		if c.Generators[i].Provider == "ollama" {
			return &c.Generators[i]
		}
	}
	return nil
}

// configured reports whether the llm_provider block is present (at least one
// generator declared). When false, validation is skipped so the legacy llm /
// models config still loads during the additive phase (ADR-0042, slice 0042-01).
func (c LLMProviderConfig) configured() bool {
	return len(c.Generators) > 0
}

// validate returns human-readable validation errors for the llm_provider +
// embedder blocks. Empty slice means valid. Only enforced when configured().
func (c LLMProviderConfig) validate(embedder EmbedderConfig) []string {
	if !c.configured() {
		return nil
	}

	var errs []string

	ids := make(map[string]bool, len(c.Generators))
	for i, g := range c.Generators {
		if g.ID == "" {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].id is required", i))
			continue
		}
		if ids[g.ID] {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].id %q is duplicated", i, g.ID))
		}
		ids[g.ID] = true

		if g.Provider != "ollama" && g.APIKeyEnv == "" {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].api_key_env is required for non-ollama provider %q", i, g.Provider))
		}
	}

	if c.Default == "" {
		errs = append(errs, "llm_provider.default is required")
	} else if !ids[c.Default] {
		errs = append(errs, fmt.Sprintf("llm_provider.default %q is not a declared generator id", c.Default))
	}

	for role, id := range c.Roles {
		if !ids[id] {
			errs = append(errs, fmt.Sprintf("llm_provider.roles[%q] = %q is not a declared generator id", role, id))
		}
	}

	if embedder.Model == "" {
		errs = append(errs, "embedder.model is required when llm_provider is configured")
	}

	return errs
}
