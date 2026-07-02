# `app` — Composition Root

The `app` package is the **only place in the codebase where every subsystem is wired together**. It is the composition root for the Cambrian runtime.

`cmd/orchestrator/main.go` is a 35-line shell that calls `app.Run(ctx, app.DefaultOptions())`. All real bootstrap logic lives here.

The package is intentionally importable so a downstream (premium) binary can reuse the same bootstrap and inject proprietary components through `app.Options` (ADR-0057, Model C). OSS builds use identity defaults; premium builds swap in Langfuse tracing, agent-call logging, and the ReactiveEngine.

## Files

| File | Purpose |
|---|---|
| `app.go` | The `Kernel` struct, `Run`, `bootstrapKernel`, `startKernelServices`, reconcilers, MCP sink, helpers |
| `options.go` | `Options` injection seam + `ReactiveServices` capability bundle + `DefaultOptions` |
| `*_test.go` | In-process micro-benchmarks and unit tests. These stay in-tree. Black-box / integration benchmarks live in the sibling `../cambrian-benchmarks` repo. |

---

## Core types

### `Options` (`options.go`)

The OSS-exported injection surface. Three hooks, all nil-safe:

```go
type Options struct {
    TraceWrapper     func(Generator, string) Generator    // Langfuse in premium, identity in OSS
    AgentCallLogger  subnetwork.AgentCallLogger           // nil in OSS
    NewSignalReceiver func(ReactiveServices) (...)        // nil in OSS -> uses the Watcher
}
```

`DefaultOptions()` returns the OSS defaults: identity trace wrapper, no agent-call logging, no reactive engine (the Watcher is used).

### `ReactiveServices` (`options.go`)

The capability bundle handed to the premium reactive hook. Every field is an interface so premium never depends on kernel internals:

```go
type ReactiveServices struct {
    Manager    ReactiveAgentDispatcher  // direct dispatch + daemon lifecycle
    Auctioneer domain.Auctioneer        // full Gatekeeper -> Auction
    Memory     ReactiveMemoryWriter     // async LTM ingest
    Planner    ReactivePlanner          // plan generation for start_plan actions
    LLM        domain.Generator         // LLM condition evaluation
    WatchStore ReactiveWatchStore       // WatchConfig persistence (BBolt)
    EventBus   domain.EventBus          // daemon-crash subscription, emit_event
}
```

This is the spike-validated seam (ADR-0057 D14): the reactive engine + executors + watch handler are buildable from this bundle alone.

### `Kernel` (`app.go:61-99`)

The container that holds every wired subsystem. After the 2026-05-11c stack refactor it carries only:

- **Infrastructure**: `Config`, `Registry`, `Store`, `Listener`, `Server`, `GRPC`
- **Domain stacks**: `Memory`, `Awareness`, `Metabolism`, `Supervision`
- **Runtime handles** + ADR-0012 synaptic bridge components (`SessionMgr`, `EventLogger`, `SynapticWatcher`, `CircadianRhythm`, `MemoryLifecycleMgr`, `ArtifactVault`, `EventBus`)
- **Cross-cutting** (ADR-0034 / 0039 / 0043 / 0047): `ScopeResolver`, `ToolGrants`, `MCPConnector`, `OperatorEffects`, `OperatorAudit`

`Kernel.Shutdown(ctx)` (line 102) tears down in reverse dependency order: gRPC GracefulStop → listener → domain stacks (reverse) → synapse components → store close.

---

## Main flow: `Run` → `bootstrapKernel` → `startKernelServices`

### 1. `Run(ctx, opts)` — top-level entry (`app.go:159`)

1. `flag.Parse()`.
2. Load `.env` then `configs/config.json` (7-layer config pipeline from ADR-0024).
3. Set up the **double-SIGINT force-quit handler**: first signal triggers graceful shutdown, second signal closes the BBolt store and `os.Exit(1)` to prevent corruption on a double-SIGTERM.
4. **Health check first**: bind the TCP port *before* anything else — fail fast.
5. Init logger.
6. Call `bootstrapKernel(...)` → `*Kernel`.
7. `defer k.Shutdown(...)` with a 10s timeout.
8. `errgroup.WithContext(rootCtx)` and call `startKernelServices(g, gCtx, k)`.
9. `g.Wait()` — blocks until every goroutine exits.

### 2. `bootstrapKernel` — the actual wiring (`app.go:232-1050`)

Strictly sequential, dependency-ordered:

| Step | What gets built |
|---|---|
| **0. OTel** | `initTelemetry` — TracerProvider + MeterProvider, no-op when all endpoints unset (ADR-0057 D11) |
| **1. Infrastructure** | BBolt store, pgvector adapter, scope store, audit store (Postgres with in-memory fallback), scope resolver, **LLM Provider** (ADR-0042), provider registry, embedder, telemetry bridge |
| **1a. Trace wrapper** | `llmProvider.SetTraceWrapper(opts.TraceWrapper)` — wraps every acquired generator at the Acquire chokepoint |
| **1b. Generators** | `memoryGen`, `awarenessGen`, `supervisionGen`, `metabolismGen` — one per organ purpose |
| **1c. Agent reconciliation** | `registerModelAgents` + `reconcileModelAgents` (prune `TraitModel` not in config) + `reconcileFilesystemAgents` (prune agents whose source file is gone) |
| **2. Domain stacks** | `MemoryStack`, `AwarenessStack`, `MetabolismStack`, `SupervisionStack` — wired in dependency order |
| **2a. Scope wiring** | `QueryService.EnableScoping(...)` (Phase 1), `EnablePhase2(...)` (Phase 2 caller_scope), `ContentStore` set on `mem.Agent` (ADR-0048) |
| **2b. Optional recall** | KG²RAG (ADR-0053), multi-signal blend (ADR-0054), hybrid dense+lexical, cross-encoder rerank — all config-gated |
| **3. EventBus** | Single `InMemoryEventBus` shared by all stacks; subscribes for `AgentReady` logging and scope promotion (ADR-0034 D11) |
| **4. Watcher** | ADR-0009 proactive signal processor |
| **5. SessionManager** | ADR-0012 episodic memory lifecycle |
| **6. EventLogger + SynapticWatcher** | Unified event stream + high-priority → LTM ingest |
| **7. Episodic extraction** | `EpisodicExtractor` + `episodicConsolidator` + `MemoryLifecycleManager` (ADR-0029 / 0030) |
| **8. CircadianRhythm** | Session token eviction (ADR-0018 sweep) |
| **8a. LLM Gateway** | `subnetwork.NewLLMGateway` with streaming clients for **every** configured generator (Ollama + OpenAI + Anthropic). Registers `llm:<id>` clients, sets `DefaultModelID` |
| **9. ArtifactVault** | Content-addressable storage at `dataDir/vault` |
| **10. Server** | `kernel.ProvideServer(...)` — the consumer that ties all stacks to the gRPC surface |
| **10a. Resource selector** | EFE selector wired when `resource_selector ∈ {efe, auto}` and Gatekeeper is present (ADR-0037) |
| **10b. Tool system** | Native tool discovery from `tools/`, MCP connector (ADR-0043), pricing / budget / egress audit, `ToolExecutor` with grants + approval controller + scope + CAS + artifact promotion + tool output curation |
| **10c. Tool index** | `ToolIndexer.IndexAll(...)` + `reconcileIndex` (ADR-0044 prunes orphaned tool docs) |
| **10d. Vector retriever** | `VectorToolRetriever` on the `ToolExecutor` (semantic `find_tools`) |
| **10e. Scout** | ADR-0051 pre-plan discovery agent — wired when `scout_enabled`; `scout_agent` confined to `discovery_safe` tools (D6) |
| **10f. KG extractor** | ADR-0053 D2 — `kg_extractor_agent` replaces LLM residue when `kg_extractor_enabled` |
| **10g. Skill system** | ADR-0046 — `LoadRegistry("skills", ...)` + `SkillIndexer` + scope-aware `VectorSkillRetriever` |
| **10h. MCP sink** | `mcpToolSink` — keeps registry + retrieval index in sync as MCP servers drop / reconnect |
| **10i. Operator effects** | `CommandEffectsFuncs` binds the operator-plane mutations: `TagMemory` (controlled vocab), `SetScope`, `RegisterSkill`, `RegisterMCP`, `TriggerConsolidation`, `SetRuntimeConfig` (hot blend-weight tuning) |

### 3. `startKernelServices` — the parallel workers (`app.go:1052-1292`)

All started in an `errgroup.Group`, so any panicking goroutine fails the whole runtime:

| Group | Worker |
|---|---|
| A. Domain stacks | `k.Memory.Start`, `k.Metabolism.Start`, `k.Supervision.Start` |
| B. Synaptic bridge | `SynapticWatcher`, `CircadianRhythm`, `MemoryLifecycleMgr` |
| B+. Scope | `WatchInvalidations` on LISTEN / NOTIFY (ADR-0034 cross-replica) |
| B++. MCP | `MCPConnector.Watch(...)` — health / reconnect (ADR-0043 D8) |
| B+++. Backfill | `backfill.RunInterviewBackfill` — brain integrity verification |
| C. gRPC | `grpc.NewServer` with method-scoped `UnaryAuthInterceptor` / `StreamAuthInterceptor` (ADR-0047 D13), registers `OrchestratorServer` + `OperatorConsoleServer`, hooks `TokenSink` for managed-proxy chunk streaming |
| D. Ingestion HTTP | opt-in via `IngestionHTTPPort > 0` — `/v1/ingest`, `/v1/admin/consolidate`, `/v1/admin/agents/{id}/{scope\|write-tags\|tool-grants}` (ADR-0028 / 0034 / 0035 / 0039) |

---

## Supporting helpers (in `app.go`)

| Helper | Role |
|---|---|
| `mcpToolSink` (line 1298) | Keeps the tool registry + ADR-0044 retrieval index in step as MCP servers drop and reconnect. `Seed` on boot, `SetServerTools` on resync, `RemoveServerTools` on permanent drop |
| `registerModelAgents` | Registers `TraitModel` agents (`llm:<id>` keyed). Idempotent upsert |
| `reconcileModelAgents` | Evicts `TraitModel` agents no longer declared in `config.Generators` (config is the source of truth; registry is upsert-only, so eviction needs an explicit reconcile pass — the "qwen-after-removal orphan bug") |
| `reconcileFilesystemAgents` | Evicts agents whose local source file is gone. Provenance-scoped: `ExecPath != ""` AND `Runtime != A2A` AND `Trait != Model` |
| `reconcileIndex` | Generic pruner for `DocTypeTool` / `DocTypeSkill`; `toolKeepFunc` preserves transient-unreachable MCP tools across boot outages |
| `episodicConsolidator` | Adapts `EpisodicExtractor` to `circadian.SessionConsolidator` with the configured consolidation delay (ADR-0029 / 0030) |
| `initTelemetry` | Opt-in OTel, silent by default |
| `operatorBootstrapIdentity` | Seeds a single operator from `CAMBRIAN_OPERATOR_USER` / `_PASSWORD` / `_ROLE`. No env ⇒ no login (secure-by-default, ADR-0047 D13) |
| `stringSliceFromMeta` / `applyTag` | Small utility coercers used by the `TagMemory` operator effect (ADR-0047 0047-25 / A1.2) |

---

## Summary

`/app` is the **only place in the codebase where everything is glued together**. It loads config, builds the four domain stacks + LLM gateway + tool system + operator plane, wires the gRPC server, starts the background workers, and exposes the Open-Core injection seam (`Options`) that lets a premium binary swap in Langfuse tracing, agent-call logging, and the ReactiveEngine without forking the kernel.
