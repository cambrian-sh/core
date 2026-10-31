# CONTEXT.md — cambrian-core (OSS Go Kernel)

The OSS Go kernel — the Substrate runtime. Pre-1.0 (`v0.6.10-Alpha`);
proto + config are stable, Go API is not.

For agent rules see `AGENTS.md`. For layering and domain terms see
`docs/ARCHITECTURE.md`. For per-ADR status and measurements see
`cambrian-knowledge/adrs/` (indexed by the knowledge graph).

---

## Knowledge & work tracking

This file is the *current shape* of cambrian-core. It is not where decisions or work live:

- **Why it is this shape** — ADRs and decisions in `../cambrian-knowledge/`. Query the
  graph (`search_graph`, `query_graph`) rather than citing from memory; check an ADR's
  freshness before building on it.
- **What is being done, and what is broken** — tickets: `list_tickets` (`repo: "cambrian-core"`), or the
  knowledge-board Tickets view. Every change is ticketed, claimed, and closed with a
  resolution — the protocol is root [`AGENTS.md`](../AGENTS.md) §0.
- **Every gap listed in this file should have a ticket.** Put its id next to the gap
  (`— T-20261031-…`); when the ticket closes, update or remove the gap here in the same change.

## Build & Run

```bash
make build          # builds cmd/orchestrator
make test           # unit tests
make test-race      # unit tests with -race
make per-pr         # proto-check proto-breaking test-race separability integration chaos bench-micro
cambrian-core setup # self-installing: Postgres, Python runtime, agents, config, migrate up, start
cambrian-core status / stop
```

Canonical proto toolchain: `protoc v7.34.1 · protoc-gen-go v1.36.11 · protoc-gen-go-grpc v1.6.1`.

---

## Module Layout

The kernel is **strictly layered**. A breach (e.g. `domain/` importing
`internal/infrastructure/`) is caught by `scripts/check-no-premium.sh` and
`make separability`.

| Path | Role |
|---|---|
| `domain/` | Pure domain types and ports. All security-critical decisions live here with zero external imports. |
| `app/` | Composition root. `app.Run(ctx, opts)` wires subsystems; `app.Options` is the open-core extension seam. |
| `cmd/orchestrator/` | Thin `main` shell over `app.Run`. |
| `internal/kernel/` | Subsystem assembly: four stacks + `ProvideServer`. The only cross-subsystem wirer. |
| `internal/awareness/` | Planner / Cortex. Zero-Hardcode rule at the LLM layer. |
| `internal/metabolism/` | AgentManager, Gatekeeper, dispatch, DAG executor, A2A connector. |
| `internal/memory/` | LTM: pgvector, chunking pipeline, structure-aware ingestion, document entities, KG²RAG. |
| `internal/evidence/` | Knowledge substrate evidence write path (ADR-0105). |
| `internal/substrate/` | gRPC server, session, OperatorConsole plane, knowledge-graph extractor. |
| `internal/supervision/` | Gatekeeper, verifier pool, watcher (signal→inspiration). |
| `internal/authz/` | Access-control enforcement (ADR-0085). Fail-closed; contains no policy. |
| `internal/infrastructure/` | Adapters: `postgres/`, `llm/`, `mcp/` (outbound), `mcpserve/` (inbound MCP endpoint). |
| `internal/reactive/` | Reactive rule engine plug surface (premium provides the implementation). |
| `internal/chat/` | OSS turn path: pool-dispatched, retry-safe, planner-free. |
| `internal/storage/` | BBolt adapter + durable reactive journal (ADR-0061). |
| `internal/config/` | Koanf 12-layer loader (ADR-0024). |
| `internal/migrate/` | Pure-Go DB migration runner (ADR-0064). |
| `internal/agentplane/` | Agent-plane gRPC connection pool (renamed from auctioneer by ADR-0100 P3). |
| `internal/agentpool/` | Bounded pool of interchangeable daemon workers (ADR-0084 D4). |
| `internal/tool/` | System-tool lifecycle (Python, `ToolRegistry`, confined `ProcessHandler`). |
| `internal/health/` | gRPC health check (PLAT-03 / ADR-0065). |
| `internal/telemetry/` | OTel bridge — the only package importing OTel. |
| `api/proto/` | The gRPC/protobuf contract: `cambrian.proto`, `operator.proto`. |
| `agents/` | Production Python agents auto-discovered by BBolt. |
| `scripts/` | Build, test, CI helpers. |
| `configs/` | Committed starter config; real `*.json` files are gitignored. |

---

## Key Invariants

- **Hexagonal boundary**: every layer below `domain/` is replaceable from its ports.
- **Premium never leaks in**: `check-no-premium.sh` audits imports.
- **Memory is an Engram engine**: typed documents, two-tier write pipeline, graph layer (BFS with 0.75^depth attenuation), content-addressed `ArtifactVault`.
- **Dispatch is unconditional**: ADR-0100 P3 deleted the auction; selection is capability-typed.
- **Config store layer 11**: durable operator writes in bbolt (ADR-0101).
- **Contributed machine tools are opt-in per agent**: an agent reaches a user's machine only through an explicit `local:*` (or exact `local:<machine>/<tool>`) grant; ownership picks the machine (ADR-0127 CL-4, `domain/contribution.go` `grantCovers`).
- **`app.KernelServices` is the plugin capability seam**: premium receives functions, not internals — e.g. `VerifyMachineToken` lets the `machine_token_v1` ingress verifier check a token without reading it (ADR-0127 CL-5).
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start.

---

## Known gaps

Each has a ticket; close the ticket and update this list in the same change.

- Three `cmd/` maintenance tools write through the raw vector adapter —
  `T-20261031-three-cmd-maintenance-tools-write-through-the`
- `nightly` and `release-gate` are not run by CI — `T-20261031-nightly-and-release-gate-checks-are-not-run-by`
- No automated check that vendored proto copies match core —
  `T-20261031-no-automated-check-that-vendored-proto-copies`
- ADR-0121 residuals (D8 policy suite, `policy_note`) — `T-20261031-adr-0121-follow-ups-d8-policy-suite-and-query`
- ADR-0128 residuals (deferred crosswalks, resolver lane) — `T-20261031-adr-0128-follow-ups-deferred-crosswalks`
- Contribution lane never run end to end on real machines (blocked: no deployment) —
  `T-20261031-contribution-lane-has-never-run-end-to-end-on`
- Handshake/MCP version literal (`0.6.9-alpha`) disagrees with the version above —
  `T-20261031-kernel-reports-version-0-6-9-alpha-while`
- Derived memory artifacts (`chunk_triplets`, …) carry no classification; contained only
  because their text is never surfaced — `T-20261031-derived-memory-artifacts-carry-no`
- No customer data export — `T-20261031-customers-cannot-export-their-data`
