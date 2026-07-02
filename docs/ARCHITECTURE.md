# Cambrian Runtime — Architecture

A public overview of the OSS runtime's structure and domain language. Decisions are
recorded as ADRs (`docs/adr/`); this document is the orientation map. *(Premium features
are out of scope here — they plug into the runtime as a separate module; see "Extension
seam" below.)*

## Design principles

- **Strict hexagonal architecture.** The domain core is isolated behind ports; adapters
  (Postgres, gRPC, LLM providers, BBolt, MCP) live at the edges. No business logic in adapters.
- **The Auction model.** Work is not hard-routed to agents. Agents *bid* on tasks; the
  Gatekeeper filters candidates and the Auctioneer selects winners on merit.
- **Zero-Hardcode Rule.** Agent-to-task routing must never be a Go `if/else`/`switch` — it
  lives in the Awareness (LLM) layer. (Deterministic exceptions: system-shell and the
  reflexive path, for safety/latency.)

## Layout

| Path | Role |
|---|---|
| `domain/` | Pure domain types & ports (public, importable). The lingua franca: `Handoff`, `Signal`, `ExecutionPlan`, `Auctioneer`, `SignalReceiver`, … |
| `app/` | **Composition root** — `app.Run(ctx, opts)` wires every subsystem. `app.Options` is the extension seam. |
| `cmd/orchestrator/` | Thin `main` shell over `app.Run`. |
| `internal/kernel/` | Subsystem assembly (`ProvideServer`, the domain "stacks"). |
| `internal/awareness/` | Planner / Cortex — produces `ExecutionPlan`s (the LLM reasoning layer). |
| `internal/metabolism/` | Auctioneer, AgentManager, Gatekeeper, verifier pool — the bidding/selection machinery. |
| `internal/memory/` | LTM: pgvector store, hippocampus, KG²RAG retrieval, scene/edge writers. |
| `internal/supervision/` | Watcher, circadian/lifecycle, signal validation. |
| `internal/substrate/` | gRPC server (`network`), session, operator plane, synaptic event log. |
| `internal/infrastructure/` | Adapters: LLM clients, Postgres, MCP. |
| `api/proto/` | The gRPC/protobuf contract (a held-stable surface). |
| `python-sdk/` | The agent SDK (separate version line; speaks the proto). |

## Request flow (high level)

1. A client opens a `ChatStream` to the gRPC **Server** (`internal/substrate/network`).
2. The **Router** classifies input; the **Planner** (Awareness) produces an `ExecutionPlan`.
3. The **DAG executor** runs steps; for each step the **Gatekeeper → Auctioneer** select an
   agent by bid/merit (the Auction model).
4. **Memory** enriches context (LTM retrieval) and records outcomes; the **Watcher**
   handles passive signal enrichment.

## Configuration (eleven layers)

`config.LoadConfig` composes, lowest-priority first:

1. **Built-in tuned defaults** (`DefaultConfig()` — the full hyperparameter set);
2. **`configs/tuning.json`** (committed, curated power-user starter — 13 hand-picked fields);
3. **`configs/tuning.local.json`** (gitignored, per-machine tuning override);
4. **`configs/config.json`** (gitignored, user-facing infrastructure: database, metabolism, server port, telemetry);
5. **`configs/config.local.json`** (gitignored, per-machine infrastructure override);
6. **`configs/embedder.json`** (gitignored, the embedding model);
7. **`configs/embedder.local.json`** (gitignored, per-machine embedder override);
8. **`configs/providers.json`** (gitignored, the LLM provider list);
9. **`configs/providers.local.json`** (gitignored, per-machine provider override);
10. **`configs/mcp.json`** (gitignored, MCP server definitions; absent ⇒ no MCP);
11. **`CAMBRIAN_*` environment** overrides (highest priority, applies to every layer).

All secondary paths are derived from `filepath.Dir(path)` — not from CWD — so layering
is deterministic across invocation contexts. The committed `tuning.json` is curated,
NOT a full mirror, so new hyperparameters added to `DefaultConfig()` fall through
cleanly. The `embedder` and `llm_provider` blocks are extracted from `config.json`
into their own files because they were 75% of the file's size. See **ADR-0024** for
the full pipeline, merge semantics, and rationale.

Secrets come from `.env` / env vars, never committed. Telemetry is **off by default**
(see `SECURITY.md`).

## Extension seam (open-core)

The runtime is open-core (source-available). The OSS module contains **no premium code**;
commercial features attach through `app.Options` at the composition root — e.g. a generator
trace wrapper, an agent-call logger, and a `NewSignalReceiver` hook that supplies a reactive
engine built from an OSS-provided `ReactiveServices` capability bundle. A premium binary
imports this module and injects those implementations; the boundary is enforced in CI
(`scripts/check-no-premium.sh`). See **ADR-0057** for the full rationale.
