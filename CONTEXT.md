# CONTEXT.md — cambrian-core (OSS Go Kernel)

The OSS Go kernel — the Substrate runtime. Pre-1.0 (`v0.6.10-Alpha`);
proto + config are stable, Go API is not.

For agent rules see `AGENTS.md`. For layering and domain terms see
`docs/ARCHITECTURE.md`. For per-ADR status and measurements see
`cambrian-knowledge/adrs/` (indexed by the knowledge graph).

---

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
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start.
- **`go.mod` declares `github.com/cambrian-sh/cambrian-runtime`** — a rename is pending.
