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
- **What has been measured** — benchmark runs in `../cambrian-knowledge/benchmarks/`
  (`list_benchmarks` by `tag`/`adr`, or the knowledge-board Benchmarks page). Every benchmark
  run is recorded there, with a verdict — the procedure is root [`AGENTS.md`](../AGENTS.md) §0.5.
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
- **Config store layer 11**: durable operator writes in bbolt (ADR-0101). Since D8/D9 (2026-11-01) a console write to a key a writable file layer already sets goes INTO that file (format-preserving, atomic; `internal/config/file_layers.go` `ResolveFileSetting`/`WriteFileSetting`, one `persist()` in `app/config_write_source.go` for every write RPC) and the store override is removed; committed `tuning.json` and read-only files are never rewritten — the value is stored and the ack/log names the file it overrides. Boot WARNs once per store key that beats a different file value (`config.StoreOverrideReport`, `app/config_store.go` `warnStoreOverrides`); `orchestrator config overrides list|unset` (`app/config_cli.go`, offline, `list` opens the store read-only) inspects and clears overrides. Generators are stored per id (`llm_provider.generator_overrides.<id>`, tombstone `removed:true`) and merged by id over the file list (`config.MergeGenerators`); a pre-D9 stored whole list is migrated at boot. The console marker / ack fields need a proto bump — T-20261101-config-write-outcome-does-not-carry-d8-written.
- **Contributed machine tools are opt-in per agent**: an agent reaches a user's machine only through an explicit `local:*` (or exact `local:<machine>/<tool>`) grant; ownership picks the machine (ADR-0127 CL-4, `domain/contribution.go` `grantCovers`).
- **`app.KernelServices` is the plugin capability seam**: premium receives functions, not internals — e.g. `VerifyMachineToken` lets the `machine_token_v1` ingress verifier check a token without reading it (ADR-0127 CL-5). `PlanExecutor` (`domain.PlanExecutor`, `app/plan_executor.go`) runs a plugin-generated plan on the real DAG executor via the Orchestrator.Execute authored-plan branch — the premium `start_plan` action uses it (PREM-0032).
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start — and, since ADR-0130, mints each discovered agent's plane credential (`agent:<id>:token`) into the ADR-0101 store.
- **The agent plane authenticates and every listener shares one policy** (ADR-0130, 2026-11-01). Every `Orchestrator` RPC and every premium service declared agent-facing requires the per-agent named secret as `authorization: Bearer` with `x-agent-id` as the assertion it must match (`internal/authz/agent_auth.go`; one constant-time compare, `authz.VerifyNamedSecret`, shared with the MCP endpoint); the principal is established there, never from the header alone. The kernel mints missing credentials at boot and injects `CAMBRIAN_AGENT_TOKEN`/`CAMBRIAN_AGENT_ID` into each spawned agent's environment (`agentmgr/agent_credential_env.go`, the one credential SEC-01 admits). `server.insecure_localhost=true` waives agent auth (D2) — the benchmark harness runs under it. Every HTTP listener is built by `app/listener_security.go` (`serveHTTP`; plugins use `app.ServeHTTPListener`): loopback default, routable-bind refusal without TLS, header/idle timeouts, a body cap; `/v1/admin/*` takes the operator bearer through the same `OperatorIdentity` as gRPC (`app/admin_auth.go`), and `app/listener_policy_test.go` refuses any other `http.Server` literal. Operator `Login` locks a peer out after 5 failures in 15 min and tokens expire after 12 h (`operator/login_limiter.go`); every attempt is logged and handed to `domain.LoginObserver` plugins (`Registry.AddLoginObserver` — the premium receipts plugin chains failures). The `/healthz` shim is a handler served by `serveHTTP`; only two operator-run tools (`cmd/pagerank-recompute`, premium `cmd/cambrian-runview`) stay on the listener allowlist, each with an ADR citation. Spawned agents dial the address the listener policy bound, and on a pure-TLS bind receive `CAMBRIAN_TLS_CA` (+ `CAMBRIAN_TLS_SERVER_NAME`) beside their token (`app/agent_transport.go`). Both gRPC planes carry a per-principal token bucket after authentication (`internal/ratelimit`, `server.rate_limit`, default 200 req/s burst 400, `ResourceExhausted` on overflow).
- **Every LLM call is metered by one ledger** (ADR-0131 D1-D3, 2026-11-01). `llm.SpendMeter` (`internal/infrastructure/llm/spend_meter.go`) is fed by the two doors every call passes: the outermost decorator in `Provider.Acquire` (all organs) and `SubstrateLLMGateway` (all agent streams and tool turns, including the `retrieval_agent` chain, whose lease is now bound to the caller's session and charged back to the asking step's lease). Usage is provider-reported — adapters fill a ctx capture cell (`usage_capture.go`) from their own response — and `len/4` is used only as a fallback flagged `Estimated`. `execution.session.max_session_tokens` (> 0) refuses calls with `llm.ErrSpendCeiling`; the plan ends with gRPC `ResourceExhausted` and a `System_SpendCeiling` TaskEvent. Wired in `internal/kernel` `ProvideServer`. Each Execute response carries `_llm_*` spend metadata, router classification included. Attribution is a property of the call's ctx on the same meter (owner decision 2026-11-01): a session in ctx is booked and enforced; `domain.WithBackgroundSpend` + the originating session (tier-2 scoring per session, the verifier call via `VerificationRequest.SessionID`) is booked, never refused — for gateway calls that property rides the lease: a lease `Acquire`d under the marker is `BudgetLeaseState.Background`, admitted without the ceiling check and booked to its bound session, which is how each verifier call's lease (minted by `VerificationWorker` through the constructor-injected gateway, bound to the originating session, completed after the call) is metered; Execute routes under the caller's existing live session (`liveCallerSession`, never creates one) or on an `llm.WithSpendTab` tab settled into the session it opens; anything else lands in the reported, unenforced kernel bucket `llm.NoSessionBucket` (`kernel:no-session`, logged by the gateway's eviction sweep). Step TaskEvents bill the step lease's provider usage (`llm.StampStepUsage` → `domain.HandoffKeyStep*` → DAG executor), with `TaskEvent.UsageEstimated` flagging the `len/4` fallback. Lease completion runs one gateway hook (`SetCompleteHook`), which clears ADR-0046 run grants for every lease site. One plan gets `1 + max_replan_attempts` planner calls in total (validation retries and replans share the budget, `plan_call_budget.go`).
- **Operator contract 0099** (2026-11-01, the bundled bump; `app/app.go` `SetHandshake`). Agent plane: the retired
  `Orchestrator` watch-CRUD RPCs and their messages are deleted. Operator plane: `Logout` (ADR-0130 D5;
  `OperatorIdentity.Revoke`), ADR-0101 D8 file facts on `ConfigWriteOutcomeOp`, and ADR-0134 event time. The HTTP
  `/v1/admin` routes with an OperatorConsole equivalent (consolidate, scope, tool-grants) are gone.
- **Event time in memory is one vocabulary and one reader** (ADR-0134, `domain/event_time.go`). `time_source` /
  `time_precision` are defined there and only there; both ingest planes resolve `occurred_at` / `parts` /
  `time_precision` through `ResolveIngestEventTime` (UTC, future times clamped beyond
  `execution.ingestion.event_time_future_tolerance_ms`, default 5 min) onto `ExternalDocument.Timestamp` + qualifiers;
  every hit mapping and the Stage-A recency term read `HitEventTime`. Storage (P0): migration `0019_event_time.sql`
  adds nullable `event_time`, `event_time_end`, `time_source`, `time_precision`, `claimed_time` to documents, chunks and
  the descriptor tables (no backfill; a NULL reads as `created_at AT TIME ZONE 'UTC'`, the ingest clock, labelled
  `ingest_default`); `domain.EventStamp` rides `Document`/`SourceDocument`; every chunk (structure leaves included) gets
  the earliest/latest time of the parts it covers (`LocateChunkSpans` + `ChunkStamps`); the upsert keeps a stated time
  over an ingest default (`keepStatedTime`, owner Q6). The two window b-trees are built CONCURRENTLY by the migration
  runner's out-of-transaction step (`migrate.ensureConcurrentIndexes`). Query (P2): `reference_time` / `event_after` /
  `event_before` ride the context (`WithEventTimeQuery`); the window goes into SQL beside the read scope on the dense
  and lexical lanes (`eventWindowExpressions`, span overlap, from `migrate.EventStartExpr/EventEndExpr`) and is
  re-asserted where every lane converges (`EventTimeQuery.Admits`); `reference_time` anchors the recency term (a fact
  after the asker's now gets no recency credit). MCP `search_memory`/`ask_memory` take the same parameters. Retrieval
  decisions carry the event-time query (`RetrievalDecision.EventTime`, for receipts). Default-off:
  `execution.retrieval.chronological_context_enabled` (D5a: final-synthesis passages ordered and dated by event time).
  Default-ON since 2026-11-02 (owner decision): `execution.retrieval.hnsw_iterative_scan = relaxed_order` (pgvector
  iterative scan against filtered-HNSW underfill, because every principal read is scope-filtered) and
  `execution.retrieval.blend_weight_recency = 0.1` (was 0; a starting value for the event-time A/B to tune).
- **Ingestion never silently drops content** (2026-11-02, T-20261102-ingestion-structure-junk-filter-silently-drops,
  ADR-0060). The `docling_agent` only CLASSIFIES OCR/layout junk (`junk_mode=mark` → `meta.junk_reason`); the kernel
  (`internal/memory/structure_junk.go` `applyStructureJunkFilter`) drops a classified leaf only when its parser backend
  is listed in `execution.ingestion.structure_junk_filter_backends` (default `["docling"]`; `markdown`/`text`, i.e.
  every chat ingest, keep everything). Each drop is logged with doc id, kind, reason, length, sha256 and a sample
  (WARN per document, DEBUG per leaf) and counted (`dropped_leaves` on the ingest-complete INFO line,
  `IngestionManager.StructureLeavesDropped()`). A sidecar request without `junk_mode` keeps the legacy drop.
- **Per-stage ingest timing** (2026-11-02, T-20261102-ingestion-logs-no-per-stage-timing-so-the-1-s, ADR-0060).
  `domain.StageClock` rides ctx (nil-safe); `ProcessSync` and the async `processBatchDoc` log one `ingest timing` line
  per document (DEBUG; INFO `(SLOW)` over 1 s or `(FAILED)`) with `<stage>_ms` for evidence, scene, chunk,
  document_row, structure_parse / structure_agent (the docling_agent's own `parse_ms`) / structure_wait, prepare,
  embed, chunk_save (the pg adapter adds begin/upsert/projection/commit), triplets_enqueue, structure_persist, entity,
  publish, plus `embed_calls` / `embed_requests` / `chunks` (`internal/memory/ingestion_timing.go`). `IngestMemory`
  returns the same breakdown as the `cambrian-ingest-timing` gRPC trailer (diagnostic, not a contract). Measured: a
  LongMemEval session costs 1076 ms with `database.host=localhost` vs 433 ms with `127.0.0.1` — on Windows Docker
  Desktop `::1` goes through wslrelay, where every client write over ~16 KB (each chunk upsert) stalls ~50 ms.
- **KG read-your-writes for sync ingest** (2026-11-02, T-20261102-async-kg-triplet-extraction-30-chunks-s-lags,
  ADR-0053). `execution.ingestion.sync_ingest_wait_for_kg` (default **false**) makes `IngestionManager.ProcessSync`
  return only after its own chunks' triplets are persisted: `persistChunks(..., trackKG)` enqueues them with
  `ChunkTripletsBatcher.EnqueueTracked`, which returns a per-document `TripletsWait`
  (`internal/memory/chunk_triplets_wait.go`; counter + done channel, no goroutine) that the existing drain loop settles
  per chunk. A pending tracked chunk makes the loop drain at once (up to batch_size, still across documents); untracked
  (async) enqueues are unchanged. The wait is bounded by ctx and `sync_ingest_kg_wait_timeout_ms` (30000); on timeout,
  caller cancel or batcher stop the ingest still succeeds with a WARN and `kg_pending#` / `kg_failed#` on the timing
  line and trailer; the wait itself is the `kg_wait` stage. Stop drains the backlog as before and releases anything
  left with `ErrTripletsBatcherStopped`. Boot log `SYNC_INGEST_WAIT_FOR_KG: ENABLED` (benchmark arm check). Measured
  (20 LongMemEval sessions, 2x2 concurrent): off 3.08 sessions/s and 0/249 chunks with triplets at return; on 1.80
  sessions/s, kg_wait mean 1.5 s, 248/248.
- **KG extraction failures are visible and retried** (2026-11-02,
  T-20261102-kg-extractor-cold-start-drops-the-first-kg, ADR-0053). `domain.TripletExtractor.ExtractBatch` returns
  `([][]ChunkTriplet, error)`; an error (wrapping `domain.ErrTripletExtractorUnavailable` when the agent could not be
  booted/dialled/called) settles the batch's chunks as FAILED in `TripletsWait` (`kg_failed#` on the timing line and
  trailer), never as "no triplets"; one WARN per batch (`ChunkTripletsBatcher: triplet extraction failed for a batch`).
  Unavailable chunks are re-queued to the tail of pending at most `execution.ingestion.kg_triplets_max_requeues` (1)
  times. `KgExtractorDispatcher` retries a failed call `kg_extractor_call_attempts` (3) times with
  `kg_extractor_retry_backoff_ms` (2000, doubling) under the batch ctx; bad/empty replies are not retried. The drain
  goroutine probes the extractor once at start (optional `domain.ReadinessProber`, an empty-batch round trip) and logs
  `KG extractor READY elapsed_ms=…` or WARN `KG extractor NOT READY` + cause; Stop cancels the probe; boot is never
  blocked. `InstanceManager.GetOrBootInstance` returns `(inst, error)` and WARNs `agent boot failed` with the cause
  (it used to return a bare nil); a tracked JIT instance whose process exited is evicted and re-booted, and an
  `Unavailable` Execute drops the transport's cached client, so a killed agent recovers on the next call. The agent
  boot timeout is `execution.agents.agent_boot_timeout_ms` (default 30000; the fixed 10 s killed the cold kg_extractor
  three times during kernel boot). Root cause and live numbers: ADR-0053 observations/measurements. Known noise: the
  interview backfill now WARNs `agent boot failed … exec: no command` for `llm:*` model agents (pre-existing, was
  silent; T-20261102-interview-backfill-tries-to-boot-llm-model).
- **Agentic decomposition spends only what a caller reads** (2026-11-02, ADR-0054 observations, prerequisites of
  ADR-0135). `decompSearch` (`internal/memory/query.go`, helpers in `query_decomp.go`): an echo plan (one sub-question,
  no `{n}`, equal to the question after normalisation, `isEchoPlan`) runs the single pass with no extraction call
  (trace `echo_single_pass`); the closing `Synthesize` call and the `_agentic_control` row are produced only when ctx
  carries `memory.WithAgenticSynthesis` — set by `QueryMemory` for `x-agentic-control` callers and by
  `QueryService.answer` (operator/scoped Answer); independent sub-questions (no unresolved `{n}`) run in parallel
  with the coverage-floor pass, bounded by `execution.retrieval.agentic_subquestion_concurrency` (default 4, `<=1`
  sequential), dependent ones after their answers, outcomes assembled in plan order so ranking equals the
  sequential loop. The extraction calls still queue at the single-threaded `retrieval_agent`
  (T-20261102-parallel-decomposition-extraction-calls-queue).
- **Adaptive decomposition routes** (2026-11-02, ADR-0135, behind `execution.retrieval.agentic_adaptive_routes_enabled`,
  default off, gate pending). With the flag on, `decompSearch` asks the planner for a routed plan
  (`RetrievalDispatcher.DecomposeRouted` → the `decompose` op with `"routes": true` → `planner.decompose_routed`, which
  returns `{route, sub_questions, refs}`; optional port `memory.RoutedDecomposePlanner`, composing `DecomposePlanner`)
  and executes it: `direct` = the single pass (trace `direct_single_pass`, no extraction), `facets` = floor plus
  independent sub-questions in one parallel wave, `chain` = today's grounded chain; a missing, unknown or inconsistent
  route (facets with `{n}`, direct with several sub-questions) runs as chain (`resolveDecompRoute`, `query_decomp.go`).
  The route is logged per recall (`agentic-trace decompose route`) and, for opted-in callers, written to the control
  row (`_agentic_route`, `_agentic_route_planned`, `_agentic_route_fallback`) and its trace. Flag off: the planner is
  sent today's request (legacy prompt, no route) and nothing else changes. The operator Answer path routes too.

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
- `/v1/admin/agents/{id}/write-tags` is the one HTTP admin route left (no OperatorConsole RPC sets write tags yet) —
  `T-20261101-write-tags-admin-route-has-no-operatorconsole`
- ADR-0134: relative time phrases in a question ("last month") are not resolved into an event window —
  `T-20261101-relative-time-phrases-in-a-memory-question-are`; filtered-HNSW underfill is measured only at 10k chunks
  (none on natural plans; the iterative-scan remedy is on by default since 2026-11-02, ~2x query latency at 10k) —
  `T-20261101-scoped-recall-may-underfill-top-k-on-a-large`; D5a chronological context and the recency term on real
  event times are unmeasured (LongMemEval A/B pending)
- Contribution lane never run end to end on real machines (blocked: no deployment) —
  `T-20261031-contribution-lane-has-never-run-end-to-end-on`
- Derived memory artifacts (`chunk_triplets`, …) carry no classification; contained only
  because their text is never surfaced — `T-20261031-derived-memory-artifacts-carry-no`
- No customer data export — `T-20261031-customers-cannot-export-their-data`
- `database.host=localhost` on Windows Docker Desktop costs ~50 ms per Postgres write over 16 KB (use `127.0.0.1`) —
  `T-20261102-kernel-postgres-writes-over-16-kb-stall-50-ms`; chunk upserts are one ~20 KB literal statement per row —
  `T-20261102-chunk-upserts-are-one-20-kb-literal`
- The async KG lane (one kg_extractor, ~30 chunks/s) lags concurrent sync ingest (~50 chunks/s), so a query right
  after ingest can see a partial KG — `T-20261102-async-kg-triplet-extraction-30-chunks-s-lags`
- The LongMemEval harness does not yet count structure-leaf drops per question (the kernel logs `dropped_leaves`
  per ingested document) — `T-20261102-longmemeval-harness-does-not-count-structure`
- ADR-0131 D4-D6 deferred (benchmark phase): spend in receipts/operator feed/UI —
  `T-20261101-adr-0131-d6-spend-totals-in-the-plan-end`; per-purpose ceilings —
  `T-20261101-adr-0131-d3-per-purpose-spend-ceilings`; planner cluster cache / prefix caching —
  `T-20261101-planner-rebuilds-the-capability-cluster-on`
- Verification now mints a session-bound background lease per verifier call (sent as `x-lease-id`
  gRPC metadata on `VerifyOutput`), but no agent serves `VerifyOutput` yet — the SDK registers no
  servicer for it — so verification does no metered LLM work in practice —
  `T-20261101-no-agent-can-serve-verifyoutput-the-sdk-has-no`
- ADR-0052 entity routing is armed by default (`recall_spreading_enabled`) but its in-memory
  `EntityIndex` is never populated, so the lane is a no-op —
  `T-20261031-entityindex-is-never-populated-so-entity`
