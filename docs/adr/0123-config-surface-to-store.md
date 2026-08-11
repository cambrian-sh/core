---
id: 0123
title: Move the operator-decided config surface onto the store (files become bootstrap + machine facts)
status: Proposed
date: 2026-08-11
supersedes: []
superseded_by: []
depends_on:
  - 0101-config-and-secret-store
  - 0057-open-core-boundary
---

# ADR-0123: Move the operator-decided config surface onto the store

## Status

Proposed — awaiting owner sign-off. Nothing in this ADR is implemented; the
contract-0096 role/generator work that motivated it shipped separately and is
recorded in CONTEXT.md.

## Context

The owner direction (2026-08-11): *"almost all config should be at the DB, not
config files."* ADR-0101 built the machinery for that — a durable store layer
above every file and below the environment, per-key provenance, shadow
detection, a write-only secret store — but the machinery is far ahead of its
coverage. What an operator can actually persist through the console today:

| Surface | Mechanism | Since |
|---|---|---|
| 12 numeric execution tunables (7 blend weights + 5 scalars) | `SetConfig` (`map<string,double>`) against the curated `tunables` catalogue | 0072 |
| The generator list | `SaveGenerator` / `RemoveGenerator` (whole-list store key) | 0083 |
| Generator credentials, Telegram token, named ingress secrets | secret store, write-only | 0072 / 0112 |
| Role bindings (planner/verifier/router/interview/memory) | `SetRoleAssignment`, per-role store keys, live hot-apply | 0096 |

Everything else is file-only: `llm_provider.default`, `health.*`,
`max_concurrency`, the whole `embedder` block, `mcp` servers, `chunker` routes,
`agent_pool`, and every execution key outside the 12 — including every
STRING-valued key (`agentic_planner_model`, `chunker.default`, the router
vocabulary), because `SetConfigOpRequest.values` is `map<string,double>` and
cannot carry one.

Two design facts constrain any widening:

1. **Koanf merge shape dictates key granularity.** Maps merge per key (roles
   compose as `llm_provider.roles.<role>`); lists replace wholesale (the
   generator list must be stored as one key). Every new stored surface must
   pick its granularity from this rule, not from convenience.
2. **A store write outlives the console that made it.** Anything storable must
   be write-time validated against the states that refuse to boot, because the
   console that could undo a bad store write needs a running kernel
   (`CAMBRIAN_CONFIG_STORE=off` recovers, but by abandoning EVERY stored
   setting). Contract 0096 established the pattern: refuse the write
   (dangling role, removing the default) and soften boot validation where the
   runtime genuinely tolerates the state.

## Decision (proposed)

**Principle: config files are for (a) bootstrap facts the kernel needs before
the store exists and (b) machine-local facts that are properties of the host,
not of the deployment's behaviour. Every knob an operator decides moves to the
store, reached through a typed operator surface.**

### Stays in files / environment, permanently

- `database.*`, `server.*`, `storage.*` — bootstrap circularity: the store
  opens from `storage`, and the console reaches the kernel through `server`.
- `metabolism.python_executable`, `agents_dir` — host paths, not decisions.
- `tuning.local.json` — remains the gitignored benchmark rig (unchanged).
- Secrets in `.env` / `CAMBRIAN_*` — the env layer deliberately outranks the
  store (ADR-0101 D1); deployments keep winning.

### Wave 1 — close the gaps in surfaces that already exist (cheap)

1. **`llm_provider.default`**: `SetDefaultGenerator` RPC, same shape as
   `SetRoleAssignment` (validate the id exists; store key
   `llm_provider.default`; restart_required — the default seeds cost/ledger at
   boot). Removal guard already refuses deleting it.
2. **Widen the tunables catalogue** to the full numeric/bool `ExecutionConfig`
   surface (from 12 keys to the ~all of them), with min/max from
   `ExecutionConfig.Validate()`'s ranges — which finally gives that unused
   validator a caller. Same RPCs, no proto change, only catalogue entries and
   consequences copy.
3. **String tunables**: additive `map<string,string> string_values` field on
   `SetConfigOpRequest` + a `Type` field on the catalogue entry
   (float/bool/string/enum with allowed values). This deliberately stays a
   CATALOGUE surface — validation data, not a free write path. Covers
   `agentic_planner_model`, `chunker.default`, and future model-name knobs.

### Wave 2 — the embedder block (careful)

`SaveEmbedder` typed RPC (provider/model/endpoint/timeout/query_prefix), with
`dimensions` **excluded from the writable set**: a dimension change is a
store-migration event (ADR-0107 projection path), not a config edit, and a
console must not be one keystroke away from it. Effect restart_required;
write-time warning when the model implies a different dimension.

### Wave 3 — MCP servers (mirror generators) — **SHIPPED 2026-08-11, contract 0097**

Pulled forward by owner directive ("I want to be able to connect to MCP
servers through the UI") and delivered ahead of waves 1–2:
`SaveMCPServer`/`RemoveMCPServer` over the whole-list store key `mcp.servers`,
with LIVE attach/detach through the connector (no restart);
`SetMCPServerToken`/`ClearMCPServerToken` on the secret store
(`mcp:<id>:token`, env still wins, set bounces the connection);
`TestMCPServer` ephemeral probe. See the CONTEXT.md row for details.

### Explicitly out of scope

- Per-agent manifests, plugin config (ADR-0082 owns entitlement), premium
  telemetry admin (contract 0078 plane already exists for it).
- Any write path that bypasses the typed accessors (no generic JSON blob RPC —
  the numeric/string catalogue plus typed block RPCs is the whole surface).

## Consequences

- `config.example.json` shrinks toward bootstrap-only; docs teach "install →
  console" instead of "install → edit four JSON files".
- Each wave is a proto surface change: contract bump + UI re-vendor each time
  (or batch waves per the 0072 precedent).
- The provenance/shadow machinery needs no changes — it is already per-key.
- Benchmark arms that must ignore operator writes keep working: a nil store
  reproduces the file-only pipeline exactly (ADR-0101).

## Open questions for the owner

1. Wave order acceptable? Wave 1.2 (full numeric catalogue) is the largest
   consequence-copy effort; it can trail 1.1/1.3.
2. Should `health.*` + `max_concurrency` ride Wave 1 as numerics (they are), or
   wait for a provider-block surface?
3. Batch the waves into one contract bump, or bump per wave?
