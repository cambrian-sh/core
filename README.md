# Cambrian Core

**A governed runtime for AI agents.** Cambrian Core is the kernel that runs agents on a person's behalf: it gives an agent that person's tools and a shared long-term memory with fail-closed access control, lets it fan work out to parallel read-only subtasks, records what it relied on, and keeps a human in the loop for anything consequential. All of it is metered, auditable and configured by data, not code.

> **Status: pre-1.0 alpha (`v0.6.x`).** The gRPC/proto surface and the config schema are held stable. The Go package API is not. Expect breaking changes between minor versions.

---

## What it is

Cambrian is an orchestration kernel written in Go. Agents are not linked into it. They are separate processes, written in any language and usually Python through the [Agent SDK](https://github.com/cambrian-sh/agent-sdk-python), that the kernel spawns, authenticates and talks to over gRPC. The kernel owns everything between "a request arrived" and "an answer went back":

```
 person / client ──► conversation (OpenConversation / SendTurn) or Execute
                          │
                          ▼  execution.lane.default
        ┌─────────────────┴──────────────────────────────┐
        │ single_agent (default, ADR-0150)               │ planner (research, behind config, ADR-0145)
        ▼                                                ▼
  one agent holding the person's tools          Router → Planner (LLM) → ExecutionPlan (DAG)
        │  └─ run_subtasks ─► parallel read-only        → Gatekeeper → Dispatcher → agent per step
        │     child agents (depth 1)                    → coverage · verifier
        ▼                                                ▼
  agent process (gRPC agent plane) ─┬─ memory_query / recall ──► Memory (pgvector + KG)
                                    └─ tool_call            ──► kernel tools · MCP servers
                          │
                          ▼
              Approvals · Spend ledger · Run ledger · Event log
```

## What it aims for

- **One agent by default, fan-out when it helps.** A request runs in the single-agent lane: one agent holds the person's tools and answers itself, and can split read-only work into parallel subtasks the kernel runs on child leases (`run_subtasks`). The multi-agent planner lane — an LLM planner, a DAG of steps, and capability-typed dispatch (Gatekeeper eligibility, then merit ranking; no auction) — is kept behind `execution.lane.default: planner` for research.
- **Policy is data, not code.** Policy, routing tables and thresholds live in layered config, never in a Go branch on an agent, tool or customer (the *Zero-Hardcode Rule*).
- **Memory that is shared but governed.** One long-term memory serves every agent: hybrid dense and lexical retrieval, a knowledge-graph layer, structure-aware ingestion and event time. Every read and write passes through one composition point that enforces access scope and fails closed. An agent sees only what its grants allow.
- **The operator stays in charge.** Plans can require operator approval. A dedicated operator plane (gRPC `OperatorConsole`) drives the [operator console UI](https://github.com/cambrian-sh/ui) and exposes configuration, approvals, agents, memory and the live event feed.
- **Every LLM call is accounted for.** One spend ledger meters all calls with provider-reported usage, attributes them to a session, and can enforce a per-session token ceiling.
- **Configuration is data.** Thresholds, routing tables and policies live in layered config, and operators can change many of them at runtime without a restart.
- **Source-available core, commercial plugins.** This module is source-available under the Elastic License 2.0 (not an OSI open-source licence) and contains no proprietary code. Commercial features plug in from a separate module through one seam (`app.Options`), and CI checks that the boundary holds.

## Key concepts

| Concept | Meaning |
|---------|---------|
| **Execution lane** | How a request runs: `single_agent` (default) or `planner` (`execution.lane.default`, ADR-0150). |
| **Knowledge substrate** | Evidence (original bytes, stored before any semantic step), ontology kinds, object tables and the query plane (ADR-0105–0111, 0147, 0149). |
| **`internal/substrate`** | Legacy package name for the gRPC server, sessions and event log — the kernel's network layer, not the knowledge substrate. |
| **Awareness / Planner** | Planner lane only: the LLM layer that turns a request into an `ExecutionPlan`, a DAG of steps. |
| **Metabolism** | Agent lifecycle (AgentManager) and, in the planner lane, dispatch: Gatekeeper, Dispatcher, DAG executor. |
| **Agent plane** | The authenticated gRPC contract between kernel and agents (`api/proto/cambrian.proto`). |
| **Operator plane** | The gRPC contract for humans and the UI (`api/proto/operator.proto`). |
| **Memory stack** | Long-term memory: Postgres + pgvector, chunking and ingestion, KG²RAG retrieval, all behind enforcing stores. |
| **Supervision** | Verifier pool, watcher and approval hub, which check outputs and gate plans. |

## Quickstart

**Requirements:** Go 1.25+, Docker (for the bundled Postgres; or bring your own Postgres with the `pgvector` extension), Python 3 for the bundled agents, and at least one LLM provider key.

```bash
# Build the kernel
go build -o bin/cambrian-orchestrator ./cmd/orchestrator

# Install and start: provisions Postgres (Docker), the Python agent runtime,
# config files, runs migrations, mints agent credentials, starts the kernel
./bin/cambrian-orchestrator setup          # interactive; --yes for defaults

./bin/cambrian-orchestrator status         # is it running and healthy?
./bin/cambrian-orchestrator stop
```

`setup` installs into `$CAMBRIAN_HOME` (default `~/.cambrian`). Useful flags: `--yes`, `--no-start`, `--wait`, `--skip-models`, `--skip-python`, `--skip-js`. Run `setup --help` for all of them.

Other subcommands:

| Command | Purpose |
|---------|---------|
| `cambrian-orchestrator` | Run the kernel in the foreground |
| `migrate up` / `migrate status` | Apply or inspect database migrations |
| `config overrides list` / `unset` | Inspect or clear durable config overrides (why a file edit "didn't take") |
| `mcp …` | Manage inbound-MCP client tokens and run the stdio bridge for local MCP clients |

To write your own agent, see the [Agent SDK quickstart](https://github.com/cambrian-sh/agent-sdk-python#quickstart). An agent is a small Python class, and the kernel spawns it and dispatches tasks to it.

## Configuration

Config is loaded in 12 layers, lowest priority first:

1. built-in defaults
2. `configs/tuning.json` (committed starter)
3. `tuning.local.json`
4. `config.json`
5. `config.local.json`
6. `embedder.json`
7. `embedder.local.json`
8. `providers.json`
9. `providers.local.json`
10. `mcp.json`
11. the embedded config store, which holds durable operator writes from the console
12. `CAMBRIAN_*` environment variables

Only `tuning.json` and the `*.example` templates are committed. Real config files and secrets are gitignored, and secrets come from `.env` or the environment. Telemetry is off by default.

## Repository layout

| Path | Role |
|------|------|
| `cmd/orchestrator/` | The kernel binary (thin shell over `app.Run`) |
| `cmd/…` | Operator-run maintenance tools (`config-migrate`, `embed-backfill`, `pagerank-recompute`, …) |
| `app/` | Composition root: `app.Run(ctx, opts)`; `app.Options` is the extension seam |
| `domain/` | Pure domain types and ports, with no infrastructure imports |
| `internal/kernel/` | Subsystem assembly (memory, metabolism and other stacks) |
| `internal/awareness/` | Planner |
| `internal/metabolism/` | Agent manager, dispatch, DAG executor |
| `internal/memory/` | Long-term memory, ingestion, retrieval |
| `internal/authz/` | Fail-closed access enforcement |
| `internal/substrate/` | gRPC server, sessions, operator plane |
| `internal/supervision/` | Verification, watcher, approvals |
| `internal/infrastructure/` | Adapters: Postgres, LLM providers, MCP (outbound and inbound) |
| `internal/config/` | Layered config loader and its schema migrations |
| `api/proto/` | The gRPC contracts (generated code; regenerate with `make proto`) |
| `agents/` | Bundled Python agents (chat, research, analyst, code, terminal, …) |
| `configs/` | Committed starter config and templates |
| `docs/` | Architecture overview and design notes |

The code follows a hexagonal architecture: ports in `domain/`, adapters in `internal/`, and wiring only in `app/` and `internal/kernel/`. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Development

```bash
go build ./... && go vet ./... && go test ./...
make test-race       # unit tests with the race detector
make per-pr          # the per-PR gate CI runs: proto checks, race tests, separability, integration, chaos
make separability    # verifies the open-core boundary (no premium imports)
make help            # every target
```

Contributor and agent conventions live in [`AGENTS.md`](AGENTS.md), and the current state of each subsystem is in [`CONTEXT.md`](CONTEXT.md). Architecture decisions are recorded as ADRs in the project's separate knowledge repository.

## The Cambrian project

| Repository | What it is | Licence |
|------------|-----------|---------|
| **core** (this repo) | The kernel and runtime; owns the proto contracts | Elastic License 2.0 |
| [sdk](https://github.com/cambrian-sh/agent-sdk-python) | Python Agent SDK (`cambrian-agent-sdk`) | Apache-2.0 |
| [ui](https://github.com/cambrian-sh/ui) | Operator console (Tauri desktop app) | AGPL-3.0 |
| premium | Commercial plugins (policy, reactive engine, receipts, …) that attach through `app.Options` | Proprietary |

## Licence

Cambrian Core is **source-available** under the [Elastic License 2.0](LICENSE). You may use, modify and self-host it. You may not offer it to third parties as a hosted or managed service, or circumvent its licence-key functionality. See [`LICENSE`](LICENSE) for the full terms.
