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
	QueryPrefix         string `json:"query_prefix,omitempty"`
	SupportsLongContext bool   `json:"supports_long_context,omitempty"`
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
	// DisableThinking turns off server-side reasoning for OpenAI-compat reasoning
	// models (e.g. deepseek-v4-flash on opencode) by sending
	// thinking:{"type":"disabled"} in the request. Reasoning tokens are generated
	// BEFORE the answer, so leaving them on adds latency and can consume the whole
	// max_tokens budget (empty content). No effect on models that ignore the field.
	DisableThinking bool `json:"disable_thinking,omitempty"`
	// NativeTools declares that this generator's endpoint honours the provider's
	// tool-calling API (ADR-0097 D2). DECLARED, not probed: probing means the first
	// request of a session decides behaviour, which makes an A/B unattributable and a
	// failure intermittent. A wrong declaration fails loudly on first use, which is
	// the better failure.
	//
	// Default false — an OpenAI-COMPATIBLE endpoint is not necessarily tool-capable,
	// and silently sending `tools` to one that ignores them yields a model that never
	// calls anything, which reads as a model quality problem rather than a config one.
	// Verified true for deepseek-v4-flash and mimo-v2.5 on opencode (2026-07-28).
	NativeTools bool `json:"native_tools,omitempty"`
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
	// MaxConcurrency caps the number of in-flight LLM calls across ALL call paths
	// (agents, planner, verifier, agentic-retrieval sub-queries, consolidator) via a
	// global semaphore at the Provider's Acquire chokepoint. The LLMGateway CONWIP
	// semaphore only bounds agent calls; the direct system-organ calls bypass it, so
	// without this Cambrian can flood a rate-limited endpoint (HTTP 429). 0 ⇒ default
	// (defaultLLMMaxConcurrency); a negative value disables the cap (unbounded).
	MaxConcurrency int `json:"max_concurrency"`
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

		// api_key_env is NO LONGER REQUIRED (ADR-0101 D5).
		//
		// It predates the credential store, when an environment variable was the
		// only way to supply a key. A key stored from the console has no env var
		// by design -- the console never invents a variable name -- so keeping
		// this a hard error meant the kernel REFUSED TO BOOT on a generator an
		// operator had just added through the supported path.
		//
		// A generator with no credential from either source is still a real
		// problem, but it is not a config-FILE problem: it surfaces as
		// key_configured=false in the console, in the generator test, and as the
		// endpoint's own 401. Refusing to start is the one response that helps
		// nobody, because the console that fixes it needs a kernel that is up.
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
