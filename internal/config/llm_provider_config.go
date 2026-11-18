package config

import (
	"fmt"
	"sort"

	"github.com/cambrian-sh/core/domain"
)

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
	// RETIRED 2026-10-31 (T-20261031-three-config-fields-are-parsed-and-defaulted):
	// `supports_long_context` (ADR-0060 D6). The late-chunker gate reads only
	// `chunker.late.enabled`; nothing ever read this flag.
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
	// MaxTokens is the output cap sent when a request sets none (ADR-0139 C1).
	// 0 = send no cap and take the provider's default — except for Anthropic, whose
	// API requires max_tokens: ApplyGeneratorDefaults fills AnthropicDefaultMaxTokens.
	// This replaces the 4096 the Anthropic adapter used to hardcode.
	MaxTokens int `json:"max_tokens,omitempty"`
	// ContextWindowTokens is the model's context window in tokens (input and
	// output together), as the provider documents it — e.g. 200000 for Claude
	// Haiku 4.5. When set, the Provider checks every request before sending it:
	// estimated prompt tokens + the output cap + the llm_provider.context_margin_ratio
	// share of the window must fit, or the call fails with domain.ErrContextOverflow
	// (gateway: OutOfRange, deterministic) WITHOUT reaching the endpoint
	// (T-20261112-pre-send-context-window-check-per-model-with). 0 = unknown: no
	// pre-send check, the provider's own refusal is the only signal (the behaviour
	// before this field). The window is config, not a built-in table, because the
	// same model id is served with different windows by different endpoints.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// RequestCapabilities overrides, per field, the C1 capability levels the adapter
	// declares (ADR-0139 C1): keys are RequestCapabilityKeys, values "enforced",
	// "best_effort" or "unsupported". Needed because an OpenAI-COMPATIBLE endpoint is
	// not OpenAI: the claudebox proxy behind haiku-local accepts temperature and stop
	// and ignores them, and pastes response_format into the prompt. A level set here
	// decides whether the field is sent, reported as ignored, or refused under strict.
	//
	// For haiku-local (claudebox, probed 2026-11-04) the owner's local providers.json
	// should carry: {"sampling":"unsupported","stop":"unsupported",
	// "response_schema":"best_effort","max_tokens":"best_effort",
	// "tool_choice":"best_effort","parallel_tools":"best_effort",
	// "cache_breakpoints":"unsupported","images":"unsupported"}.
	RequestCapabilities map[string]string `json:"request_capabilities,omitempty"`
}

// AnthropicDefaultMaxTokens is the output cap applied to an Anthropic generator whose
// config sets no max_tokens. The Messages API rejects a request without one.
const AnthropicDefaultMaxTokens = 4096

// RequestCapabilityKeys are the accepted keys of GeneratorConfig.RequestCapabilities,
// one per domain.Capabilities field.
var RequestCapabilityKeys = []string{
	"system_role", "tools", "tool_choice", "parallel_tools", "response_schema",
	"max_tokens", "sampling", "stop", "cache_breakpoints", "images",
}

// ApplyRequestCapabilities returns caps with this generator's overrides applied.
// Unknown keys and values never reach here: validate refuses them at load.
func (g GeneratorConfig) ApplyRequestCapabilities(caps domain.Capabilities) domain.Capabilities {
	for k, v := range g.RequestCapabilities {
		l := domain.Level(v)
		if !l.Valid() {
			continue
		}
		switch k {
		case "system_role":
			caps.SystemRole = l
		case "tools":
			caps.Tools = l
		case "tool_choice":
			caps.ToolChoice = l
		case "parallel_tools":
			caps.ParallelTools = l
		case "response_schema":
			caps.ResponseSchema = l
		case "max_tokens":
			caps.MaxTokens = l
		case "sampling":
			caps.Sampling = l
		case "stop":
			caps.Stop = l
		case "cache_breakpoints":
			caps.CacheBreakpoints = l
		case "images":
			caps.Images = l
		}
	}
	return caps
}

func validateRequestCapabilities(i int, caps map[string]string) []string {
	var errs []string
	keys := make([]string, 0, len(caps))
	for k := range caps {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error order
	for _, k := range keys {
		known := false
		for _, want := range RequestCapabilityKeys {
			if k == want {
				known = true
				break
			}
		}
		if !known {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].request_capabilities: unknown key %q (want one of %v)", i, k, RequestCapabilityKeys))
			continue
		}
		if !domain.Level(caps[k]).Valid() {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].request_capabilities.%s: %q is not enforced|best_effort|unsupported", i, k, caps[k]))
		}
	}
	return errs
}

// ApplyGeneratorDefaults fills the ADR-0042 per-generator defaults in place
// (timeout_ms 0 ⇒ 60 s; an Anthropic max_tokens 0 ⇒ AnthropicDefaultMaxTokens). One function for boot and for the console write path's
// effective list, so a generator hot-applied from a file entry that omits
// timeout_ms gets the same timeout it would get after a restart — not 0, which
// the HTTP clients read as "no timeout".
func ApplyGeneratorDefaults(gens []GeneratorConfig) {
	for i := range gens {
		if gens[i].TimeoutMs == 0 {
			gens[i].TimeoutMs = 60000
		}
		if gens[i].MaxTokens == 0 && gens[i].Provider == "anthropic" {
			gens[i].MaxTokens = AnthropicDefaultMaxTokens
		}
	}
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
	// default cost).
	Default string `json:"default"`
	// Generators is the set of LLMs the Provider can route to, keyed by id.
	Generators []GeneratorConfig `json:"generators"`
	// Roles maps a system-organ role (planner/verifier/router/memory/facts; `interview` is retired)
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

	// Pinned forbids every model fallback (ENG-453, owner decision
	// 2026-11-14: "benchmark and pinned kernels never fall back and fail fast").
	// Each call is served by its FIRST-CHOICE generator — the step's suggested
	// model, else the purpose's role, else the default (the agent plane: the first
	// registered candidate of the step's chain) — or it fails with the provider's
	// own error, logged at ERROR. No other generator is tried, equivalent or not, so
	// a benchmark run can never mix backbones or spend on a paid API behind the
	// operator's back. Default false. Benchmark kernels set it
	// (CAMBRIAN_LLM_PROVIDER__PINNED=true or "pinned": true in the providers file).
	Pinned bool `json:"pinned,omitempty"`

	// EquivalentGenerators lists groups of generator ids an operator declares
	// interchangeable (owner decision 2026-11-14: "production falls back only to
	// generators marked equivalent"). When a call's first-choice generator is
	// unhealthy or fails with an endpoint-class error, the Provider falls back only
	// to a generator that shares a group with it; any other generator is never
	// tried and the call fails with the first choice's own error. Two ids are
	// equivalent iff some group contains both (the relation is not closed over
	// shared members). Default empty: NO fallback crosses to another generator.
	// That is the one behaviour change for existing configs — before this field a
	// failed call walked to the role, the default and then any capability-matching
	// generator, i.e. to a different (often paid) model. Retrying the same
	// generator once after a 401/403 (credential refresh) is not a fallback and
	// is unchanged. Ignored when Pinned. Read at construction (a live
	// SaveGenerator/RemoveGenerator does not change it); an id that names no
	// generator is tolerated with a warning, like a role.
	EquivalentGenerators [][]string `json:"equivalent_generators,omitempty"`

	// ContextMarginRatio is the share of a generator's context_window_tokens the
	// pre-send check keeps free on top of the estimated prompt and the output cap:
	// the estimate has no tokenizer, so a prompt right at the window may still be
	// refused by the provider. 0 = DefaultContextMarginRatio (5%); must be in
	// [0, 1). Read by llm.NewProvider. Ignored for a generator with no window.
	ContextMarginRatio float64 `json:"context_margin_ratio,omitempty"`
}

// DefaultContextMarginRatio is ContextMarginRatio's default.
const DefaultContextMarginRatio = 0.05

// ContextMargin is ContextMarginRatio with its default applied.
func (c LLMProviderConfig) ContextMargin() float64 {
	if c.ContextMarginRatio <= 0 {
		return DefaultContextMarginRatio
	}
	return c.ContextMarginRatio
}

// DefaultGenerator returns the generator marked as the global default, or nil
// if unset/unknown. Used for default cost (metabolism).
func (c LLMProviderConfig) DefaultGenerator() *GeneratorConfig {
	for i := range c.Generators {
		if c.Generators[i].ID == c.Default {
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
		errs = append(errs, validateRequestCapabilities(i, g.RequestCapabilities)...)
		if g.MaxTokens < 0 {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].max_tokens must be >= 0", i))
		}
		if g.ContextWindowTokens < 0 {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].context_window_tokens must be >= 0 (0 = unknown, no pre-send check)", i))
		} else if g.ContextWindowTokens > 0 && g.MaxTokens >= g.ContextWindowTokens {
			errs = append(errs, fmt.Sprintf("llm_provider.generators[%d].max_tokens (%d) must be below context_window_tokens (%d): no prompt would fit", i, g.MaxTokens, g.ContextWindowTokens))
		}

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

	// A role naming an unknown generator is deliberately NOT a boot error
	// (contract 0096). The runtime tolerates it by design — the failover ladder
	// serves the role from the default, ListRoleAssignments reports
	// resolved=false, and NewProvider logs a warning naming the role. Refusing
	// to boot was survivable while roles could only come from files, but roles
	// now also live in the ADR-0101 store: a file edit that removes a generator
	// a STORED role points at would otherwise brick the kernel, and the console
	// that could fix the store needs the kernel up. (`llm_provider.default` and
	// generator ids stay hard errors — the ladder cannot recover from those.)

	for i, group := range c.EquivalentGenerators {
		if len(group) < 2 {
			errs = append(errs, fmt.Sprintf("llm_provider.equivalent_generators[%d] must name at least two generator ids (a group of one declares nothing)", i))
		}
		for j, id := range group {
			if id == "" {
				errs = append(errs, fmt.Sprintf("llm_provider.equivalent_generators[%d][%d] is empty", i, j))
			}
		}
	}

	if c.ContextMarginRatio < 0 || c.ContextMarginRatio >= 1 {
		errs = append(errs, fmt.Sprintf("llm_provider.context_margin_ratio %v must be in [0, 1) (0 = the default %v)", c.ContextMarginRatio, DefaultContextMarginRatio))
	}

	if embedder.Model == "" {
		errs = append(errs, "embedder.model is required when llm_provider is configured")
	}

	return errs
}
