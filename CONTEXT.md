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
| `internal/metabolism/` | AgentManager, Gatekeeper, dispatch, DAG executor, A2A connector, `yield/` (consumer of agent-yielded sub-goals: decorates the Dispatcher as `MetabolismStack.Selector`, binds the sub-goal through dispatch, resumes the yielding agent; ADR-0041 D5 / ADR-0100 D10). |
| `internal/memory/` | LTM: pgvector, chunking pipeline, structure-aware ingestion, document entities, KG²RAG. |
| `internal/evidence/` | Knowledge substrate evidence write path (ADR-0105). |
| `internal/substrate/` | gRPC server, session, OperatorConsole plane, knowledge-graph extractor. |
| `internal/supervision/` | Gatekeeper, verifier pool, watcher (gates SignalStream; in OSS plans each accepted signal once and queues the plan for operator approval on the ApprovalHub, running it via `domain.PlanExecutor` only if approved). |
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
| `internal/health/` | gRPC health check + the `/healthz` handler `app` serves under the listener policy (PLAT-03 / ADR-0065, ADR-0130 D3). |
| `internal/ratelimit/` | Per-principal token-bucket interceptors on both gRPC planes (`server.rate_limit`; ADR-0047 / ADR-0130). |
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
- **`app.KernelServices` is the plugin capability seam**: premium receives functions, not internals — e.g. `VerifyMachineToken` lets the `machine_token_v1` ingress verifier check a token without reading it (ADR-0127 CL-5). `PlanExecutor` (`domain.PlanExecutor`, `app/plan_executor.go`) runs a plugin-generated plan on the real DAG executor via the Orchestrator.Execute authored-plan branch — the premium `start_plan` action uses it (PREM-0032).
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start — and, since ADR-0130, mints each discovered agent's plane credential (`agent:<id>:token`) into the ADR-0101 store.
- **The agent plane authenticates and every listener shares one policy** (ADR-0130, 2026-11-01). Every `Orchestrator` RPC and every premium service declared agent-facing requires the per-agent named secret as `authorization: Bearer` with `x-agent-id` as the assertion it must match (`internal/authz/agent_auth.go`; one constant-time compare, `authz.VerifyNamedSecret`, shared with the MCP endpoint); the principal is established there, never from the header alone. The kernel mints missing credentials at boot and injects `CAMBRIAN_AGENT_TOKEN`/`CAMBRIAN_AGENT_ID` into each spawned agent's environment (`agentmgr/agent_credential_env.go`, the one credential SEC-01 admits). `server.insecure_localhost=true` waives agent auth (D2) — the benchmark harness runs under it. Every HTTP listener is built by `app/listener_security.go` (`serveHTTP`; plugins use `app.ServeHTTPListener`): loopback default, routable-bind refusal without TLS, header/idle timeouts, a body cap; `/v1/admin/*` takes the operator bearer through the same `OperatorIdentity` as gRPC (`app/admin_auth.go`), and `app/listener_policy_test.go` refuses any other `http.Server` literal. Operator `Login` locks a peer out after 5 failures in 15 min and tokens expire after 12 h (`operator/login_limiter.go`); every attempt is logged and handed to `domain.LoginObserver` plugins (`Registry.AddLoginObserver` — the premium receipts plugin chains failures). The `/healthz` shim is a handler served by `serveHTTP`; only two operator-run tools (`cmd/pagerank-recompute`, premium `cmd/cambrian-runview`) stay on the listener allowlist, each with an ADR citation. Spawned agents dial the address the listener policy bound, and on a pure-TLS bind receive `CAMBRIAN_TLS_CA` (+ `CAMBRIAN_TLS_SERVER_NAME`) beside their token (`app/agent_transport.go`). Both gRPC planes carry a per-principal token bucket after authentication (`internal/ratelimit`, `server.rate_limit`, default 200 req/s burst 400, `ResourceExhausted` on overflow).
- **Every LLM call is metered by one ledger** (ADR-0131 D1-D3, 2026-11-01). `llm.SpendMeter` (`internal/infrastructure/llm/spend_meter.go`) is fed by the two doors every call passes: the outermost decorator in `Provider.Acquire` (all organs) and `SubstrateLLMGateway` (all agent streams and tool turns, including the `retrieval_agent` chain, whose lease is now bound to the caller's session and charged back to the asking step's lease). Usage is provider-reported — adapters fill a ctx capture cell (`usage_capture.go`) from their own response — and `len/4` is used only as a fallback flagged `Estimated`. `execution.session.max_session_tokens` (> 0) refuses calls with `llm.ErrSpendCeiling`; the plan ends with gRPC `ResourceExhausted` and a `System_SpendCeiling` TaskEvent. Wired in `internal/kernel` `ProvideServer`. Each Execute response carries `_llm_*` spend metadata. Lease completion runs one gateway hook (`SetCompleteHook`), which clears ADR-0046 run grants for every lease site. One plan gets `1 + max_replan_attempts` planner calls in total (validation retries and replans share the budget, `plan_call_budget.go`).

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
- Spawned agents on a pure-TLS kernel get `server.tls_cert_file` as their TLS root, which fails for a CA-issued leaf (no agent-CA config key) —
  `T-20261101-spawned-agents-get-server-tls-cert-file-as`
- The premium ReactiveEngine ignores `DaemonQuarantinedEvent`, so `ListWatches` cannot show a quarantined stream (core now shows it on the operator feed) —
  `T-20261101-premium-reactiveengine-ignores`
- ADR-0130 deferrals to contract bump 0096: `/v1/admin/*` → `OperatorConsole` RPCs, an explicit `Logout` RPC.
- Contribution lane never run end to end on real machines (blocked: no deployment) —
  `T-20261031-contribution-lane-has-never-run-end-to-end-on`
- Derived memory artifacts (`chunk_triplets`, …) carry no classification; contained only
  because their text is never surfaced — `T-20261031-derived-memory-artifacts-carry-no`
- No customer data export — `T-20261031-customers-cannot-export-their-data`
- ADR-0131 D4-D6 deferred (benchmark phase): spend in receipts/operator feed/UI —
  `T-20261101-adr-0131-d6-spend-totals-in-the-plan-end`; per-purpose ceilings —
  `T-20261101-adr-0131-d3-per-purpose-spend-ceilings`; planner cluster cache / prefix caching —
  `T-20261101-planner-rebuilds-the-capability-cluster-on`
- Step TaskEvents still bill `len/4` although the step lease returns provider usage —
  `T-20261101-step-taskevents-still-bill-len-4-although-the`
- Router classification and async organs (verification, tier-2, interview) are metered but not attributed to a session —
  `T-20261101-router-classification-and-async-organs`
- ADR-0052 entity routing is armed by default (`recall_spreading_enabled`) but its in-memory
  `EntityIndex` is never populated, so the lane is a no-op —
  `T-20261031-entityindex-is-never-populated-so-entity`
