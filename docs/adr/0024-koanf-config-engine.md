# ADR-0024 — Koanf Configuration Engine

**Status:** Implemented · Amended by this PR (config split: 11 layers, curated `tuning.json`, separate `embedder.json` / `providers.json` / `mcp.json`).

The current `LoadConfig` uses `encoding/json` + `os.ExpandEnv` on a single file, with `database.password` hardcoded in a version-controlled file. We replace it with a Koanf four-layer pipeline to enforce the Zero-Secrets Policy and support clean local overrides.

## Decision

`internal/config` adopts `github.com/knadh/koanf/v2` as its loading backbone. All callers continue to receive a `*Config`; the public API is unchanged.

### Loading pipeline (lowest → highest priority)

The pipeline was originally four layers; this amendment expands it to **eleven layers** to reflect the config-file split (`config.json` for user-facing infrastructure, `tuning.json` for hyperparameters, `embedder.json` for the embedding model, `providers.json` for the LLM provider list, `mcp.json` for MCP server definitions, plus per-machine `*.local.json` overrides). All secondary paths are derived from `filepath.Dir(path)`, **not** the process CWD, so tests can use `t.TempDir()` cleanly and the layering is independent of invocation context.

1. **Go struct defaults** — `DefaultConfig() *Config` marshalled to JSON rawbytes via `rawbytes.Provider`. Replaces the ~100 `if == 0` checks in `LoadConfig`. All tuning parameters live here.
2. **`configs/tuning.json`** — committed curated power-user starter. Loaded when present (`loadIfPresent` helper, missing file is not an error). A small, hand-picked subset of the most-likely-to-tune hyperparameters (gatekeeper weights, EWMA alpha, plan timeout, min auction confidence, etc.). New `DefaultConfig()` fields fall through cleanly because the file does not pin them — see "Curated `tuning.json`" below.
3. **`configs/tuning.local.json`** — per-machine tuning override. Wins over `tuning.json` when present. Gitignored.
4. **`configs/config.json`** — user-facing infrastructure. Database connection, metabolism, server port, telemetry. No hyperparameters, no LLM/embedder config, no MCP. Gitignored.
5. **`configs/config.local.json`** — per-machine developer override. Wins over `config.json` when present. Gitignored.
6. **`configs/embedder.json`** — the embedding model (provider, model, endpoint, dimensions, query prefix). Wins over the `embedder` block in `config.json` when both define it. Gitignored. The committed `embedder.json.example` is the template.
7. **`configs/embedder.local.json`** — per-machine embedder override. Wins over `embedder.json` when present. Gitignored.
8. **`configs/providers.json`** — the LLM provider list (ADR-0042; centralized model provisioning). Wins over the `llm_provider` block in `config.json` when both define it. Gitignored. The committed `providers.json.example` is the template.
9. **`configs/providers.local.json`** — per-machine provider override. Wins over `providers.json` when present. Gitignored.
10. **`configs/mcp.json`** — MCP server definitions (ADR-0043). Loaded when present; absent ⇒ no MCP behavior. Wins over the `mcp` block in `config.json` when both define it. Gitignored. The committed `mcp.json.example` is the template.
11. **`env.Provider("CAMBRIAN_", ...)`** — full override of any field via environment variable. Highest priority; the canonical injection point for secrets. The `CAMBRIAN_*` convention applies to fields in every layer (including `tuning.json`, `embedder.json`, `providers.json`, and `mcp.json`).

`LoadConfig(path string)` derives all secondary paths by `filepath.Dir(path)`: `tuning.json`, `tuning.local.json`, `embedder.json`, `embedder.local.json`, `providers.json`, `providers.local.json`, `mcp.json` use `filepath.Join(dir, ...)`, and `config.local.json` uses `strings.TrimSuffix(path, ".json") + ".local.json"`. No signature change.

### Curated `tuning.json` (NOT a full mirror)

The committed `configs/tuning.json` lists only **13 hand-picked fields** — the "big knobs" power users actually reach for first (gatekeeper weights, EWMA alpha, plan timeout, min auction confidence, fallback confidence threshold, memory relevance threshold, tool retrieval floor, KG²RAG enable flag, etc.). It is **not** a mirror of all 130 hyperparameters in `DefaultConfig()`.

**Why curated, not a full mirror**: a full mirror would create a silent-drift problem. If a new hyperparameter were added to `DefaultConfig()` in a future PR, the committed `tuning.json` would override it to the field's zero value (because Koanf merges per-key and the missing key falls through to the layer-below value, which would be the explicit zero in the mirror). The curated file only names fields it actually intends to tune; everything else falls through to the Go default. New fields just work.

The committed `tuning.json` is therefore **honest documentation of the public tuning surface for the big knobs**, not a snapshot of every value. Users who want to tune something not in `tuning.json` have two paths: (a) a `CAMBRIAN_EXECUTION__*` env var, or (b) copy the field from the README's "Configuration Reference" table into their `tuning.local.json`.

### MCP merge semantics

`mcp.json` (layer 6) and `config.json` (layer 4) can both hold a `mcp` block. Layer 6 wins. **Empty-file edge case**: if `mcp.json` exists but contains `{"mcp": {}}`, it silently wipes the `mcp` block from `config.json` because Koanf merges per-key. This is documented in the `mcp.json.example` header and in the CHANGELOG entry. Mitigation: don't create `mcp.json` if you don't use MCP.

### Environment variable convention

`__` (double-underscore) is the hierarchy separator between key segments; `_` within a segment is literal. This avoids collision with underscored field names.

```
CAMBRIAN_DATABASE__PASSWORD       → database.password
CAMBRIAN_EXECUTION__EWMA_ALPHA    → execution.ewma_alpha
CAMBRIAN_MODELS__0__API_KEY_ENV   → models[0].api_key_env
```

All config fields are overridable this way — not just secrets. Secrets simply happen to be env vars like everything else (12-factor).

### Zero-Secrets Policy

No `{{SECRET}}` or `${VAR}` placeholders in config files. Secrets (`database.password`, `langfuse.secret_key`, `models[*].api_key_env` when the provider is non-local) must arrive via the env layer. The post-load validator checks these required fields are non-empty after all layers merge.

### Why `rawbytes.Provider` for defaults, not `structs.Provider`

`structs.Provider` requires adding `koanf:"..."` struct tags to every field — a parallel tag set alongside the existing `json` tags with no benefit. The `rawbytes` approach marshals the pre-filled `defaultConfig()` struct using the existing `json` tags and loads the resulting bytes as the first layer. Defaults remain co-located with the struct, readable and testable in isolation.

### Why `config.json` is trimmed to infrastructure-only

With Go defaults as layer 1, every execution tuning field in `config.json` is redundant — it overrides the default with the same value. Trimming removes ~50 lines of duplicated state that drifts silently. Operators who need non-default tuning use `config.local.json`, the new `tuning.json` / `tuning.local.json` pair (curated power-user starter + per-machine override), or env vars.

### `ConfigError` named type

Both unmarshal type errors (Koanf/mapstructure) and post-load validation failures are wrapped as:

```go
type ConfigError struct {
    Field   string
    Message string
}
```

`main.go` uses `errors.As` to detect `*ConfigError` and prints a clean operator-facing message without a stack trace, then exits. All other errors (file I/O) propagate normally.

### File split

- `config.go` — `Config`, `ExecutionConfig`, `GraphConfig`, `TelemetryConfig`, `LangfuseRawConfig`, `LoadConfig`, `defaultConfig`, `ConfigError`, `Validate`
- `models.go` — `ModelConfig`. Fields `cost_per_1m_input`/`cost_per_1m_output` are retained over the requirements doc's `cost_weight`; raw dollar amounts are required to compute `NormalizedCost = unitCost / maxUnitCostAcrossAllModels` at runtime. Pre-normalising in config would make per-model cost updates require manual rebalancing of all weights.
- `agents.go` — `AgentPoolConfig{ DefaultAgentTimeoutMs int }`. Applied by `BBoltAdapter` when a discovered agent's manifest omits a timeout. No speculative quota fields until a quota subsystem exists.

### Immutability

`*Config` lives exclusively in `internal/kernel` (the composition root). Domain packages (`internal/awareness`, `internal/metabolism`, `internal/supervision`) do not import `internal/config` — they receive extracted scalar values at wiring time. No `*Config` pointer escapes the kernel layer, so mutation after boot is structurally impossible without modifying the composition root.

## Considered Options

- **Keep `os.ExpandEnv` + `encoding/json`** — cannot support layered overrides without reimplementing Koanf; secret-in-file problem persists.
- **`{{SECRET}}` placeholder validation** — introduces a second expansion syntax alongside `${VAR}`; Koanf's `env.Provider` makes it unnecessary entirely.
- **Secrets-only env vars** — simpler to document but prevents container deployments from tuning execution parameters without file changes; inconsistent with 12-factor.
- **`structs.Provider` for defaults** — requires `koanf` struct tags on ~60 fields with no benefit over `rawbytes` + existing `json` tags.
