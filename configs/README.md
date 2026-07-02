# Cambrian configuration files

This directory holds every file Cambrian reads at startup. The runtime composes
its config in an **11-layer pipeline** (lowest → highest priority) via Koanf;
see [`docs/adr/0024-koanf-config-engine.md`](../docs/adr/0024-koanf-config-engine.md)
for the full rationale. The short version:

1. Go defaults (`internal/config.DefaultConfig()`)
2. `tuning.json` — committed curated power-user starter
3. `tuning.local.json` — per-machine tuning override
4. `config.json` — user-facing infrastructure
5. `config.local.json` — per-machine infra override
6. `embedder.json` — the embedding model
7. `embedder.local.json` — per-machine embedder override
8. `providers.json` — the LLM provider list
9. `providers.local.json` — per-machine provider override
10. `mcp.json` — MCP server definitions
11. `CAMBRIAN_*` environment variables (highest priority; full override)

All secondary paths (every layer except `config.json` and the env vars) are
**derived from `filepath.Dir(config.json)`**, not from the process CWD — so
layering is deterministic across invocation contexts (tests, systemd, dev
shells).

## The files at a glance

| File | Tracked? | What it is |
|---|---|---|
| `config.json` | **gitignored** | Your per-machine infra: database, server, telemetry, metabolism. Copy from `config.example.json`. |
| `config.local.json` | gitignored | Per-machine override of `config.json`. Rarely needed. |
| `embedder.json` | **gitignored** | The embedding model (provider, endpoint, dimensions, query prefix). Copy from `embedder.json.example`. |
| `embedder.local.json` | gitignored | Per-machine embedder override. Lets you swap models without touching the shared `embedder.json`. |
| `providers.json` | **gitignored** | The LLM provider list (ADR-0042). Copy from `providers.json.example`. |
| `providers.local.json` | gitignored | Per-machine provider override. |
| `tuning.json` | **committed** | Curated power-user starter — 13 hand-picked hyperparameters. See [§ Tuning](#tuning) below. |
| `tuning.local.json` | **gitignored** | Per-machine tuning override. Wins over `tuning.json`. |
| `mcp.json` | **gitignored** | External MCP server definitions. Copy from `mcp.json.example`. |
| `config.example.json` | committed | Template for `config.json`. The only file that documents the user-facing schema. |
| `embedder.json.example` | committed | Template for `embedder.json`. |
| `providers.json.example` | committed | Template for `providers.json`. |
| `mcp.json.example` | committed | Template for `mcp.json` (one filesystem MCP server as a starter). |
| `config.local.json.example` | committed | Template for `config.local.json`. |

**Bold = "you almost certainly need this file" for a fresh install.**

## What lives where (the split)

The split is the whole point — each file holds one concern. If you're hunting
for a setting, here's where to look:

| If you want to change… | Edit |
|---|---|
| Database host / port / user / dbname | `config.json` |
| Server port, telemetry endpoint | `config.json` |
| Python executable / agents dir | `config.json` |
| Embedding model (e.g. bge-large → nomic-embed-text) | `embedder.json` |
| Embedding dimensions / query prefix | `embedder.json` |
| Which LLM models are available | `providers.json` |
| Which model serves the planner / verifier / etc. | `providers.json` (the `roles` block) |
| Gatekeeper weights, EWMA alpha, plan timeout | `tuning.local.json` |
| KG²RAG enable, blend weights, recall settings | `tuning.local.json` |
| MCP server URLs, auth, tool policy | `mcp.json` |

## First-time setup

The three steps you need to get a working instance:

```bash
# 1. Copy the user-facing templates
cp configs/config.example.json   configs/config.json
cp configs/embedder.json.example  configs/embedder.json
cp configs/providers.json.example configs/providers.json

# 2. Edit each one to point at your LLM/embedding endpoints
#    At minimum, set: database.password, embedder.endpoint, providers endpoint + api_key_env
$EDITOR configs/config.json
$EDITOR configs/embedder.json
$EDITOR configs/providers.json

# 3. (Optional) If you want MCP, copy its template too
cp configs/mcp.json.example configs/mcp.json
$EDITOR configs/mcp.json
```

The first three `cp` commands give you a complete config. `mcp.json` is optional
— the runtime simply does not load MCP if the file is absent.

For per-machine tuning (gatekeeper weights, plan timeout, etc.), edit
`tuning.local.json` (or just rely on the committed `tuning.json` defaults).
Don't tune anything unless you know why.

## Secrets

Secrets **never go in these files**. They come from environment variables
(`.env` or the shell), referenced by name in the config:

- `database.password` → `CAMBRIAN_DATABASE__PASSWORD`
- `llm_provider.generators[].api_key_env` → e.g. `OPENAI_API_KEY`, `GEMINI_API_KEY`
- `mcp.servers[].auth.token_env` → e.g. `MY_MCP_TOKEN`

Every key the operator sets in `.env` (which is itself gitignored) is loaded
into the process environment before any config layer runs, so the
`api_key_env` references resolve naturally.

Env vars also override **any field in any layer** — that's the
`CAMBRIAN_*` env convention:

```
CAMBRIAN_EXECUTION__EWMA_ALPHA=0.7
CAMBRIAN_LLM_PROVIDER__DEFAULT=openai-large
CAMBRIAN_EMBEDDER__MODEL=nomic-embed-text
```

`__` is the hierarchy separator; `_` is literal within a segment. Env vars
**always win** (layer 11).

## Tuning

`tuning.json` is **curated, not a full mirror** of `DefaultConfig()`. It lists
13 hand-picked fields — the "big knobs" power users reach for first. It
deliberately does **not** pin every hyperparameter, because if it did, adding
a new field to `DefaultConfig()` would silently break: the new field would be
absent from the file, and Koanf's per-key merge would mean the field falls
through to its zero value rather than the documented default.

The 13 fields in `tuning.json` are exactly the ones the team has hand-validated
as worth tuning without recompiling. For everything else, use
`CAMBRIAN_EXECUTION__*` env vars (override without a config file) or just
leave it at the default.

The same logic applies to `tuning.local.json`: list only the fields you
actually want to override; let the rest fall through to `tuning.json` (or
the Go default).

## Backward compatibility

If you have an older single-file `config.json` that still contains
`embedder` and `llm_provider` blocks inline — **it still works**. Layer 4
loads it as before, and layers 6/8 (the separate files) are simply absent
and skipped. The validator's `embedder.model is required when llm_provider
is configured` guard still works correctly — it just sources `embedder`
from whichever layer wins (4, 6, or 7).

The same is true for the older `mcp` block inside `config.json`: if present
and `mcp.json` is absent, it loads from `config.json`. If both are present,
`mcp.json` wins (it's a higher layer).

**One non-obvious gotcha**: an empty `mcp.json` (`{"mcp": {}}`) silently
wipes the `mcp` block from `config.json` because Koanf merges per-key.
Don't create `mcp.json` if you don't use MCP.

## Validating a config

`config.LoadConfig` runs a `validateSecrets` step after all 11 layers merge.
Common failures:

- `database.host is required` — `config.json` is missing or has no `database` block.
- `database.password is required (set CAMBRIAN_DATABASE__PASSWORD)` — set it in `.env` or via `CAMBRIAN_DATABASE__PASSWORD=...`.
- `embedder.model is required when llm_provider is configured` — set `embedder.model` in `embedder.json` (or `embedder.model` inline in `config.json`).
- `llm_provider.default is required` / `llm_provider.default X is not a declared generator id` — your `default` must match a generator's `id` in `providers.json`.
- `api_key_env is required for non-ollama provider X` — every non-ollama generator needs an `api_key_env` field whose value is the name of the env var holding the key.

## See also

- [`docs/adr/0024-koanf-config-engine.md`](../docs/adr/0024-koanf-config-engine.md) — the full pipeline, layer-order rationale, curated-`tuning.json` rationale, path-derivation contract, env-var convention.
- [`docs/adr/0042-centralized-llm-provider.md`](../docs/adr/0042-centralized-llm-provider.md) — the `providers.json` schema and the centralized LLM provider design.
- [`docs/adr/0043-mcp-tool-provider.md`](../docs/adr/0043-mcp-tool-provider.md) — the `mcp.json` schema and per-tool policy.
- [`docs/adr/0057-open-core-boundary.md`](../docs/adr/0057-open-core-boundary.md) — why langfuse / reactive engine config is NOT in this directory (it lives in the premium module).
