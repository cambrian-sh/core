# Cambrian Runtime — Architecture

A public overview of the core runtime's structure and domain language. Core is
**source-available under the Elastic License 2.0** (not an OSI open-source licence). Decisions
are recorded as ADRs in the knowledge repository (`cambrian-knowledge/adrs/`; this repo has no
`docs/adr/`); this document is the orientation map. *(Premium features are out of scope here —
they plug into the runtime as a separate module; see "Extension seam" below.)*

*Revised 2026-11-13 for the single-agent default lane (ADR-0150).*

## Design principles

- **Strict hexagonal architecture.** The domain core is isolated behind ports; adapters
  (Postgres, gRPC, LLM providers, BBolt, MCP) live at the edges. No business logic in adapters.
- **One agent by default; fan out when it helps.** A request runs in an **execution lane**
  chosen by `execution.lane.default` (ADR-0150, `internal/config/lane.go`):
  - `single_agent` (the default): one agent holds the person's tools and answers itself. It can
    split read-only work into parallel subtasks with the kernel tool `run_subtasks`
    (`internal/subtasks`), which the kernel runs on child leases capped to read effects, depth 1.
  - `planner` (behind config, kept for research — ADR-0145): an LLM planner produces an
    `ExecutionPlan` (a DAG); for each step **capability-typed dispatch** selects an agent — the
    Gatekeeper filters candidates by declared capability and ranks them on merit, the
    `Dispatcher` calls the winner. Selection costs zero RPCs and zero LLM calls and boots only the
    winner. The auction it replaced is gone (ADR-0100).
- **Zero-Hardcode Rule.** Policy, routing tables and thresholds are config or data, never a Go
  branch on an agent, tool or customer. (Deterministic exceptions: system-shell and the reflexive
  path, for safety/latency.)
- **Fail closed on access, fail open on retrieval quality.** Every memory read and write goes
  through enforcing stores composed in one place (`internal/kernel/memory_stack.go`).

## Layout

| Path | Role |
|---|---|
| `domain/` | Pure domain types & ports (public, importable). The lingua franca: `Handoff`, `Signal`, `ExecutionPlan`, `StepDispatcher`, `AgentCaller`, `SignalReceiver`, `ToolExecutor`, … |
| `app/` | **Composition root** — `app.Run(ctx, opts)` wires every subsystem. `app.Options` is the extension seam. |
| `cmd/orchestrator/` | Thin `main` shell over `app.Run`; other `cmd/` binaries are operator maintenance tools. |
| `internal/kernel/` | Subsystem assembly (`ProvideServer`, the domain "stacks", `NewMemoryStack`). |
| `internal/config/` | The layered config pipeline, the grouped `execution.*` blocks and their v1→v2 migration; `lane.go` is the execution-lane switch. |
| `internal/chat/` | The conversation lane (OpenConversation / SendTurn): turn service, grounding, reply guards; `lane.go` makes the chat agent the single-agent lane's agent (ADR-0150 D3). |
| `internal/subtasks/` | `run_subtasks`, the kernel tool behind single-agent fan-out (ADR-0150 D2). |
| `internal/awareness/` | Planner lane: the planner that produces `ExecutionPlan`s. |
| `internal/metabolism/` | AgentManager (agent lifecycle) and, for the planner lane, Dispatcher and verifier pool. |
| `internal/agentplane/` | `Transport` — the kernel's client side of the agent plane: the gRPC connection pool plus the typed calls over it (execute, verify, propose). No selection. |
| `internal/memory/` | Long-term memory: ingestion, hybrid retrieval, KG²RAG, fact lane, experiential records. Scenes, precedents, procedure induction and the tier-2 memory judge were removed 2026-11-05. |
| `internal/evidence/`, `internal/ontology/`, `internal/objects/`, `internal/knowledgecommit/` | The **knowledge substrate** (ADR-0105–0111, 0147, 0149): evidence, ontology kinds and relations, per-kind object tables, approver-asserted knowledge commits. |
| `internal/ledger/`, `internal/calc/` | ADR-0145 kernel tools: the run ledger (`ledger_get` / `ledger_find`) and calculations. |
| `internal/authz/`, `internal/identity/` | Scope enforcement (enforcing stores, write classification) and principal resolution. |
| `internal/ingress/` | Conversational ingress (ADR-0090): surface identity and outbound delivery for chat front doors. |
| `internal/supervision/` | Watcher, circadian/lifecycle, signal validation. |
| `internal/substrate/` | Legacy name for the kernel's network layer: gRPC server (`network`), sessions, operator plane, event log. Not the knowledge substrate. |
| `internal/infrastructure/` | Adapters: LLM clients, Postgres (incl. the query plane), MCP. |
| `api/proto/` | The gRPC/protobuf contract (a held-stable surface): `cambrian.proto` (agent plane), `operator.proto` (operator plane). Generated — regenerate with `make proto`. |

The Python agent SDK is a separate repository (`cambrian-agent-sdk`, Apache-2.0) that speaks the
proto; it is not in this repo.

## Request flow (high level)

1. A person reaches the kernel through a **conversation** (`OpenConversation` / `SendTurn` on the
   operator plane, or a registered chat ingress) or a client calls **`Execute`**. (`ChatStream` is
   still declared in `cambrian.proto` but not served.)
2. **Default lane (`single_agent`):** the kernel leases the request to one agent with the person's
   tools and scope. The agent calls tools through the `ToolExecutor` chokepoint (kernel tools and
   MCP servers) and may call `run_subtasks`; the kernel runs those children in parallel on child
   leases and returns their results. The agent answers.
3. **Planner lane (`planner`):** the **Router** classifies the input, the **Planner** produces an
   `ExecutionPlan`, and the **DAG executor** runs it; for each step **Gatekeeper → Dispatcher**
   select an agent and call it through the agent-plane `Transport`; coverage and the verifier
   check the result.
4. In both lanes **Memory** serves recall through the enforcing read store and records outcomes;
   approvals go to the approval hub; every LLM call is metered by the spend ledger.

## Configuration (twelve layers)

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
11. **The ADR-0101 config store** (`config.db`, embedded) — where durable operator writes land;
12. **`CAMBRIAN_*` environment** overrides (highest priority, applies to every layer).

All secondary paths are derived from `filepath.Dir(path)` — not from CWD — so layering
is deterministic across invocation contexts. The committed `tuning.json` is curated,
NOT a full mirror, so new hyperparameters added to `DefaultConfig()` fall through
cleanly. The layout is versioned (`schema_version` 2 = grouped `execution.*` blocks); a v1 layer is
migrated on load. See **ADR-0024** and **ADR-0101** for the pipeline, merge semantics, and
rationale.

Secrets come from `.env` / env vars, never committed. Telemetry is **off by default**.

## Extension seam (open-core)

Core contains **no premium code**; commercial features attach through `app.Options` at the
composition root — e.g. a generator trace wrapper, an agent-call logger, a `NewSignalReceiver`
hook that supplies a reactive engine built from the `app.KernelServices` capability bundle
(formerly `ReactiveServices`), and extra gRPC services mounted on the operator plane. A premium
binary imports this module and injects those implementations; the boundary is enforced in CI by
`make separability` (`scripts/check-separability.sh`). See **ADR-0057** for the full rationale.
