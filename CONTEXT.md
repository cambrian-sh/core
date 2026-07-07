# CONTEXT.md, cambrian-core (Substrate)

This file is the **manual for the OSS Go kernel**, scoped to `cambrian-core`. It
is the per-sub-repo source of truth for the Substrate: layering, module
responsibilities, current implementation status, and the load-bearing design
decisions that make the runtime run.

For agent-facing rules, see `AGENTS.md` in this directory. For layering and
domain terms, see `docs/ARCHITECTURE.md`. For the reasoning behind a decision,
see the relevant ADR in `docs/adr/`.

---

## Implementation Status

Status values follow the ADR vocabulary in `docs/adr/README.md`, condensed to
the 5 surface tokens used by the F-verifiers. The runtime is at
`v0.6.10-Alpha` (pre-1.0; proto + config are stable, Go API is not).

| Area | ADR | Status |
|---|---|---|
| DAG parallel execution, value-copy plan freeze | 0001 | Implemented |
| Self-healing, replan, negative edges, loop detection | 0005, 0010 | Implemented |
| Mid-execution semantic checkpoint (H1 gate) | 0013 | Implemented |
| Neuromodulator cost-aware routing (GatekeeperScore cost term) | 0011 | Implemented |
| Synaptic bridge (episodic write-into-readout) | 0012 | Implemented |
| Thalamic gating, workspace priming (cold-start fallback) | 0014 | Implemented |
| Engram engine: `activation_strength`, floor-multiplier re-rank, Tier-1/Tier-2, `DocType` taxonomy | 0015 | Implemented |
| Global Workspace Stage (SCENE+FACT, contradiction guard) | 0016 | Implemented |
| Spreading activation (GraphRAG over `document_edges`, `0.75^depth`) | 0017 | Implemented |
| Managed cognitive resource allocation (LLM gateway streaming) | 0018 | Implemented |
| System quality measurement (PlanEvent, RetrievalSession, ContradictionResolution) | 0021 | Implemented |
| Koanf config engine (11-layer pipeline, curated `tuning.json`) | 0024 | Implemented |
| Memory reform (retire `DocTypeMemory`, XML-tagged planner injection, live graph edges) | 0025 | Implemented |
| Plan template generalizer (Hippocampus promote/blacklist) | 0030 | Implemented |
| Universal input router + reactive rule engine plug | 0031, 0033 | Implemented |
| Tag-based isolation (Phase 1: agent-scope read/write enforcement) | 0034 | Implemented (residual: Phase 2 `caller_scope` wiring is HITL-gated, see Known Gaps) |
| Working-memory context hygiene (recall same-session filter, `summary` column, tool-card offload, trajectory cards, cid hydration) | 0048 | Implemented (residual: D2 spreading off by default, no `resolve` for `content_cid`, truncation heuristic, embed-summary-vs-full a flagged choice) |
| Verbal self-reflection (SDK-side, gated on the LRW recurrence gate) | 0052 | Implemented |
| Agent trait classification (Cognitive / Model; retired TraitTool) | 0058 | Implemented |
| Centralized LLM provider (health-guarded broker, circuit breaker, price ledger, failover ladder) | 0042 | Implemented (residual: model-failover requires ≥2 generators in config, see Known Gaps) |
| MCP tool provider (MCP server discovery, agent-unreachable approval, injection-scanned responses) | 0043 | Implemented (residual: tool price in menu + per-tenant budget + OAuth deferred) |
| Local Recurrent Workspace (typed working memory + recurrence gate in the SDK) | 0041 | Implemented (typed working memory + gate live in the Python SDK; ADR-0048 amends the prompt-condensation half) |
| Two-tier tool disclosure (`toolSummary` / `describe_tool`) | 0045 | Implemented (extends 0044) |
| Agent skills (system + agent-local; `RunGrantOverlay` for system-skill grants) | 0046 | Implemented (residual: `RunGrantOverlay.Clear` on session-end is unwired, see Known Gaps) |
| Operator transport plane (UI gRPC surface: feed, cursor resume, projection, auth, audit, HITL, capability handshake) | 0047 | Implemented (all 13 slices live in `internal/substrate/operator/`; producer-side emit/dispatch wiring in non-operator organs is the residual) |
| Experiential memory: typed records, online graph, scenes-as-world-model, precedent lanes | 0049 | Implemented (issues 001-014 + A1.1/A1.2 entity drift signal; residual: API endpoint-set is last-observed, LLM contradiction-edges deferred) |
| Grounded planner: pre-plan discovery Scout (privileged `run_think` agent over `ToolExecutor.RestrictedTools`) | 0051 | Implemented (issues 001-006 closed 2026-06-23; the Go `awareness.Scout` organ detour was reversed; agent-side path is the authority) |
| KG²RAG retrieval: chunks + per-chunk triplets + tiered extraction (frozen `kg_extractors/` pipeline, Tier-1 metadata + Tier-2 spaCy patterns, LLM demoted to opt-in Tier-3) | 0053 | Implemented (D2 revised + wired 2026-06-25: vector seed + one-hop `kgExpand` shipped; `kg_extractor_agent` is a privileged system organ, behind `execution.kg_extractor_enabled`) |
| Multi-signal ranking: stage-A blend weights (cosine / lexical / coherence / confidence / pagerank / recency / activation) | 0054 | Implemented (LoCoMo recall@10 0.673/0.662 → 0.763/0.766; `SetRuntimeConfig` hot-swap; reranker deferred) |
| Multi-strategy chunking pipeline (pluggable `domain.Chunker` port; 5 chunkers `option_c` / `recursive_character` / `ast_go` / `markdown_header` / `late`; data-driven registry with `SourceType → ext → default` precedence; `chunk_relations` 512B budget; `source_document` entity via `DocTypeMnemonicEntity`, GC-exempt; `ChunkDocument` back-compat shim) | 0060 | Implemented (the `option_c` arm is the back-compat floor; `late` is gated on `chunker.late.enabled` + `embedder.supports_long_context` + `chunker.late.max_doc_tokens=8192`; over-budget docs fall back to `option_c` with a `late_fallback` log) |
| Semantic tool retrieval (embed-rank, `find_tools`, hybrid push+pull) | 0044 | Implemented (residual: HITL benchmark + floor/k tuning deferred) |
| Global Workspace context (ContentStore push/pull, `ContextRef` cid offload) | 0022 | Accepted (design) (the prime/pull surface is wired; full-derivation rollout follows ADR-0048 follow-ups) |
| Kernel-derived write classification (DefaultWriteTags, agent may only narrow) | 0035 | Accepted (design) |
| Trait-aligned cognitive SDK v2 (CognitiveAgent, DeterministicAgent, DaemonAgent) | 0036 | Accepted (design) |
| Central-Executive Planner / EFE selection (gated on A/B spike) | 0037 | Accepted (design) (Phase-1 modules in `internal/centralexec/`; `CapabilityBelief` loop unwired by design) |
| GAIA benchmark instrumentation (PRD lives, instrumentation surfaces pending) | 0050 | Accepted (design) |
| Open-core boundary (separate premium module, BSL 1.1, OSS = source of truth) | 0057 | Accepted (design) |
| Kernel-owned tool registry (System Tool reference monitor: grant → resource → data scope → approval → budget) | 0039 | Proposed (gated on the falsification spike; legacy `internal/tool/` will land here) |
| Hermes capability migration (one-shot consumer migration to the kernel-owned registry) | 0040 | Proposed (depends on ADR-0039) |
| Memory answer agent (synthesis LLM call over KG²RAG results; another privileged system organ) | 0055 | Proposed (not yet built; the third organ after the Scout and kg_extractor) |
| Community detection (Layer 3 of the KG²RAG roadmap) | 0056 | Proposed (ADR-0053 Layer 3; design space explored, not yet proposed) |
| Deep Kernel migration (Wasm sandbox + WASI-HTTP interception) | 0004 | Cancelled (2026-05-06; agents run as OS processes over UDS; the Wasm walls are gone) |

---

## Core Philosophy

Cambrian is a **Deep Kernel Substrate**: the Go kernel treats agents as
untrusted cells in a managed sandbox, owns the composition root, and refuses
to let routing, scoring, or selection live in Go conditionals. Three rules
thread through every organ:

- **Strict hexagonal separation.** `domain/` is the importable lingua franca
  (zero external dependencies, every security-critical decision lives here).
  Adapters (Postgres, gRPC, BBolt, LLM clients, MCP) sit at the edges and
  contain no business logic. The boundary is compile-time-enforced; a breach
  is a build failure, not a runtime check.
- **The Auction model.** Work is not hard-routed to agents. The Planner
  emits a natural-language `query` plus `depends_on` indices; the Gatekeeper
  filters to 3-5 candidates by ANN semantic match; the Auctioneer collects
  bids; the winner is selected on `GatekeeperScore`. The Zero-Hardcode Rule
  forbids any Go `if/else`/`switch` on agent identity, with three deliberate
  exceptions: system-shell commands, the reflexive path (latency), and
  security gates (scope, approval, budget). Those exceptions are the only
  type-aware code in the routing path.
- **The open-core boundary (ADR-0057).** Premium code (reactive rule engine,
  langfuse tracing) lives in a separate Go module and plugs in via
  `app.Options`. The OSS module never imports premium code; the CI script
  `scripts/check-no-premium.sh` enforces it, with the *policy* owned by
  `cambrian-premium/` per ADR-0057 D13. The kernel's value is the OSS
  composition root, the gRPC/OperatorConsole surface, and the
  memory/scoring/auction machinery, all public under BSL 1.1.

The biological model is incidental to the rules: long-term memory (pgvector
Engram engine with `activation_strength`), cellular metabolism (agent
process lifecycle + resource quotas), prefrontal awareness (LLM Planner with
workspace priming), and nervous regulation (Watcher proactive signals +
PauseController HITL). The mapping is in `docs/ARCHITECTURE.md` §Design
principles; the rules above are the load-bearing ones.

---

## Module Breakdown

The kernel is **strictly layered**. A breach (a `domain/` file importing
`internal/infrastructure/`, an adapter importing `internal/awareness/`) is a
build failure caught by `scripts/check-no-premium.sh` and the
`make separability` target. Every layer below `domain/` is replaceable from
inside `domain/`'s ports.

| Path | Role |
|---|---|
| `domain/` | Pure domain types and ports (importable, public). All security-critical decisions (Gatekeeper, TrustScore, scope, classification) live here, with zero external imports. |
| `app/` | Composition root. `app.Run(ctx, opts)` wires every subsystem; `app.Options` is the open-core extension seam (premium injects via hooks). |
| `cmd/orchestrator/` | Thin `main` shell over `app.Run`. |
| `internal/kernel/` | Subsystem assembly: four stacks (`MemoryStack` / `AwarenessStack` / `MetabolismStack` / `SupervisionStack`) and `ProvideServer`. The only cross-subsystem wirer. |
| `internal/awareness/` | Planner / Cortex. Produces `ExecutionPlan`; owns the Zero-Hardcode rule at the LLM layer; `ReplanHandler`, `ConsolidatorAgent`, `XMLParser`. |
| `internal/metabolism/` | Auctioneer, AgentManager, Gatekeeper, verifier pool, interview worker, DAG executor, A2A connector. The bidding and selection machinery. |
| `internal/memory/` | LTM: pgvector store adapter, hippocampus (procedural templates), KG²RAG retrieval (`kgExpand`), scene/edge writers, `WorkspaceStage`, `SpreadingEngine`, `ProfileStore`, `ArtifactVault` (CAS), `MemoryManager` / `MemoryAgent` / `MemoryWorker`. **Chunking pipeline (ADR-0060):** `chunker_registry` (data-driven `SourceType → ext → default` routing, Zero-Hardcode clean), `chunk_relations` (per-chunk 512B parent+linear+sibling metadata), `chunkers/` subpackage (the 5 implementations: `option_c` / `recursive_character` / `ast_go` / `markdown_header` / `late`), `OptionCChunker` (the 115-line back-compat floor), `ChunkDocument` shim over `OptionCChunker`. **Structure-aware pipeline (ADR-0060 default):** `structure_graph.go` (leaves-as-chunks + `BuildStructureGraph`), `structure_retrieval.go` (`applySectionConstraint`), `neighbor_window.go` (off by default), `anchor_query.go` (document-local anchor constraint), plus the `docling_agent` parse RPC and `postgres/structure_store.go` writer. |
| `internal/supervision/` | Gatekeeper, verifier pool, `ProfileAggregator`, `SynapticWatcher`, `Watcher` (signal-to-inspiration), `MemoryLifecycleManager` (event-driven session lifecycle, replaces `CircadianRhythm`). |
| `internal/substrate/` | gRPC server (`network`), session, OperatorConsole plane (`operator/`), synaptic event log, knowledge-graph extractor dispatch. |
| `internal/scope/` | Access control (ADR-0034/0035). `ScopedVectorStore` (fail-closed read chokepoint), `ScopedStoreWriter`, `ScopeResolver` (Postgres `agent_scopes` + LISTEN/NOTIFY), controlled vocabulary. Compiled into every build; security primitive, not paywalled. |
| `internal/infrastructure/` | Adapters: `postgres/` (pgx + pgvector), `llm/` (LLM client + `LLMProvider` broker), `mcp/` (MCP connector), BBolt storage DTOs. |
| `internal/tool/` | System-tool lifecycle (Python modules, `ToolRegistry`, confined `ProcessHandler`, jail sweep into CAS). Will be superseded by ADR-0039's kernel-owned registry. |
| `internal/skill/` | System-skill discovery and registry; agent skills live in the Python SDK (`agent.local_skills`). |
| `internal/storage/` | BBolt adapter (DTOs only, zero `domain/` imports). `AgentRepoDecorator` wraps storage as the domain interface; `BootstrapStorage` is the only constructor. |
| `internal/mapper/` | Proto-to-domain translation; the only bridge between `pb` and `domain` types. |
| `internal/telemetry/` | The only package importing OTel. `Bridge` translates `TelemetryObserver` calls to Prometheus + OTLP. Runtime-config-gated, no build tags. |
| `internal/config/` | Koanf 11-layer loader (ADR-0024); merges built-in defaults, `tuning.json`, `tuning.local.json`, `config.json`, `config.local.json`, `embedder.json`, `embedder.local.json`, `providers.json`, `providers.local.json`, `mcp.json`, `CAMBRIAN_*` env. |
| `internal/centralexec/` | Phase-1 modules for the Central-Executive Planner (ADR-0037); gated on the A/B spike against the auction. |
| `internal/router/` | Universal input router (ADR-0031); classifies inbound traffic into reflex / cognitive / signal lanes. |
| `internal/reactive/` | Reactive rule engine plug surface (OSS side; premium `cambrian-premium` provides the implementation via `app.ReactiveServices`). |
| `internal/service/` | Cross-cutting services (PII masker, audit ledger, scope resolution glue). |
| `internal/benchmarks/`, `internal/testing/` | Internal benchmark and test fixtures (kernel-side; the public harness is `cambrian-benchmarks`). |
| `api/proto/` | The gRPC/protobuf contract: `cambrian.proto` (legacy Orchestrator), `operator.proto` (OperatorConsole, the held-stable UI surface per ADR-0047). Generated Go stubs sit alongside. |
| `agents/` | Production Python agents auto-discovered by BBolt (`*agent.py` + `AGENT_DESCRIPTION`/`AGENT_MANIFEST`): `code_generator`, `code_executor`, `terminal`, `summariser`, `analyst`, plus the system organs `scout_agent` (ADR-0051), `kg_extractor_agent` (ADR-0053 D2 revised), `docling_agent` (ADR-0060 structure-aware ingestion; lazy Docling backend), and `reranker_agent` (ADR-0054, built + deferred, lazy model load). |
| `pkg/` | Reusable internal packages with stable enough semantics to import across the `internal/` wall (currently `util/`). |
| `scripts/` | Build, test, CI helpers: `check-no-premium.sh` (premium-leak audit), `check-separability.ps1` (hexagonal boundary), `run-tests.ps1` / `run-all-tests.ps1`, `setup-python-runtime.{ps1,sh}`, `chaos-compose.yml`, `toxiproxy-config.json`. |
| `configs/` | Committed starter config: `tuning.json` (curated power-user starter, 13 fields), `*.example.json` templates for config/embedder/providers/mcp. The real `*.json` files are gitignored (live secrets). |
| `db/migrations/` | Postgres migrations 002-011 (`add_document_type`, `activation_strength`, `hnsw_cosine`, `ebbinghaus_decay`, `document_edges_update`, `add_summary_column`, `chunk_triplets`, `chunk_triplets_confidence`, `chunk_pagerank`, `fts_index`). |
| `data/` | Runtime data: `content_store_blobs/` (CAS for `ArtifactVault` offload), plus the `vault/` tree (content-addressed agent products). |

---

## Memory & context model

Memory is an **Engram engine**, not a flat vector store. The kernel reads
typed documents by `DocType*` (Fact / Action / Scene / Entity / Episodic /
AgentProfile / ProceduralTemplate / NegativeEdge / Tool / Skill), writes
through a two-tier pipeline, and primes the agent with a bounded,
relevance-ranked bundle. Eight decisions are load-bearing:

- **SCENE + FACT pairing (ADR-0015).** A completed step writes up to two
  coupled documents: a `DocTypeMnemonicFact` (the structured result) and a
  `DocTypeMnemonicScene` (a snapshot of `masterContext` at step completion).
  A FACT is knowledge; a SCENE is the conditions under which that knowledge
  is true. `WorkspaceStage` fetches both. The pair decoupled scene creation
  from the Tier-2 FULL threshold (ADR-0025 D4), which in practice was never
  reached and left the SCENE corpus empty.
- **Two-tier write pipeline (ADR-0015).** A new memory lands first in the
  in-memory Tier-1 channel (immediately queryable within the same run). A
  background Tier-2 LLM-as-Judge scores it and commits to LTM as FULL
  (FACT+SCENE), FACT_ONLY, or DROP, with a heuristic fallback on timeout.
  `activation_strength` ∈ [0,1] is the lifecycle metric; effective relevance
  at read = `cosine × (α + (1−α)·activation) × e^(−λ·age)` (floor-multiplier
  + temporal decay), with a 5% exploration slot (Matthew-effect guard).
- **Graph layer (ADR-0017, expanded ADR-0025).** `document_edges` carries
  `closes` / `specifies` / `contradicts` / `discussed_in` edges. The
  `SpreadingEngine` does GraphRAG BFS, attenuating `0.75^depth`. Live
  execution writes `specifies` (scene_N → scene_{N-1}) and `discussed_in`
  (committed fact → producing scene) deterministically; only `closes` and
  `contradicts` need LLM semantic reasoning and remain the Consolidator's job.
- **Global Workspace, push/pull (ADR-0022 + ADR-0048).** The kernel pushes
  context, the agent pulls more. `WorkspaceStage.PrimeForStep` assembles a
  bounded, relevance-ranked bundle (SCENE+FACT + episodic + spreading
  neighbours), past a contradiction guard, offloads heavy payloads to the
  content-addressed `ContentStore` (CIDs), and populates
  `Handoff.WorkingMemory`. The agent reasons over that bundle and pulls more
  on demand via `memory_query` (the same pattern `find_tools` and
  `find_skills` reuse). This replaced the old O(N²) context growth with a
  push + on-demand pull model.
- **Context hygiene (ADR-0048).** Recall no longer re-injects the run's own
  step output: `QueryService.Search` drops same-session `step_N` records (D1)
  and applies a cosine `RecallSimilarityFloor` (an all-irrelevant query
  returns empty, the agent answers from its own knowledge, knowingly). Every
  promoted memory carries a one-line `summary` (Tier-2 descriptor,
  `documents.summary` column, migration 007); recall serves the summary as
  the agent-facing surface with the full body behind
  `metadata["content_cid"]`. A `{"$cid":"…"}` tool arg is hydrated
  kernel-side so existing content is referenced, not re-emitted. The agent
  loop renders executed calls as condensed `<step>` cards inside a numbered
  `<Trajectory step="N">`.
- **Experiential memory (ADR-0049) + KG²RAG (ADR-0053).** Memory is typed by
  intent: a side-effecting tool call is a `mnemonic_action` (recorded
  directly, bypassing Tier-2 keep/drop); a read is a `mnemonic_fact`. Each
  plan writes one immutable scene at completion (two-faced: a reconstruction
  with engaged refs + baseline CIDs, and an abstracted projection for
  situational match). Touched things become first-class `mnemonic_entity`
  records keyed by canonical `kind:id` (mutated-only minting, field-LWW
  rebuildable cache, action-driven supersession). The world model carries a
  **precedent lane** (push via `PrimeForPlanning` `<PrecedentLTM>`, pull via
  `recall_precedents`), failure-weighted and similarity-gated; the LLM only
  reasons over precedents. KG²RAG complements this: chunks become
  `documents`, triplets are written by a frozen `kg_extractors/` tiered
  pipeline (Tier-1 metadata + Tier-2 spaCy patterns, no LLM; confidence and
  `sources[]` columns per migration 009). The `kg_extractor_agent` is a
  privileged system organ, exactly like the Scout, so it bypasses
  auction/Gatekeeper/interview by construction.

- **Multi-strategy chunking pipeline (ADR-0060).** External knowledge ingest
  is no longer a single Go function. A pluggable `domain.Chunker` port
  (`cambrian-core/domain/chunker.go`) declares
  `Name() / Supports() / Chunk()`; the `Chunk` value carries `Body` + a
  free-form `Metadata` map (the `chunk_relations` subkey is reserved
  for the IngestionManager and is set after `Chunk()` returns). Five
  implementations live in `internal/memory/chunkers/`: `OptionCChunker`
  (the 115-line paragraph-or-sentence back-compat split; the floor;
  the `ChunkDocument` shim in `internal/memory/chunker.go` delegates
  to it), `RecursiveCharacterChunker` (LangChain-style recursive
  separator with `["\n\n", "\n", " ", ""]`, default `chunk_size=200`
  per Chroma 2024), `ASTGoChunker` (pure-Go `go/ast` decl-level
  splitter for `.go` files; no cgo, no tree-sitter dependency),
  `MarkdownHeaderChunker` (heading-based split; retains
  `section_path` in metadata), and `LateChunker` (Günther et al.
  long-context encoder + per-chunk mean-pool over masked token
  embeddings, gated — see below). The `chunker_registry`
  (`internal/memory/chunker_registry.go`) routes `(sourceType, ext)`
  in strict precedence
  `match(SourceType) → match(ext) → default("option_c")`; the default
  name is a config value, not a Go constant — `Resolve` is a pure map
  lookup with no `switch sourceType` / `switch ext` (the
  **Zero-Hardcode Rule**). Each ingested document is materialised as
  one `DocTypeMnemonicEntity` row (ADR-0049 D8) with discriminator
  `kind: "source_document"`, carrying `SourceURI`, `SourceType`,
  `Title`, `Author`, `Timestamp`, and the offloaded `ContentCID`
  (the byte-oriented `domain.ContentStore.Put` offloads the full body
  once per document, not once per chunk). Source-document entities
  are **GC-exempt** (ADR-0060 D8) because they are the drill-down
  targets for chunk recall, not the chunk-level recall targets
  themselves — the `parent_entity_id` in `chunk_relations` resolves
  to them. `IngestMemory` forwards the authenticated author, session,
  tags, and importance into this path; `ProcessSync` returns only after
  the chunk bodies have been embedded as a batch, then saved as durable
  `DocTypeMnemonicFact` rows in one store batch, using deterministic
  `{document_id}-chunk-N` IDs and raw chunk text. Each chunk's
  `Metadata["chunk_relations"]` carries the
  JSON-marshaled
  `ChunkRelations { ParentEntityID, PrecedingChunkID, FollowingChunkID,
  SiblingContext { ParentTitle(80B), ParentSummary(120B),
  ParentScene(120B), PrecedingSnippet(96B), FollowingSnippet(96B) } }`
  with a strict 512B total budget (`SiblingContext.MarshalJSON`
  enforces it; over-budget fields are trimmed at the right, never
  mid-rune). The retriever follows
  `chunk_relations.parent_entity_id` → source-doc entity →
  `content_cid` → full body via `ContentStore.Get`, so drill-down
  from a recalled chunk to the source document is deterministic.
  `LateChunker` is **opt-in**, not the default: it is selected only
  when `chunker.late.enabled = true` AND
  `embedder.supports_long_context = true` (default `false` until the
  embedder-selection ADR resolves) AND the body is within
  `chunker.late.max_doc_tokens` (default `8192`, matching
  `nomic-embed-text`); over-budget docs fall back to
  `OptionCChunker` with a `late_fallback` log + run-manifest metric.
  `domain.Embedder` gains an additive
  `EmbedBatch(ctx, []string) ([][]float32, error)` so the late
  chunker drives the vectorized Ollama `/api/embed`
  `input: texts` endpoint in one request rather than N (the batch embedder
  was fixed 2026-07-06 to use `/api/embed`; the legacy single-vector
  `/api/embeddings` ignored the `input` array and silently fell back to a
  slow per-chunk loop).

- **Structure-aware ingestion pipeline (ADR-0060 deferred items, now the
  default).** This implements three items ADR-0060 v1 explicitly deferred —
  *PDF structure-aware parsing*, *hierarchical chunk relations (`section_id`)*,
  and *chunk-level graph edges in `document_edges`* — as the **default**
  chunking path (`execution.structure_graph_enabled`, default **true** in
  `internal/config/config.go` + `configs/tuning.json`; opt-out). On ingest the
  `IngestionManager` RPCs the `docling_agent` sidecar
  (`agents/system/docling_agent/`) to recover the document's real hierarchy:
  a dependency-free Markdown/text parser for born-digital text, and a guarded
  **Docling** backend (RT-DETR layout + TableFormer) for PDF bytes
  (`docling_backend.py`; OCR off by default, `DOCLING_OCR=1` forces it for
  scanned pages). The parse returns a normalized `StructuredDocument`
  (`structure.py`) of `StructNode`s, with an **OCR-junk filter**
  (`is_junk_leaf`) that drops image placeholders, config-echo, and low-alpha
  fragments at parse time. On the Go side (`internal/memory/structure_graph.go`)
  the parser's **leaves *are* the chunk set** (`ChunksFromLeaves`, with
  size-controlled merging of consecutive same-section leaves toward ~500 chars),
  so `section_path` stamps are correct by construction. Sections are persisted
  as `documents` rows with `document_type='doc_section'` (no embedding, so the
  `document_edges` FK holds); every leaf chunk is stamped with `section_path` /
  `section_ltree` (Postgres `ltree`, GiST-indexed for `<@` subtree queries) /
  `parent_section_id`; typed structural edges (`part_of`, `next`) are written
  to `document_edges` (`internal/infrastructure/postgres/structure_store.go`,
  wired through `DoclingDispatcher`). Retrieval adds a **section-scoped
  promotion** stage (`internal/memory/structure_retrieval.go`):
  `applySectionConstraint` in `searchByType` promotes chunks in the
  `section_ltree` subtree of a section the query names (`extractSectionTerms`
  prefers hierarchical numbers like `3.2` over topic words). When a document has
  no heading hierarchy (flat plain text, or a broken PDF text layer) the section
  graph is empty and the pipeline falls back cleanly to the flat chunker. Two
  adjacent levers ship built but **off by default**:
  `execution.neighbor_window_enabled` (`internal/memory/neighbor_window.go`,
  append-after context expansion over `chunk_relations` neighbors) and the
  Stage-B `reranker_enabled` (see Known gaps). Ingest perf was fixed alongside:
  per-item scene generation on the ingest path is gated by
  `execution.scene_gen_on_ingest_enabled` (default **false**) — with the batch
  embedder fix this cut a 2-PDF / 236-chunk ingest from ~37 s (RPC-deadline
  timeouts) to ~6.7 s.

---

## Security & isolation

The **Deep Kernel security model is documented but not fully implemented.**
The Wasm sandbox (ADR-0004) was cancelled 2026-05-06; agents run as standard
OS processes communicating over UDS, not as Wasm cells. The WASI-HTTP
interception layer was never built. What holds today is the **hexagonal
architecture + DAG immutability** enforced at compile time. Four invariants
are non-negotiable:

1. **Hexagonal separation.** `domain/` has zero external imports. All
   security logic (Gatekeeper, TrustScore, scope, classification) is
   unit-testable without infrastructure. Adapters import `domain/`; the
   reverse is forbidden, and the build catches it.
2. **Fail-closed reads.** `ScopedVectorStore` refuses unscoped Search. The
   pgvector adapter mirrors `Allows` as a jsonb predicate over
   `metadata.tags`. An agent only sees documents its `EffectiveScope`
   (caller_scope ∩ agent_scope) permits.
3. **DAG immutability.** Once `DAGExecutor.Execute` begins, the
   `ExecutionPlan` is frozen (value-copy semantics). A compromised agent
   cannot redirect future steps; the executed plan equals the approved plan.
4. **Kernel composition-root monopoly.** `internal/kernel/` is the only
   cross-subsystem wirer. DI changes are bounded to one package; nothing else
   constructs subsystems.

`ToolExecutor` is the single reference monitor for tool calls (grant →
resource policy → data scope → approval → budget → dispatch → audit). The
`discovery-safe` grant (ADR-0051) is the ceiling that overrides unrestricted
access for the Scout and similar read-only discovery agents. MCP tools
(`mcp:<server>/<tool>`) are operator-trusted but their responses are
untrusted: every response is injection-scanned, remote args are an audited
egress.

Access scoping lives in `internal/scope/` (ADR-0034/0035). `ScopeConfig`
carries three tag sets (`RequiredTags` / `AnyOfTags` / `ForbiddenTags`),
CNF-evaluated by `EffectiveScope.Allows`. Write classification is
**kernel-derived** from operator-set `DefaultWriteTags`; an agent may only
narrow, never broaden (mis-classify-to-leak is closed). Phase 1 (agent-scope
read/write enforcement) is shipped; Phase 2 (`caller_scope` live wiring,
persist at `StartConversation`, re-derive per-RPC) is HITL-gated pending
review. Do not advertise caller-scope protection until that wiring lands.

---

## Data-driven development

Cambrian's product is developed through a data-driven development (DDD) loop anchored in [the benchmark harness](../cambrian-benchmarks/context.md). The harness is the black-box test suite that measures the product's behavior.

**Rule: every feature change MUST be measured.**

When you add, remove, or change a feature in this sub-repo:

1. **Identify the affected part** (memory, retrieval, tool use, planning, agent selection, LLM tracing, etc.).
2. **Map to benchmarks**: find the existing suite(s) in [cambrian-benchmarks/](../cambrian-benchmarks/) that exercise the affected part. The current suite catalog: `locomo` (memory recall/answer/full), `operator_sweep` (runtime config arms + LoCoMo), `gaia-tool-use` (single-turn tool use), `stress` (concurrency + long-plan), `hello` (protocol smoke).
3. **Baseline first**: run the relevant benchmark(s) BEFORE the change to establish a baseline.
4. **Make the change.**
5. **Measure after**: run the same benchmark(s) AFTER the change.
6. **If no existing benchmark covers the affected part**, you MUST:
   a. Run the closest existing benchmark as a proxy.
   b. **Propose a new benchmark** to add to [cambrian-benchmarks/](../cambrian-benchmarks/) that measures the affected part specifically. The proposal lives in the harness's `docs/` folder as `<feature>-benchmark.md` and includes: a one-paragraph rationale, a one-paragraph task spec, a one-paragraph scoring spec, and a draft `failure_kind` taxonomy. The new suite is implemented in `src/cambrian_bench/suites/<name>/` once approved.
   c. Document the proposed benchmark in the bench CONTEXT.md's "Suites" section.
7. **Update this CONTEXT.md**: Implementation Status table (add/remove/change the row), Module Breakdown (if a new module), Known Gaps (if a deferred item changes), Glossary (if a new term), Core Philosophy (if the principle changes).
8. **Atomic commit**: the feature change AND any benchmark change go in one commit; the Implementation Status update and the code change are inseparable.

**Why this matters:** the product's quality is its benchmark score. Code changes without measurement are not progress; they are hope. The harness is the contract between "I changed the code" and "the product got better."

---

## Known Gaps

A non-exhaustive list of the load-bearing items that are still partial,
unwired, or guarded. Each entry cites the file or ADR where the work lives.

| Item | Detail |
|---|---|
| `DialAgent` context timeout | `grpc.WithBlock()` with no deadline in `AgentConnector`; the `bootAgent` UDS poll is safe, but a long-lived hang on a stuck agent process has no upper bound. |
| Agent liveness | `grpc.health.v1` is wired; a proactive liveness probe (reap zombie processes) is not. A crashed agent with a still-open stream will not be detected until the next request fails. |
| Consolidation atomicity race | `consolidateCluster` does non-atomic `Ingest` + `DeleteBatch`. A crash between the two leaves duplicates; a crash before leaves the old memory plus a new one. Lives in `internal/memory/`. |
| Anomaly detection | Stub in `memory_agent.go`; the hooks are present, the logic is partial. Memory pressure events are emitted but not yet acted on. |
| `RunGrantOverlay.Clear` on session-end | Unwired. The session-keyed overlay is single-use, so there is no cross-run leak, but there is no clean session-completion hook either. Per-run grants accumulate as ephemeral state until the session ends. (ADR-0046) |
| Unary `Execute` HITL | The `PauseController` is only active on the streaming path. A unary `Execute` call cannot pause for human approval. |
| Tool sandbox depth | OS process + caps only (the Wasm layer was cancelled). rlimit / cgroup / seccomp is a Unix follow-up. |
| ADR-0037 `CapabilityBelief` loop | Unwired by design. The EFE-vs-auction A/B spike has not run; Phase-1 modules live in `internal/centralexec/` but the active-inference resource binding is not connected. |
| `caller_scope` Phase-2 wiring (ADR-0034) | Mechanism is implemented; live wiring (persist at `StartConversation`, re-derive per-RPC, `ScopeSystem` for system reads, artifact RPCs, promotion event-bus) is HITL-gated. |
| `embedder` 768→1024 dim migration | Resolved for recall (`bge-large`@1024 + query prefix, LoCoMo recall@100 0.47→0.94). Residual: the ADR-0044 tool menu is not re-embedded; `tool_retrieval_floor>0` would let the tool path benefit too. Stays sub-1B so it co-resides with `qwen3:8b` on 12 GB. |
| Stage-A blend weights (ADR-0054) | Tuned and adopted in `config.json` (cosine 0.40, lexical 0.20, coherence 0.05, confidence 0.10, pagerank/recency/activation 0.0). Coordinate-descent sweep over the operator plane (`SetRuntimeConfig` hot-swap), conv-split train/test: baseline @10 0.673/0.662 → 0.763/0.766. Takes effect on restart. |
| Stage-B reranker (ADR-0054) | Built and DEFERRED (`reranker_enabled=false`). bge cross-encoder as a warm system organ (`reranker_agent`, mirrors `kg_extractor`) regressed recall on LoCoMo (recall@1 0.168→0.089): the Stage-A blend already orders the top-50 well. Ollama can't serve cross-encoders; GPU revisit = CUDA torch + `bge-reranker-v2-m3`. Re-tested 2026-07-06 on a 2-PDF corpus: still net-negative (specific-fact top-5 0.50→0.30, the cross-encoder disagreeing with the correct chunk) and ~8 s/query on CPU. Two latent bugs were fixed while there and kept: `sentence-transformers` was uninstalled (agent crashed → silent fail-soft, never ran), and the model loaded in `__init__` blew the 10 s agent-boot socket timeout (`instance_manager.go`) → re-spawn per query; now **lazy-loaded** (`_ensure_model`), so it boots instantly and stays warm. Enable only with a GPU. |
| pgvector dim-migration is destructive | An embedding-dimension change recreates `documents` (drop+recreate; pgvector can't `ALTER` a VECTOR dim). The boot path now refuses to start if memory docs are present unless `ALLOW_DESTRUCTIVE_DIM_MIGRATION=1` is set. The guard only protects inside the running binary; rebuild before launch. |
| Model-failover requires ≥2 generators | Automatic model failover via `selectModelCandidates` only fills `Fallbacks` when `FindModelCandidates` returns ≥2. A single-generator config has one candidate and no rung to fall back to. |
| MCP tool OAuth / per-tenant budget | `mcp` tool price in the menu and per-tenant OAuth/budget are deferred. The `mcp:<server>/<tool>` shape is wired and injection-scanned. |
| Re-chunking backfill (ADR-0060) | When the operator changes the `chunker.*` config, existing documents are not re-chunked in v1. A backfill path is future work; operators re-ingest the affected sources to apply a new strategy. |
| Source-document entity size (ADR-0060) | Source-doc entities are GC-exempt (ADR-0060 D8) and accumulate; operators must size the `ContentStore` (CAS) and the entity table accordingly. A dedicated size-cap / tiered-storage ADR is a follow-up. |
| Late-chunker gate is implicit (ADR-0060) | Setting only one of `chunker.late.enabled` / `embedder.supports_long_context` silently routes to `OptionCChunker` (logged warn at resolve time, not a hard error). Intent is "fail to the known-good default," but it means the gate is *implicit* for any operator who only sets one of the two flags. |
| Future chunkers (ADR-0060) | **Implemented 2026-07-06 (default on) — see *Structure-aware ingestion pipeline* above:** PDF structure-aware parsing (via **Docling**, not Marker/LayoutLMv3), hierarchical chunk relations (`section_path` / `section_ltree` / `parent_section_id`), and chunk-level graph edges in `document_edges` (`part_of` / `next`). Still deferred: tree-sitter multi-language AST chunker (cgo-blocked; the v1 AST path is `go/ast`-only), `sibling_index` chunk relations, LLM-driven chunkers (Contextual Retrieval / Propositions / Small-to-big — explicitly rejected: no per-chunk LLM call), auto-router (LLM picks chunker per document), and re-chunking backfill when `chunker.*` config changes. |
| `go.mod` module rename | `cambrian-core/go.mod` still declares `module github.com/cambrian-sh/cambrian-runtime` even though the directory is `cambrian-core/`. The rename cascades through every internal Go import, the premium `go.mod` `require` block, and `go.work`. Follow-up ticket, not part of this docs plan. |

---

## Terminology Glossary

The kernel's domain language. Bold terms are the canonical names used in
`domain/` and the ADRs. Each entry is one or two sentences.

**Core orchestration**

- **Substrate**, the Go kernel/runtime; the OSS composition root that owns agent lifecycle, the auction, and the gRPC surface.
- **Handoff**, an inter-agent message envelope (`domain.Handoff{ID, FromAgent, ToAgent, Payload, Confidence, Uncertainties, Context}`); the proto `pb.Handoff` only exists at the gRPC boundary.
- **Payload**, a Handoff's data body (`domain.Payload{ID, Type, Data, Metadata}`).
- **Auction / Proposal / Auctioneer**, the bidding round, an agent's bid (Confidence + Rationale + Requirements + Latency), and the unit that collects bids and picks the winner.
- **Planner**, the LLM component in `internal/awareness/` that decomposes a request into an `ExecutionPlan`; the Zero-Hardcode rule lives here.
- **DAGExecutor**, traverses the plan as a DAG with concurrent independent steps; tiered recovery (SelfHealer → fallback → replan → `PartialPlanError`); value-copy plan freeze.
- **Zero-Hardcode Rule**, agent-to-task routing lives in the Awareness (LLM) layer, never as Go `if/else`/`switch`. Three exceptions: system-shell, reflexive path (latency), and security gates (scope/approval/budget).

**Gatekeeper, merit, and trust**

- **Gatekeeper**, the 3-layer filter (Declaration → Interview → Merit) that narrows the candidate pool to 3-5 entries by ANN semantic match + cognitive fingerprint + weighted performance.
- **GatekeeperScore**, the winner-selection score, `w1·SuccessRate + w2·TrustScore + w3·(1/NormLatency) [− w4·NormalizedCost]` (ADR-0011 adds the cost term).
- **TrustScore**, the agent's reliability, EWMA-over-task-verifier-score ratio. Defense against **Confidence Inflation** (an agent over-bidding with low real success).
- **Verifier Pool / Verification**, high-Merit agents that independently score a ~10% sample (FNV-1a hash on plan id) of completions; Surveillance mode flags serial failures for re-verification.
- **ProfileAggregator**, a background worker that recomputes Merit EWMA metrics from `TaskEvent`s.

**Memory and context**

- **VectorStore**, the `domain.VectorStore` port over pgvector (HNSW cosine, query-time `TemporalDecay`, three-set/CNF scope predicate over `metadata.tags`).
- **Document**, a single LTM row typed by `DocType*`: `MnemonicFact` (knowledge), `MnemonicAction` (events), `MnemonicScene` (conditions), `MnemonicEntity` (world-model things), `EpisodicMemory` (session narrative), `AgentProfile` (cognitive fingerprint), `ProceduralTemplate` (Hippocampus replay), `NegativeEdge` (failure memory), `Tool` (vectorised menu), `Skill` (vectorised menu).
- **activation_strength**, the `[0,1]` lifecycle metric per Document (0.1 default, +0.05/retrieval to 0.8); replaces `ImportanceScore`. Effective relevance at read = `cosine × (α + (1−α)·activation) × e^(−λ·age)`.
- **WorkspaceStage**, the pre-planning enrichment: SCENE+FACT fetch + pairwise contradiction guard (cosine>0.85 → `[CONFLICT]`) + cold-start fallback; the single LTM-to-Planner gate.
- **SpreadingEngine**, the GraphRAG BFS over `document_edges`, `0.75^depth` attenuation; the Graph Coverage Guard is the 3-band gate before BFS enrichment.
- **EpisodicMemory**, the session-level narrative index (`Goal`, `Decisions`, `ActionItems`, no `Outcome`) as `DocTypeEpisodicMemory`; produced by the ConsolidatorAgent on `SessionCompletedEvent` (ADR-0029).
- **LTMEnrichment**, the typed `PrimeForPlanning` result: `Facts` + `Negatives` + `Episodes`; carries `<PrecedentLTM>` from ADR-0049 and `<DiscoveryLTM>` from the Scout (ADR-0051).
- **Tier-1 / Tier-2**, the in-memory channel immediately queryable in-run, and the background LLM-as-Judge that commits FULL / FACT_ONLY / DROP with a heuristic fallback on timeout (ADR-0015).
- **failure_kind**, the typed result of a failed step (`retryable` / `non_retryable` / `verification_failed` / `partial` / `replan_needed`); consumed by the DAGExecutor's tiered recovery (ADR-0005/0010/0013).
- **Chunker**, the `domain.Chunker` hexagonal port (`Name() / Supports() / Chunk()`); the `Chunk` value carries `Body` and a free-form `Metadata` map. Five implementations live in `internal/memory/chunkers/` (`option_c` / `recursive_character` / `ast_go` / `markdown_header` / `late`); routing is data-driven via the `chunker_registry` (Zero-Hardcode Rule, ADR-0060).
- **chunker_registry**, the data-driven switchboard at `internal/memory/chunker_registry.go` that routes `(sourceType, ext)` to a registered `Chunker` in strict precedence `match(SourceType) → match(ext) → default`; the default name is a config value, not a Go constant. An unknown route or default is a startup error, not a silent fallback.
- **chunk_relations**, the per-chunk JSON-marshaled metadata payload (set by the IngestionManager in `Chunk.Metadata["chunk_relations"]`): a `ChunkRelations` value carrying `ParentEntityID`, `PrecedingChunkID`, `FollowingChunkID`, and a `SiblingContext`. The retriever follows `parent_entity_id` to drill down to the source document (ADR-0060).
- **SiblingContext**, the content half of `chunk_relations`: parent title, parent summary, parent scene, preceding snippet, following snippet. Per-field caps 80 / 120 / 120 / 96 / 96 bytes; strict 512B total budget enforced by `SiblingContext.MarshalJSON` (over-budget fields are trimmed at the right, never mid-rune). JSON keys and identifier fields are not in scope.
- **source_document**, the `DocTypeMnemonicEntity` discriminator used for ingested external documents (ADR-0060 D1). The entity row carries `SourceURI`, `SourceType`, `Title`, `Author`, `Timestamp`, and `ContentCID` (the CAS handle returned by `domain.ContentStore.Put`). **GC-exempt** (ADR-0060 D8) because source-doc entities are drill-down targets for chunk recall, not chunk-level recall targets themselves.
- **LateChunker**, the gated long-context encoder chunker (Günther et al., arXiv:2409.04701). Selected only when `chunker.late.enabled = true` AND `embedder.supports_long_context = true` (default `false` until the embedder-selection ADR resolves) AND the body is within `chunker.late.max_doc_tokens` (default `8192`, matching `nomic-embed-text`); over-budget docs fall back to `OptionCChunker` with a `late_fallback` log + run-manifest metric.

**Scope and access (ADR-0034/0035)**

- **Scope**, the three-set tag vocabulary (`RequiredTags` / `AnyOfTags` / `ForbiddenTags`); CNF-evaluated by `Allows`.
- **EffectiveScope**, the caller∩agent intersection; the per-RPC scope that gates reads (`ScopedVectorStore`) and writes (`ScopedStoreWriter`).
- **DefaultWriteTags**, the operator-set genotype; the kernel derives a write's classification from it. An agent may only narrow, never broaden.
- **ScopeSystem**, the explicit kernel-internal bypass sentinel; used for system reads that must cross scopes (e.g. the ConsolidatorAgent reading the producing session).

**Tools and skills (ADR-0039/0043/0044/0045/0046)**

- **ToolGrant**, the operator-set per-tool grant + resource bounds (filesystem roots, egress SSRF guard, command allowlist; empty = fail-closed).
- **ToolExecutor**, the single reference monitor: grant → resource policy → data scope → approval → budget → dispatch → audit. Anything that runs a tool goes through here.
- **Skill**, an authored procedural capability (`SKILL.md` with `{Name, Description, Instructions, ToolGrants, ScopeTags}`); distinct from a procedural template (authored vs learned).
- **SkillRegistry / SkillRetriever**, the system-skill index (`DocTypeSkill`) and the scope-gated retriever that serves `ListSkills`. Agent skills are SDK-local and bypass the registry.
- **RunGrantOverlay**, the session-keyed, ephemeral overlay that grants a system skill's bundled tools for the run. Operator authority; dangerous tools are still approval-gated; agent skills are narrow-only by construction (ADR-0046).
- **Two-tier tool disclosure**, the "short to choose, full to call" pattern: deterministic `toolSummary` (first sentence-or-line, capped) is embedded and served as Tier-1 by `ListTools`; `describe_tool(name)` fetches Tier-2 (full spec) grant-gated, fail-closed (ADR-0045).
- **`find_tools` / `find_skills`**, the agent-side pull that complements the kernel-side push of `ListTools` / `ListSkills`. Same hybrid pattern as `memory_query`.

**LLM and prompts (ADR-0042)**

- **LLMProvider**, the health-guarded model broker: id-keyed registry, circuit breaker (failure = transport err, HTTP≠200, timeout, empty response), price ledger, failover ladder. Organs stay blind to model id and health.
- **Generator**, the consumer-side LLM interface; the only type the organs see. Streaming and non-streaming variants are registered for every generator via `llm.NewStreamersFromGenerators` so any provider can serve cognitive agents and `StepAllocation` fallback can span providers.
- **PromptBuilder**, 8 pure functions for the canonical `<System>` / `<Context>` / `<Task>` / `<OutputSchema>` prompt sections; the `PromptRegistry` is the compile-time prompt catalog, threaded to `PlanEvent` via the 8-char `PlannerPromptVersion` hash.

**Operator transport (ADR-0047)**

- **OperatorConsole**, the kernel gRPC service the `cambrian-ui` console speaks to. The only UI→kernel API. Carries the sequenced feed+spool, cursor resume / RESYNC, projection + snapshot, auth + role gate, audit + idempotent commands, control hub / HITL, chat/inject, token lane, and capability handshake.
- **AgentManager** (reused), the internal lifecycle owner that the OperatorConsole projects, not a separate component.

---

## Cross-repo pointer

This file is a sub-repo manual. The monorepo-level map, the four cross-repo
invariants, and the open-core boundary live one level up.

- Monorepo context: [../../CONTEXT.md](../../CONTEXT.md)
- Monorepo agent guide: [../../AGENTS.md](../../AGENTS.md)
- Open-core boundary ADR: [../../0057-open-core-boundary.md](../../0057-open-core-boundary.md)
- Operator transport plane (the UI surface): [docs/adr/0047-operator-transport-plane.md](docs/adr/0047-operator-transport-plane.md)
- Multi-signal ranking (the bench-driven ranking ADR): [docs/adr/0054-multi-signal-ranking.md](docs/adr/0054-multi-signal-ranking.md)
- Chunking pipeline (pluggable Chunker port + 5 chunkers + `chunk_relations` + `source_document` entity): [docs/adr/0060-chunking-pipeline.md](docs/adr/0060-chunking-pipeline.md)
- Kernel agent guide (the rule book, complementary to this manual): [AGENTS.md](AGENTS.md)
- Kernel architecture orientation: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- ADR index and status vocabulary: [docs/adr/README.md](docs/adr/README.md)
