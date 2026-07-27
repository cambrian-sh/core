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
| Access-policy port + extraction: `domain.Authorizer`, `TagPredicate`, kernel-side enforcement points, decision moved to the premium `authz` plugin | 0085 | Implemented (OSS `domain/authz.go` + `internal/authz/`; `domain/scope.go` and `internal/scope/` GONE — `grep ScopeConfig cambrian-core` returns nothing. `execution.scope_enforcement_enabled` retired: the chokepoints run unconditionally and only the answer varies. OSS fails OPEN, the plugin fails CLOSED. Contract 0065→0066 + cap `access-policy`: `ExplainAccess`, `ListClassificationTags`, `AccessDecisionOp`, `QueryMemoryResponse.policy_note`; agent plane gained `MemoryResponse.policy_note`. Skill `ToolGrants` are now INTERSECTED with policy (`skill_grant_clipped`) — that was a live escalation path. Residual: policy persistence, SEC-03 for the surface clamp, no benchmark suite) |
| Plugin identity on the handshake: `SnapshotResponse.plugins` + per-plugin version skew | 0089 | Implemented (contract **0066→0067**). `PluginInfoOp` carries each DECLARED plugin's id, display name, **own version line**, state (`active`/`deps_unmet`/`not_entitled`/`expired`), capabilities, panels, reason, missing deps and entitlement expiry. `contract_version` describes the OSS proto surface only, so it cannot detect a plugin two majors ahead of the console's panels — every RPC would answer normally. Reported for plugins that did NOT register too: a console otherwise cannot tell "no such plugin" from "it declined to register". Kernel still interprets nothing (ADR-0082 D2); the `app.PluginStatus` → `operator.PluginInfo` mapping lives in the composition root. UI side: `PINNED_PLUGIN_VERSIONS`, a four-state verdict (aligned/minor/major/unknown), and a `PluginSurface` shell every plugin panel renders inside so the banner is structural rather than opt-in. Live-validated: 4 plugins on the wire at contract 0067) |
| Ingress and surface identity: `domain.IngressResolver` port, `Session.Surface` writer, Postgres ingress registry with namespaces | 0090 | **D0/D2/D3 implemented** (D4 identity linking and the outbound/duplex path are NOT). An *ingress* is any point where the outside world enters — Telegram, webhook, websocket, inbound API — and chat is one payload type through one; ADR-0080's Chat Manager is renamed the **Chat Ingress** (`cambrian-premium/chat/ingress.go`, lifecycle `chat-ingress`, env `CHAT_INGRESS_ADDR` with the legacy name still honoured so a rename cannot silently disable chat). **An ingress HAS a surface; it is not one, and it never asserts its own** (INV-5) — the mapping is registered out of band and the kernel attaches it. OSS holds only the port (nil ⇒ nothing is an ingress ⇒ every surface stays transport-derived, i.e. unchanged); premium owns `authz_ingress` (write-through, wholesale reload, LISTEN/NOTIFY on `authz_ingress_changed`). `IngressRegistration.Namespace` bounds which external ids an ingress may speak for — the Matrix application-service precedent, so one bridge cannot impersonate another's users. Only **agent** principals can be ingresses (a console operator must never inherit a clamp). **D7 (delivery address):** `domain.DeliveryAddress{IngressAgentID, ExternalID}` on `Conversation` (migration `0006`), bound by the kernel on first inbound and **write-once** — rebinding is a redirect, so the guard lives in the UPDATE WHERE clause rather than a read-then-write (12-way concurrent test: exactly one bind wins). An agent names a CONVERSATION, never a recipient (SMTP envelope split) — otherwise anything producing a message could choose who reads it and a fire-and-forget ingress would deliver it. Namespace enforced at bind time via `IngressRegistration.DeliveryFor`. An unbound conversation is undeliverable, which matches Telegram's own rule. **D8 (outbound delivery):** `internal/ingress` — `DeliveryService` (resolve -> RE-AUTHORISE against the current registration -> idempotency -> send) plus `DaemonTransport` over the existing `CallDaemon` seam. Re-authorising at delivery time is what makes revoking a compromised ingress stop conversations bound while it was trusted. Permanent-vs-transient is the ingress's call (only it knows if a 403 is "blocked" or "rate-limited"); unlabelled = transient, permanent = dead-lettered. `DeliveryJournal` nil-safe = best-effort. **D9 (Turn no longer caps replies at one):** `TurnService.Emit` is the speak primitive — append + deliver-if-bound; `Turn` routes its reply through it, so the console still gets exactly one reply while anything may Emit any number of times, including with no inbound turn (proactive). The message is durable BEFORE delivery, so a delivery failure never loses it and never fails the turn. **D10 (SDK):** `cambrian_agent_sdk.IngressAgent` serves the delivery endpoint AND holds the signal stream — neither `DaemonAgent` (produces, never called) nor `CognitiveAgent` (called, never produces) could express an ingress. No API to declare a surface or choose a recipient (INV-5 by omission). **D11 (inbound):** `internal/ingress.InboundService` + a check in `Server.SignalStream` that runs BEFORE the Watcher — an ingress message is a conversational turn, so it goes to the chat lane and the planner never sees it (ADR-0080 D4 enforced for external traffic); an unregistered sender returns `ErrNotAnIngress` and falls through untouched. First contact opens a conversation with its address bound in the same write; owner is `ingress:<agent>:<external-id>` (per-sender pseudonym, isolates without proving identity); a closed conversation is not silently reopened. `Conversation.FindByDelivery` + migration 0006 index serve the lookup. **D12 (the port):** SDK `HTTPChatIngress` serves the same /open /turn /close contract as the Go ingress, with request/response correlation living IN the ingress (it is a property of that external protocol, not of Cambrian); premium `agents/airline_chat_ingress.py` is that class with the airline namespace. **Both paths coexist — the swap is UNMEASURED and the Go ingress is still the validated one until an airline run compares them.** **D13 (admin surface):** `ListIngresses`/`RegisterIngress`/`DeregisterIngress` on the premium `AccessPolicyAdmin` plane — registration MINTS a surface, so it sits behind the same operator auth as policy. This closed a gap that made the whole feature unreachable: the table and Go API existed but nothing exposed them, so nothing could ever become an ingress. Validation errors are InvalidArgument (inert registration, wildcard-looking namespace); unconfigured answers Unimplemented. Live-validated over the wire: registered, refused an inert spec, listed back, durable in `authz_ingress`. Live-validated: tables created at boot, `authz-ingress-sync` lifecycle non-blocking, migration 0006 applied through the real runner. |
| Closed tags + the crippled grant: deny-by-default per tag, and the one term that ADDS access | 0091 | **Implemented + live-validated.** ADR-0087 makes every fold an intersection, which buys "order affects the explanation, never the outcome" but cannot express *"only this path may reach this data"* — an unprofiled principal is unrestricted and `ForbiddenTags` is absolute, so a forbid broad enough to cover everyone also covers the principal you meant to allow. Default-deny + explicit grant is the dominant practice elsewhere (AWS IAM vs SCPs, K8s NetworkPolicy/RBAC, Zanzibar, MLS clearances); the narrowing-only model is the unusual one. **A tag may be declared CLOSED** (`CAMBRIAN_CLOSED_TAGS`, premium env, must be in the vocabulary): nobody reaches it unless granted. **`ScopeConfig.GrantedTags` reopens a closed tag** at a container and is crippled on purpose — it may name nothing but a closed tag (refused at write time otherwise), it can never override a forbid, and grants UNION while everything else intersects (still order-independent). **No global default-deny switch, ever** — SELinux is the cautionary tale, so a deployment that closes nothing is byte-identical in behaviour. Enforcement adds NO kernel concept: `closed − granted` is folded into `ForbiddenTags` at resolve time, so the absolute-forbid invariant IS the mechanism, and "a grant cannot beat a bound" falls out for free. Denials explain themselves (`AccessDecision.GrantedTags` + a closure note) because `forbidden_tag` for a tag nobody forbade is true and useless. Prerequisite: MCP tools became taggable (dynamically discovered tools arrived untagged, so no predicate could act on them). Two silent traps recorded in the ADR: `IsZero()` ignoring a new term drops grant-only policies, and `Build` rebuilding the Vocabulary discarded the closed set while the boot log claimed closure was active. |
| Natural-language policy authoring: an assistant that PROPOSES and cannot enforce | 0092 | **Implemented; not yet exercised against a live model endpoint.** Authoring one boundary means holding the ADR-0087 tag algebra, ADR-0091 closure/grant rules, four container kinds and the report-only rollout path at once — and a wrong policy in this model usually does NOTHING rather than something visible (ADR-0085 D11's coined tag, `IsZero()` dropping grant-only policies), which makes authoring help valuable and a writing assistant dangerous in the same breath. So: **the model has no write path.** `ProposePolicy` is read-only; applying means the operator calling the ordinary `SavePolicy`/`LinkPolicy`, so approval cannot route around a validation, an audit record or the rollout default (an `ApplyProposal` RPC was rejected — one round trip is not worth making the model's output a write). Proposals are validated with the checks the WRITE path uses (disagreeing with the store teaches operators to ignore warnings): blocking on a coined tag, a grant on an open tag, a link to a nonexistent group, no links, an empty rule; warning on any grant, an organisation link, and an unresolvable principal (a human operator is deliberately not in the agent registry). An approved proposal **lands report-only** — the model cannot ask to be enforced — while the blast radius is simulated as **enforced**, because simulating report-only reports 'no effect' for every proposal ever made; `SimulationBasis` distinguishes 'nothing to compare' from 'no effect'. The request is nonce-fenced as data (ADR-0063), which is defence in depth and NOT the load-bearing protection — validation plus human approval is. No LLM ⇒ `Unimplemented` and a convenience is lost, never a check. |
| The document as an ENTITY: one table split into six | 0093 | **Implemented + verified against a real database; NOT yet benchmarked.** `documents` held everything with an embedding — 14 `document_type` values sharing one HNSW index, from document chunks to the seeded tool/skill/agent_profile descriptors rebuilt on every boot. The serious defect was not the index: **a document was not an entity.** Nothing represented the source document a chunk came from — parentage was an id string convention (`{docID}-chunk-{n}`) plus a metadata key — so classification tags were COPIED onto every chunk at ingest with no authoritative row. Re-tagging was N updates with no transaction boundary, and a partial failure left a document **half-classified**: some chunks reachable, some not, nothing able to report the disagreement. Now: `documents` (new entity, authoritative `tags`), `chunks` (every vectorised recall unit, `document_id` **nullable** — an agent's memory of its own work has no parent), `document_sections`, `tools`, `skills`, `agent_profiles`. **All recall types stay in ONE table** so the split cannot have narrowed what a search finds. The chunk tag copy is KEPT as a derived cache (the filter is on the hot path behind a GIN index; a join would trade correctness for latency) but gains a single writer: `RetagDocument` moves the document row and every chunk in one transaction, and deleting a document cascades. `document_id` is written through a SUBSELECT so a missing parent yields NULL rather than failing the write — losing a memory over bookkeeping is worse than losing the link. Migration is ADDITIVE: `documents` is renamed to `documents_legacy`, never dropped. **The trap:** `update_activation_strength`/`apply_ebbinghaus_decay` name `documents` in their bodies and plpgsql resolves that at CALL time — renaming without redefining them would have left decay silently operating on the new empty table, every call succeeding and nothing happening. Both redefined against `chunks`, with a test. **Three follow-on migrations, all found by a LIVE BOOT after 9 integration tests and 50 green packages — every one of them lived in the seam between the schema and the code, which the tests exercised separately:** `documents.version + 1` in the ON CONFLICT target (a tool upsert emitting it while inserting into `tools`); **0008** adds `section_path` to the descriptor tables (0007 claimed "same column set as chunks" without checking it against the adapter's SELECT list, so every agent's interview-vector check failed); **0009** drops `document_edges.source_id`'s foreign key, which PostgreSQL had carried along with the rename so it pointed at the FROZEN legacy copy — ~700 silent `SaveStructuralEdges` failures per ingest, non-fatal only because that write is best-effort, losing the whole ADR-0060 structure graph. An edge endpoint may now be a chunk OR a section, so no single FK can express it (the baseline had already dropped the `target_id` twin for that reason). The row shape was an UNASSERTED CONTRACT between migration and adapter; `row_shape_test.go` now runs the adapter's exact SELECT against every table `GetByID` consults, with the column list deliberately duplicated rather than imported (importing would make it agree by construction). **Benchmarked, no regression:** locomo 0.310→0.380, document-qa 0.500→0.500, tau2-knowledge recall 0.435→0.486 / mrr 0.339→0.378 / hit_any 0.959→1.000. Read as "nothing got worse" rather than a measured gain — locomo gave 0.410 pre-0009 and 0.380 post-0009 on the same code path, so a single run-pair cannot support a precise delta. **A FIFTH bug lived in the measuring instrument:** tau2-knowledge scored a flat 0.0000 because its scorer read `metadata["session_id"]`, a key a prior change had renamed to `ingest_thread_id` — pre-existing (the frozen `documents_legacy` snapshot has zero rows with it) and unrelated to the split. Repointed at `metadata["document_id"]`, recall went 0.000→0.486. A broken scorer and a catastrophic regression produced the IDENTICAL row, which cost two wrong conclusions in one session; rows now carry `hits_returned`/`docid_unrecoverable` and a distinct `failure_kind` so a dead instrument cannot masquerade as a bad score. |
| Tool effect classes: closed `read|write|egress|spend|admin` set, `ClassificationTags`, registration validation | 0086 | Implemented (`domain/tool_effects.go`; `TOOL_MANIFEST` gains `classification_tags`/`effects`; `InMemoryToolRegistry.Register` is the normalization chokepoint. Un-migrated tools get DETERMINISTICALLY INFERRED effects flagged `effects_inferred` on `ToolOp`, because shipped manifests live outside this repo; `execution.tool_effects_strict` (default false) makes absence fatal once migrated. An effect outside the closed set is fatal in both modes) |
| Policy composition: groups, policy objects, links, LSDOU-analogue precedence, Block/Enforced, report-only, What-If, audit export | 0087 | Implemented (premium `authz/policy.go` + `simulate.go`; administration on the premium proto plane `AccessPolicyAdmin` per ADR-0073, so groups/policies/links never enter the pinned OSS contract. Cycles rejected at write AND survived at read; unsatisfiable rules refused at SAVE time; deny survives Block Inheritance. Groups/policies/links are durable in Postgres per the ADR-0087 D9 amendment — write-through durable-first, wholesale reload, LISTEN/NOTIFY across replicas — with the in-memory map as the read model. Residual: no authoring-history table) |
| Operator answer lane: grounded, `[n]`-cited answers (`AnswerMemory`) | 0081 | Implemented (proto contract `0060`; capability `memory-answer`). New RPC runs the agentic retrieval loop on the operator plane — a deliberate, capability-gated exception to the single-pass invariant (read-only, no auction, no `x-agent-id`). `QueryService.AnswerSystem` reuses `agenticSearch` for evidence then re-synthesizes over the evidence-only order via the optional `CitedSynthesizer` (`RetrievalDispatcher.SynthesizeCited` → Python `synthesize_cited`, which numbers chunks `[1..N]` and cites inline) so marker `n` aligns to `evidence[n-1]`. Distinct Python op keeps the benchmark synthesize (whose answer text the scorer substring-matches) marker-free. `operator.Service.AnswerMemory` maps evidence → `MemoryCitation[]`. Wired + capability advertised only when `execution.agentic_retrieval_enabled`. Builds + operator/network unit tests green. Residual: benchmark gate (operator-answer coverage on the `agentic-retrieval` suite) + live end-to-end run |
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
| Routing traces on the auction event (Gatekeeper L1/L2/L3 candidate funnel + winner margin + bid requirements on `AuctionEventOp`) | backlog ROUTE-02 | Implemented (behind `execution.routing_trace_enabled`, default true; funnel is a `json:"-"` output on `AuctionTask`, mapped to proto in `operator/mapper.go`; consumed by the `orchestration` bench suite; residual: full-120 `routing_trace` on/off latency arm → DECISIONS.md) |
| Capability contract (`Step.RequiredCapabilities` + planner emission from manifest vocabulary + L1 hard-gate `required ⊆ manifest.Capabilities`) | backlog ROUTE-03 | Implemented (arm `execution.capability_contract`, default off = byte-identical; planner uses `buildCapabilityClusterFromManifests`; fixed the `ManifestRecord` DTO that dropped `capabilities`; `InterviewWorker` now sources manifest caps — single source; the LLM CapabilityClusterer was RETIRED in ROUTE-04/ADR-0067. Offline eval: 100% emission, 0.95 routable, 0.62 gold-overlap. Residual: default-on pending plan-rubric no-regression) |
| Benchmark-mode kernel flags: `execution.plan_preview_only` (return plan JSON, skip DAG), `execution.disable_interviews` (skip graded LLM interview), `execution.disable_scout` (skip pre-plan discovery) | backlog ROUTE-03 | Implemented (all default false, eval-only; power the ROUTE-03 offline routing eval) |
| Scout usefulness logging: per-session `ScoutUsefulnessOp` on the operator feed (referenced-by-plan? ran-without-replan? scout latency) + suite scout cost-share | backlog ROUTE-08.A | Implemented (logging only, zero behavior change; `domain.ScoutUsefulnessEvent` emitted after execution in `server.Execute`, `DiscoveryReferencedByPlan` entity-id overlap, `DAGExecutor.ReplanCount()`; operator.proto `ScoutUsefulnessOp` oneof 24, contract 0049→0050 + cap `scout-usefulness`; `scoring.scout_summary` → `scout_cost_share`. Phase B invoke/skip gate = P3, deferred) |
| Watch observability: per-watch metrics + dry-run mode + journal backtesting | 0071 (backlog REACT-05) | Implemented (Prometheus/OTel export = residual). Premium `reactive/metrics.go` per-watch atomic counters (signals/fires/suppressions/dry-run/dead-letters/latency, incremented in `process`); `WatchConfig.DryRun` (evaluate + record would-fire, NEVER act); `reactive/backtest.go` `Backtest(cfg, afterSeq)` replays the journal (REACT-01) through a candidate condition without acting. Operator surface: RPCs `GetWatchMetrics`/`BacktestWatch` + `WatchConfigOp.dry_run`, via ports `domain.WatchMetricsReader`/`domain.WatchBacktester` (engine-satisfied, wired by type-assertion on the injected signal receiver; nil in OSS ⇒ Unimplemented). Contract 0053→0054 + cap `watch-observability`. Unit-tested. Residual: native Prometheus/OTel per-watch scrape; durable metric history |
| `schedule` watch source (cron-driven synthetic signals) | 0072 (backlog REACT-06) | Implemented (24h soak-drift = residual). Pure-Go 5-field cron parser `reactive/cron.go` (`@`-shortcuts, `*`/`*/N`/`A`/`A-B`/`A,B`, standard DOM/DOW OR-semantics, `Next`/`Prev` bounded-scan) + self-rescheduling scheduler `reactive/scheduler.go` held by the engine: per active schedule watch a `time.AfterFunc` at each cron instant emits `domain.Signal{FromAgent:"scheduler", Payload:{_scheduled_time,_occurrence,_cron}, Timestamp:scheduled}` via `OnSignal` — flows through the UNCHANGED condition/action pipeline. Wired via `RegisterConfig`(schedule-only)→upsert / `DeleteConfig`→remove / `SetConfigActive`→arm-disarm / `Start`→arm-all / `Stop`→stop-all; armed only between Start/Stop. `WatchSource.Cron`/`Timezone` + `WatchConfig.MissedFirePolicy` (`fire_once` catch-up on restart, exactly-once via REACT-01 schedule-time idempotency key, journal-gated; `skip` default). `WatchHandler` excludes schedule type from daemon ref-counting. Operator/proto: `WatchConfigOp.source_cron/source_timezone/missed_fire_policy`, contract 0054→0055 + cap `watch-schedule`. Cron + catch-up + delete/inactive tests pass. Residual: 24h soak-drift check; ui/cli re-vendor |
| Compile-time plugin architecture: `app.Plugin` + `Registry` + `Lifecycle` | 0074 | Implemented + reactive-lane live-validated (7/7). `app/plugin.go` generalizes the fixed `Options` hooks into a registry: **replace-one** points (`SetSignalReceiver`, `SetResourceSelector` — conflict = startup error) vs **add-many** (`AddTraceWrapper`/`AddGRPCService`/`AddLifecycle`/`SetAgentCallLogger` — composed). `applyPlugins` folds `Options.Plugins` into the effective Options + ordered `Lifecycle` set (started at boot, stopped reverse on shutdown). Tiering policy (ADR-0074): Tier-1 replace-one (Generator/Chunker/ResourceSelector/SignalReceiver), Tier-2 add-many, **Tier-3 never-pluggable** (scope/approval/budget gates, auction merit integrity, grounded-only, audit — pluggable security = privilege-escalation). NOT Go `.so` (CGO+Windows+version-lock ruled out); out-of-process gRPC reserved for untrusted plugins. Fixed a latent bug: the reactive engine was **never `Start()`ed** in a live kernel — now started via the plugin `Lifecycle`. Unit-tested (`app/plugin_test.go`: fold/conflict/lifecycle). Residual: widen extension points beyond reactive+selector (Chunker map needs threading through the memory stack; Generator) |
| AgentSource seam: unify agent registration behind a provider interface | 0075 | Implemented (plugin agents) + unit-tested. `app.AgentSource` (`Name()`+`DiscoverAgents(ctx) []domain.AgentDefinition`) is the seam agent registration flows through; `Registry.AddAgentSource`/`AddAgent` (regular, Tier-2 add-many) / `AddSystemAgent` (privileged — **explicit + logged grant**, `System=true` stamped, vs `AddAgent` forcing `System=false`). `registerPluginAgents` (app.go) discovers plugin sources and upserts via the same `reg.SetAgent` path as `registerModelAgents` — plugin agents participate in auction/scope/merit normally. No change to the bbolt filesystem seeder. Phase 2 (delivered): `BBoltAdapter.Seed` **inverted into discover-then-persist** (`storage.DiscoverFilesystemAgents` + `upsertDiscovered`; behavior-preserving — live-validated same 17 agents); reusable `app.FilesystemAgentSource` so a plugin contributes its own agents dir (health-gated: missing exec path skipped); **sidecar-preference** (Python agent prefers a sibling `<id>.manifest.json` over the `AGENT_MANIFEST` source-regex); **plugin MCP servers** (`Registry.AddMCPServer` → `MCPServerSpec`, connected alongside config servers, ADR-0043). Phase 3 (fully unified): the seam carries the **manifest** — `DiscoveredAgent{Definition, Manifest}` + idempotent `SetAgentWithManifest`/`UpsertDiscoveredAgent` (SourceHash idempotency preserved so no re-interview on reboot; unit-proven). The built-in scan is now **literally a `FilesystemAgentSource`**: `BootstrapStorage` uses `NewBBoltAdapterNoScan`, and the composition root registers the agents dir through a system-aware `FilesystemAgentSource` before the reconciles. Live-validated: fresh boot registers all 12 filesystem agents with `manifest=true` + correct system flags, no wrongful prune. `Seed` kept for tests/calibration-report. |
| Premium transport-plane extension: `app.Options.ExtraServices func(*grpc.Server)` | 0073 | Implemented + live. An inert OSS seam lets a downstream (premium) binary mount ADDITIONAL gRPC services (defined in its OWN proto) on the kernel server after the core services and before Serve, inheriting the server-level operator auth interceptors — the OSS `operator.proto` contract stays untouched. First consumer: the premium `ReactiveControl` plane (`EmitSignal`) that lets the benchmark harness inject synthetic signals to exercise the reactive guarantees. Composed through the ADR-0074 registry (`AddGRPCService`). Reconciled with invariant #2 (that governs the UI contract; this is a premium-only, authenticated, non-UI plane). |
| Additive licensed plugins: manifest, capability ownership, Build phase | 0082 | **Phase 1 implemented** (of 5; ADR is `Proposed`). `Plugin` declares a data `PluginManifest{ID, DisplayName, Version, Requires, Capabilities, Panels}`; `Manifest()` **replaces** `Name()` so a plugin has exactly one identity (its ID is also the future entitlement key). **D2 — capability ownership:** plugin-declared capabilities are collected by `applyPlugins` (deduped, order-stable) and appended to the operator handshake; the seven premium `watch-*` strings moved OUT of `app/app.go` into the reactive plugin's manifest, so the OSS kernel no longer names — or knows the meaning of — any premium feature. **D10 — dependencies:** `Requires` is topologically ordered (dependencies register first) and an unmet dependency **skips** the plugin non-fatally (`PluginStatus{State:"deps_unmet"}`, cascading to dependents) rather than failing boot — with subscriptions an unmet dependency is a billing combination, not a crash. Cycles/duplicate IDs remain hard errors. **D12 — Build phase:** optional `Build(KernelServices)` runs after the stacks exist and before serving, in dependency order, removing the pointer-capture hack where the reactive engine was built inside the signal-receiver hook and stashed for the gRPC registrar to find. **D7 (staged):** `KernelServices` added as an alias of `ReactiveServices` so plugins can migrate to the neutral name before the fields are split. **D11:** premium reorganized into isolated `plugins/<name>/` packages, guarded by `check-plugin-isolation.sh` (no plugin may import a sibling). Unit-tested (`app/plugin_test.go`: capability collection/dedupe, dependency order, non-fatal + cascading unmet deps, cycle + duplicate-ID errors, Build ordering). **Phase 2 implemented:** `EntitlementProvider` seam + chokepoint in `applyPlugins` (D3) — consulted BEFORE a plugin registers and BEFORE dependency resolution, so an unentitled plugin contributes nothing (no caps/lifecycles/agent sources/gRPC) and its dependents cascade to `deps_unmet`; nil provider ⇒ allow-all (OSS default); a provider error **fails closed**; an expired-but-in-grace plugin still activates and reports `expired`. `KernelServices` is now the real bundle type with `ReactiveServices` a **deprecated alias** (D7 rename half). **Chat extracted into its own plugin** (`cambrian-premium/plugins/chat`) — it takes `Manager`+`AcquireLLMToken` from the Build bundle instead of riding reactive's hook — and `execution.chat_manager_addr` was **removed from the OSS config**, closing a standing ADR-0057 D5 violation (the premium chat plugin reads `CHAT_MANAGER_ADDR`). **Not yet built:** Ed25519 licence verifier + issuance (D5, deferred until billing), business-package layer (ADR-0083), `ListPlugins` RPC (D9), plugin-owned operator protos (D8), `PluginStore` (D7 persistence half). **Sequencing note:** `PluginStore` is blocked *behind* D8 — it cannot replace `WatchStore`/`Journal` until `domain.WatchConfig` leaves OSS `domain/`, which ~11 operator-plane references prevent until the watch RPCs move to a premium-owned proto. |
| Conversation model + session pool + pluggable manager | 0084 | **Phase 1 of 5 implemented** (ADR is `Proposed`; **amends ADR-0080**, whose planner-bypass rule is unchanged and must not be weakened). Phase 1 = the data model that was simply missing: the kernel had **no `Message` or `Conversation` entity at all**, `domain.Session` is a *task* container despite its doc comment, operator `SendMessage` is `Unimplemented`, and the premium manager kept an in-memory transcript that died on restart. Shipped: `domain.Conversation`/`Message`/`ConversationProfile` + `ConversationStore` port; `PgConversationStore`; migration `0002_conversations.sql`; `Kernel.Conversations` (nil + warn when unmigrated — chat surfaces disable, the kernel still boots). `Seq` assignment is race-free by row lock (proved by a 20-goroutine concurrency test), and `ClientID` gives retry idempotency without burning a seq. Verified live against Postgres (6 integration tests) plus 6 pure-domain tests; the integration test applies the **real migration file** so it cannot drift from shipped DDL. **Phase 2 implemented:** `internal/agentpool` (bounded stateless worker pool, admission-gated shed, load-balanced dispatch, lost-vs-execution failure split) and `internal/chat` (`TurnService`), wired behind `execution.chat_pool_size` (**default 0 = disabled**) with `chat_pool_agent_id`/`chat_pool_queue_size`/`chat_pool_acquire_timeout_seconds`; the pool runs as a kernel-contributed `Lifecycle` (started at boot, drained on shutdown). Chat is now an **OSS** capability, so these keys name no premium feature (ADR-0057 D5 holds). 16 new tests, race-clean. The OSS chat worker ships as `agents/chat_agent.py` (the pool's default agent id), so OSS chat is self-contained. It is deliberately GENERIC — domain behaviour arrives as `policy` data, plus generic tool-posture flags (`tools_full`/`max_tool_rounds`) read off the turn — so it carries no benchmark/domain specifics. The premium chat manager keeps dispatching to the premium `chat_session_agent` (the airline/customer-service agent), so the benchmark's agent stays OUT of OSS and the τ²-bench airline path is unchanged by this work (no re-run required; the benchmark does not exercise the Phase-2 pool). **Phase 3 (operator half) implemented:** operator-plane conversation surface (`OpenConversation`/`SendTurn`/`CloseConversation`/`ListConversationMessages`), **contract 0060→0062** (0061 conversation surface + 0062 `ListConversations` sidebar), capability `chat`. `SendTurn` routes through the worker pool, NOT the planner — the old `SendMessage`/task-session path stays on the planner by design (task work vs chat turn = the D2 distinction). Client-supplied `conversation_id` ⇒ idempotent Open; principal-stamped ownership enforced per call; migration `0003` adds optional per-conversation policy. 7 handler tests + store integration green (both migrations); codegen idempotent. **Contract skew recorded:** UI pins 0060, CLI 0047 — not re-vendored; safe because the RPCs are additive and `chat` is capability-gated. **Not yet built:** Phase-3 D6 agent-plane `SendTurn` + daemon-agent manager + UI surface; Phase 4 session-noun refactor (`SessionToken`→`BudgetLease`, `streamID` split); Phase 5 `caller_scope` Phase-2 wiring. |
| Daemon supervision: auto-restart with exponential full-jitter backoff + flap quarantine + recovery/quarantine events | 0070 (backlog REACT-04) | Implemented (crash policy; heartbeat = residual). `agentmgr/restart_policy.go` `DaemonRestartPolicy` (per-stream sliding-window attempts; `Base·2ⁿ` capped at `Max`, full jitter; MaxAttempts→quarantine). `handleDaemonExit` captures spawn params → restarts after backoff or quarantines + emits `DaemonQuarantinedEvent`; success emits `DaemonRecoveredEvent` and the premium ReactiveEngine re-marks the stream available. Default-on via `execution.daemon_restart_*` (`max_attempts=0` disables = pre-REACT-04). Unit-tested + **live kill chaos test** (`daemon_chaos_test.go`: real subprocess kill→restart→quarantine via a `DaemonBootHook` seam, 5× stable). Residual: hung-daemon heartbeat liveness (SDK + monitor + signal-path), degraded-watch flag in `ListWatches` |
| Per-capability merit + bounded provisional exploration | 0069 (backlog ROUTE-06) | Implemented (arm `execution.per_capability_merit`, default off = byte-identical). D5 fix: `TaskEvent.Capability` stamped from the step's first required cap (`dag_executor`), aggregator `computeCapabilityStats` → `AgentProfile.CapabilityStats` (per-tag EWMA), L3 `computeMeritBreakdown(ctx, agent, requiredCaps)` reads tag-scoped success/trust with global fallback. Bounded exploration: shared `domain.ExplorationBudget` (N provisional wins/capability/window, nil-safe); Gatekeeper grants the provisional L2 bypass only while `Allowed`, Auctioneer `RecordWin` on a provisional win, `ExplorationBudgetExhaustedEvent` on exhaustion. Unit-tested (budget windowing, per-tag stats, tag-scoped merit). Residual: benchmark gate + N-sweep + per-tag data accrual; v1 keys by a single capability |
| Learned gatekeeper scorer | 0076 (backlog ROUTE-07) | 🔶 Partial (arm `execution.learned_scorer`, default off = byte-identical). `internal/metabolism/routescorer/`: inspectable pure-Go **logistic regression** over the EXACT `meritBreakdown`/ROUTE-02-funnel fields `[success_rate, trust_score, latency_term, cost_term, provisional]` (so a training sample is a direct funnel read — no reconstruction), standardized features, GD+L2, JSON-persisted with schema-drift guard. Offline pipeline `cmd/route07-scorer` {`extract` runs→samples, `train` → learned-vs-hand **AUC** on held-out split + `adopt` gate verdict}. Online: `Gatekeeper.RouteScorer` replaces the hand-weighted score (+ skips the cold-start penalty — provisional is a feature) when armed + `learned_scorer_model_path` loads; missing/invalid model ⇒ hand weights (never a silent 0). Unit-tested (train/AUC/save-load/schema/gate on synthetic). Residual: the offline WIN over the calibrated baseline needs accrued funnel+verifier runs (`resource_selector=auction`); then online A/B + DECISIONS.md; GBT/retrain-job are follow-ups |
| Bid calibration: offline isotonic (PAVA) per-agent calibration from verifier outcomes + shrinkage; arm-gated online winner-selection by calibrated confidence | 0068 (backlog ROUTE-05) | Implemented (offline-first). Pure `internal/metabolism/calibration` (isotonic PAVA + per-agent maps shrunk to a fleet-global prior below `bid_calibration_min_samples`). `AgentRepoDecorator.BidCalibrationSamples` extracts (agent, bid_confidence, verifier_score) from verified `TaskEventRecord`s. `Auctioneer.Calibrator` (nil-safe) selects the winner by CALIBRATED confidence under arm `execution.calibrated_bids` (default off = raw self-report, byte-identical); the `MinAuctionConfidence` floor + recorded bid keep the RAW value. Offline artifact `cmd/calibration-report`. Unit-tested (calibration math + winner-flip). Residual: offline replay lift + online enablement (DECISIONS.md) pending accrued verified-event data; v1 keys by agent not agent×capability |
| Capability vocabulary: retired the LLM CapabilityClusterer + deterministic capability normalization | 0067 (backlog ROUTE-04) | Implemented (reframed per user: the embedding/fuzzy canonicalizer was REJECTED — fuzzy cosine merges risk wrong merges, e.g. `file-read` ↔ `file-write`, which misroute worse than the variance they fix. Delivered: **deleted `internal/supervision/clusterer`** + its `SupervisionStack`/`SweepTrigger` wiring; capabilities are the ones agents declare. `domain.NormalizeCapability` (lowercase/trim/collapse `-`,`_`,space) applied to BOTH sides of L1 `PassesDeclaration` + the planner vocabulary under `execution.canonical_vocab` (default off = ROUTE-03 verbatim). Format/typo variance matches with **zero wrong-merge risk**; cross-word synonyms (`browser`≡`web-navigation`) deliberately out-of-scope → curated vocabulary. Unit-tested. Residual: A/B confirm + L2 threshold sweep) |
| Container distribution: multi-stage `Dockerfile` + top-level `docker-compose.yml` + GHCR publish workflow (CPU + `-cuda`) + opt-in model pre-fetch | 0066 (backlog PLAT-04) | Partial (artifacts authored: `cambrian-core/Dockerfile` (golang:1.25 → python:3.12-slim, kernel+agents colocated, non-root, PLAT-01 venv, PLAT-03 `/healthz` HEALTHCHECK, PLAT-02 migrate-on-boot), `Dockerfile.dockerignore`, monorepo-root `docker-compose.yml` (db+kernel+pagerank+optional ollama, health-gated), `.github/workflows/publish-image.yml`. **Build context = monorepo root** (the SDK is the sibling `sdk/` repo). GPU = `TORCH_INDEX_URL` wheel swap; model pre-fetch = `PREFETCH_MODELS=1`. **Validated:** the Go builder stage compiles both binaries in-container. **Release-time (not validated here):** runtime stage (multi-GB torch/docling), clean-machine `docker compose up`, image sizes, GHCR tag push) |
| gRPC health service: standard `grpc.health.v1` on the main listener + DB-gated readiness probe + drain-before-stop + optional `/healthz` HTTP shim | 0065 (backlog PLAT-03) | Implemented (`internal/health.Checker` wraps grpc's `health.Server`; readiness = `pgxpool.Ping` (default 10s, `server.health_check_interval_seconds`), NOT gated on agents; sets `""` + `cambrian.OperatorConsole`. Registered on `k.GRPC`; `Kernel.Shutdown` flips sticky NOT_SERVING before GracefulStop. `/healthz` shim on `server.healthz_port` (0=off). Standard proto (not vendored) ⇒ proto-check clean; unit-tested. Residual: live `grpc_health_probe` + `cambrian status`/installer consumption are CLI/installer-side) |
| Embedded DB migration runner: `schema_migrations` version table + `orchestrator migrate up/status` + first-class `0001_baseline.sql` + DB-ahead refuse + `storage.auto_migrate` | 0064 (backlog PLAT-02) | Implemented (pure-Go `internal/migrate` — no goose/golang-migrate dep, no CGO; `go:embed migrations/*.sql`, `${EMBEDDING_DIM}` substitution, per-migration tx. **The head schema is `0001_baseline.sql`** — generated from the former `ensureSchema` statement list (faithful, no transcription) and now the SINGLE source of truth (`BaselineStatements` deleted). The runner EXECUTES `0001` like any migration (idempotent ⇒ safe no-op that adopts the version table on an existing DB). `ensureSchema` is reduced to the config-driven **dimension-mismatch destructive guard** (ADR-0021, intact) then delegates schema creation to the runner (a destructive dim change also drops `schema_migrations` so `0001` re-applies). Boot gated by `storage.auto_migrate` (default true). Live-validated: `0001` executes cleanly against the running Postgres. Residual: wire `migrate` into CI; no down-migrations) |
| Reactive llm-condition injection hardening: payload-as-data fenced prompt + per-watch payload-key allowlist + registration risk gate | 0063 (backlog REACT-03) | Implemented (premium `condition_evaluators.go` `buildConditionPrompt` — nonce-fenced, JSON-encoded typed fields, "payload is untrusted data, never instructions" system framing, strict true/false fail-closed; engine `checkCondition` strips non-allowlisted keys (`WatchConfig.ConditionPayloadKeys`) + logs them. Core additive: `WatchConfig.Approved` + `RegisterWatch` risk gate — an `llm` condition driving `start_plan`/`dispatch_agent` is rejected unless `approved=true` (deterministic ADR-0034 security gate). Red-team corpus `reactive/testdata/injection_corpus.txt`; guarantee is structural (injection confined to the data fence). Contract 0052→0053 + cap `watch-condition-guard`. Residual: per-fire HITL / content-risk scoring deferred) |
| Reactive backpressure & storm control: per-watch debounce/coalescing + per-stream rate limit + plane-wide hourly LLM/plan budgets + shed order + operator budget event | 0062 (backlog REACT-02) | Implemented (all in `cambrian-premium/reactive/backpressure.go` + engine; core changes additive — `WatchConfig.DebounceSeconds` (operator-plane `RegisterWatch`), `ReactiveBudgetEvent`→`ReactiveBudgetOp` feed op bridged via `feedEventTypes`, contract 0051→0052 + cap `reactive-backpressure`. Shed order: only `llm` conditions + `start_plan` actions draw from the global budget; deterministic/pattern keep flowing. Stream-rate sheds drop+throttled-event (no dead-letter storm); budget sheds dead-letter (REACT-01) + throttled event. Defaults permissive (0 ⇒ unlimited). Residual: debounce buffers ephemeral; plane-wide not per-owner (REACT-07); circadian energy tie-in deferred) |
| Durable reactive execution: signal journal + per-watch ack cursor + exactly-once action idempotency + dead-letter + replay-on-start | 0061 (backlog REACT-01) | Implemented (storage in `internal/storage/bbolt_reactive.go` — 4 bbolt buckets journal/cursor/idempotency/dead-letter; seam `app.ReactiveJournal` on the `ReactiveServices` bundle, implemented by the OSS decorator `internal/kernel/reactive_journal_decorator.go`, consumed by premium `reactive.ReactiveEngine`: append-before-eval → `MarkExecutedOnce` exactly-once claim → action → dead-letter; conservative per-watch ack cursor never skips an unprocessed signal; `Start` replays from cursor, TTL-expired signals dead-lettered not re-run. **A nil journal preserves the pure in-memory engine** — OSS default. Operator read surface `ListWatchDeadLetters`, contract 0050→0051 + cap `watch-deadletter`. Residual: periodic journal `Prune` GC is implemented but not yet scheduled by a caller; ui/cli proto not re-vendored to 0051 (skew recorded); a `stress`-suite forced-*process*-restart scenario is a follow-up — exactly-once + no-loss are unit-proven) |
| Reproducible agent envs: auto-generated per-agent `requirements.txt` + union `requirements.lock` + drift check + manifest `python_deps` dependency self-check | backlog PLAT-01 | Implemented (`scripts/gen_agent_requirements.py` / `make agent-reqs[-check]` — AST import analysis + `importlib.metadata`, no pip-tools; deterministic drift gate. `agentmgr/python_deps_check.go` `find_spec` pre-check names a missing dep at boot instead of a silent ImportError crash. Residual: system agents don't yet declare `python_deps`; clean-venv install is the CI/PLAT-04 gate) |
| Agent sandboxing Tier 0: env allowlist (agents no longer inherit kernel secrets) + memory resource caps | backlog SEC-01 | Implemented (`agentmgr/env_allowlist.go` deny-by-default env in `buildAgentCmd`, secrets stripped; `process_caps_{windows,linux,other}.go` pure-Go **no-cgo** memory cap — Windows Job Object kill-on-close, Linux RLIMIT_AS — behind **per-agent** `manifest.memory_limit_mb` (declared limit wins; **system organs exempt from the global default** so docling/reranker/torch are never killed by a fleet-wide number) with `execution.agent_memory_limit_mb` (default 0) as the default for user agents; `execution.agent_env_passthrough` for non-secret extras. Live-verified agents spawn+run with keys stripped. Residual: memory-bomb live-kill test, per-agent scratch dir, Unix pgroup grandchild-kill) |
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
| Pre-plan Scout: deterministic-first discovery (probe registry filesystem/system/http; LLM demoted to opt-in; selection matches request words against real names, Aider-style) | 0078 | Implemented (amends 0051; `internal/discovery/`) |
| Parametric fan-out steps (discover-N → do-N; deterministic append-expansion of the plan, no replan) | 0078 R2 / 0051 D10 | Implemented (`domain/fanout.go`; DAGExecutor + `max_fanout_width`; planner emits `fan_out_over`) |
| Config-load robustness (configs/.env/agents_dir/data_dir resolved against the binary, not cwd) | — | Implemented (`config.ResolveBaseDir`; fixed the Scout silently disabling under a benchmark supervisor) |
| Roster latch (agent→capability roster seeded from manifests + heartbeat) | 0047 | Implemented (`internal/substrate/operator/roster_latch.go`; survives `disable_interviews` + late `--no-supervise` subscribers) |
| Agent-loop observability (`AgentStepOp` on the feed: memory_query thrash + retrieval-provenance poisoning) | 0047 | Implemented (contract 0058, cap `agent-steps`) |
| LLM-exchange provider tap (`AgentLLMExchangeOp`: full prompt+completion of every managed-proxy agent turn on the feed; the ordered sequence reconstructs an agent's whole ReAct loop for benchmark review) | 0079 | Implemented (contract 0059, cap `llm-exchange`, gated `execution.capture_llm_exchanges` default off; live-only/never-replayed, fire-and-forget, zero behavior change) |
| Experiential memory (step-result / plan-scene / procedural / negative-edge / episodic / scope-promotion write-back) | 0015/0025/0029/0030/0034/0049 | **REMOVED** (unwired 2026-07-18; implementations retained; corpus ingestion kept; pending redesign) |

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
| `app/` | Composition root. `app.Run(ctx, opts)` wires every subsystem; `app.Options` is the open-core extension seam (premium injects via hooks). `app/plugin.go` (ADR-0074/0082) generalizes the hooks into a `Plugin`/`Registry`/`Lifecycle` system with a data `PluginManifest` (id/version/requires/capabilities/panels); `Options.Plugins` are dependency-ordered and folded by `applyPlugins`, then constructed by `buildPlugins` (the ADR-0082 D12 Build phase). |
| `cmd/orchestrator/` | Thin `main` shell over `app.Run`. |
| `internal/kernel/` | Subsystem assembly: four stacks (`MemoryStack` / `AwarenessStack` / `MetabolismStack` / `SupervisionStack`) and `ProvideServer`. The only cross-subsystem wirer. |
| `internal/awareness/` | Planner / Cortex. Produces `ExecutionPlan`; owns the Zero-Hardcode rule at the LLM layer; `ReplanHandler`, `ConsolidatorAgent`, `XMLParser`. |
| `internal/metabolism/` | Auctioneer, AgentManager, Gatekeeper, verifier pool, interview worker, DAG executor, A2A connector. The bidding and selection machinery. |
| `internal/memory/` | LTM: pgvector store adapter, hippocampus (procedural templates), KG²RAG retrieval (`kgExpand`), scene/edge writers, `WorkspaceStage`, `SpreadingEngine`, `ProfileStore`, `ArtifactVault` (CAS), `MemoryManager` / `MemoryAgent` / `MemoryWorker`. **Chunking pipeline (ADR-0060):** `chunker_registry` (data-driven `SourceType → ext → default` routing, Zero-Hardcode clean), `chunk_relations` (per-chunk 512B parent+linear+sibling metadata), `chunkers/` subpackage (the 5 implementations: `option_c` / `recursive_character` / `ast_go` / `markdown_header` / `late`), `OptionCChunker` (the 115-line back-compat floor), `ChunkDocument` shim over `OptionCChunker`. **Structure-aware pipeline (ADR-0060 default):** `structure_graph.go` (leaves-as-chunks + `BuildStructureGraph`), `structure_retrieval.go` (`applySectionConstraint`), `neighbor_window.go` (off by default), `anchor_query.go` (document-local anchor constraint), plus the `docling_agent` parse RPC and `postgres/structure_store.go` writer. **Document entity (ADR-0093):** `document_entity.go` defines `SourceDocument` + the optional `DocumentStore` port (a document has no embedding and is never a search result, so it is deliberately NOT part of the vector store); `postgres/document_store.go` implements it, `postgres/document_retag.go` carries `RetagDocument` — the transactional re-classification that makes a half-classified document impossible — and `tableFor()` in `pgvector_adapter.go` routes every read and write to the table that owns the type. |
| `internal/supervision/` | Gatekeeper, verifier pool, `ProfileAggregator`, `SynapticWatcher`, `Watcher` (signal-to-inspiration), `MemoryLifecycleManager` (event-driven session lifecycle, replaces `CircadianRhythm`). |
| `internal/substrate/` | gRPC server (`network`), session, OperatorConsole plane (`operator/`), synaptic event log, knowledge-graph extractor dispatch. |
| `internal/authz/` | Access-control ENFORCEMENT points (ADR-0085). `EnforcingVectorStore` (fail-closed read chokepoint), `EnforcingStoreWriter` (write classification), artifact filters, and the gRPC surface interceptor. Contains no policy: every question is delegated to `domain.Authorizer`. Not pluggable — a missing plugin must mean "no policy", never "no check". (Replaces `internal/scope/`, whose DECISION half moved to `cambrian-premium/authz`.) |
| `internal/infrastructure/` | Adapters: `postgres/` (pgx + pgvector), `llm/` (LLM client + `LLMProvider` broker), `mcp/` (MCP connector), BBolt storage DTOs. |
| `internal/tool/` | System-tool lifecycle (Python modules, `ToolRegistry`, confined `ProcessHandler`, jail sweep into CAS). Will be superseded by ADR-0039's kernel-owned registry. |
| `internal/skill/` | System-skill discovery and registry; agent skills live in the Python SDK (`agent.local_skills`). |
| `internal/storage/` | BBolt adapter (DTOs only, zero `domain/` imports). `AgentRepoDecorator` wraps storage as the domain interface; `BootstrapStorage` is the only constructor. Durable reactive execution lives in `bbolt_reactive.go` (ADR-0061: journal/cursor/idempotency/dead-letter buckets); the decorator surfaces it as `app.ReactiveJournal` via `internal/kernel/reactive_journal_decorator.go`. |
| `internal/mapper/` | Proto-to-domain translation; the only bridge between `pb` and `domain` types. |
| `internal/metabolism/calibration/` | Pure bid-calibration (ROUTE-05 / ADR-0068): isotonic PAVA + per-agent maps with shrinkage. Fit offline from the event log; applied by the `Auctioneer.Calibrator` behind `execution.calibrated_bids`. Offline report: `cmd/calibration-report`. |
| `internal/health/` | gRPC `grpc.health.v1` checker (PLAT-03 / ADR-0065): DB-gated readiness probe + drain-before-stop + optional `/healthz` HTTP shim. Registered on the main gRPC listener. |
| `internal/migrate/` | Pure-Go DB migration runner (PLAT-02 / ADR-0064): `schema_migrations` version table, `go:embed migrations/*.sql` (head schema is `0001_baseline.sql` with `${EMBEDDING_DIM}`), per-migration tx, DB-ahead guard. `postgres.ensureSchema` (dimension guard only) delegates to it; `orchestrator migrate up/status` is the CLI. |
| `domain/conversation.go` | ADR-0084 D1: `Conversation`, `Message`, `ConversationStatus`, `ConversationProfile` (+`Recall()`), and the `ConversationStore` port. Pure domain — validation only, no persistence. |
| `internal/infrastructure/postgres/conversation_store.go` | `PgConversationStore` (`domain.ConversationStore`). `Seq` is assigned by `UPDATE conversations SET next_seq = next_seq+1 ... RETURNING` **inside the append transaction**, so it takes the conversation row lock and concurrent turns cannot collide (a `SELECT MAX(seq)+1` would race). The same statement carries the open/closed gate. Unlike the older audit store it does **not** create its own tables — schema is owned by migration 0002 (ADR-0064) so the DDL exists once. |
| `internal/migrate/migrations/0002_conversations.sql` | ADR-0084 D1 schema: `conversations` (+`next_seq` counter) and `conversation_messages` (`UNIQUE (conversation_id, seq)`, partial unique index on `client_id` for retry idempotency). |
| `internal/agentpool/` | ADR-0084 D4: a bounded pool of interchangeable daemon workers over the `SpawnDaemon`/`CallDaemon`/`StopDaemon` seam. `Size` workers on synthetic stream ids; an admission gate of `Size+QueueSize` sheds **immediately** with `ErrPoolBusy` rather than queueing without bound; `AcquireTimeout` sheds a waiter. Dispatch is load balancing, **not** sticky — any worker serves any turn because the turn carries its own context. Start is all-or-nothing (a partial pool would silently serve at reduced capacity). On failure it distinguishes a **lost** worker (not live ⇒ respawn + `ErrWorkerLost`, the turn never ran) from an **execution** failure (worker still live ⇒ surface as-is, no respawn) — the pool never retries, because only the caller knows whether re-running a side-effecting turn is safe. Free of chat concepts, so reusable. |
| `internal/chat/` | ADR-0084 D4 OSS turn path: `TurnService.Turn` loads the conversation, appends the user message, threads a bounded history window, dispatches ONE turn to the pool, and appends the reply. Retry-safe — a replayed `ClientID` returns the original reply without dispatching again. Emits the ADR-0080 per-turn handoff contract (`_conversation_id`/`policy`/`transcript` metadata + `_session_token_id` lease) so `chat_session_agent` works unchanged, plus `profile`/`recall_lookup` so the conversation's posture travels with the turn (D7). The planner is not involved. |
| `internal/substrate/operator/conversation.go` | ADR-0084 D9: the operator-plane chat lane — `OpenConversation`/`SendTurn`/`CloseConversation`/`ListConversationMessages`. `SendTurn` routes through the pool (`TurnService`), NOT the planner. Ownership is stamped from the resolved principal and enforced per call (non-owner ⇒ PermissionDenied). `conversation_id` is client-supplied ⇒ Open is idempotent. Capability `chat`, gated on `k.ChatTurns != nil`. |
| `app/conversation_ops.go` | Composition-root adapter binding `operator.ConversationOps` to the conversation store + `TurnService` (keeps the operator package free of kernel handles). |
| `internal/migrate/migrations/0003_conversation_policy.sql` | ADR-0084 D9: optional per-conversation `policy` column (threaded into every turn; set once at open). Append-only migration (0002 already applied). |
| `agents/chat_agent.py` | ADR-0084 D4: the OSS conversational worker the chat pool runs (id `chat_agent`). Owns ONE turn in a single ReAct loop — the planner is reachable only via an explicit yield, never by decomposition (ADR-0080). Stateless per call: policy/transcript/posture all arrive on the task, which is what lets interchangeable pooled workers serve any turn. Honours `recall_lookup` (D7) so a customer-facing conversation cannot reach shared memory, and reads `tools_full`/`max_tool_rounds` per turn so one agent serves both light chat and tool-grounded customer service. A spoken-only guardrail replaces any internal error/reasoning/trajectory leak with a non-committal fallback — never a fabricated action. |
| `internal/telemetry/` | The only package importing OTel. `Bridge` translates `TelemetryObserver` calls to Prometheus + OTLP. Runtime-config-gated, no build tags. |
| `internal/config/` | Koanf 11-layer loader (ADR-0024); merges built-in defaults, `tuning.json`, `tuning.local.json`, `config.json`, `config.local.json`, `embedder.json`, `embedder.local.json`, `providers.json`, `providers.local.json`, `mcp.json`, `CAMBRIAN_*` env. |
| `internal/centralexec/` | Phase-1 modules for the Central-Executive Planner (ADR-0037); gated on the A/B spike against the auction. |
| `internal/router/` | Universal input router (ADR-0031); classifies inbound traffic into reflex / cognitive / signal lanes. |
| `internal/reactive/` | Reactive rule engine plug surface (OSS side; premium `cambrian-premium` provides the implementation via `app.ReactiveServices`). Durability (ADR-0061) is injected through the bundle's `Journal app.ReactiveJournal` field — the engine appends before eval, claims exactly-once, dead-letters failures, and replays from the cursor on start. |
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
  tags, and importance into this path. Contract `0057` adds the
  **binary upload lane**: `content`+`filename` carry raw file bytes,
  which `app.operatorIngestDoc` turns into an `ExternalDocument` whose
  `SourceType` is derived from the filename extension (`pdf`, `docx`, …)
  and whose new `Data []byte` field carries the bytes. `persistChunks`
  base64-encodes `Data` into `StructureParseRequest.DataB64`, which is
  what opens the docling_agent's `_BINARY_TYPES` gate and reaches the
  Docling backend. Before `0057` **no production caller ever set
  `DataB64`**, so the Docling binary path was unreachable and a PDF
  could only ever be ingested as flattened text. The `context` field is
  folded into the body as a `## Context` section (not metadata) so it is
  chunked, embedded, and citable. `ProcessSync` returns only after
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

Access control is split (ADR-0085). The kernel owns the **enforcement
points** (`internal/authz/`) and always asks `domain.Authorizer` before it
acts; the **decision** — the tag algebra, principal resolution, groups,
policy objects, links, precedence — lives in the premium `authz` plugin.
There is no flag: the chokepoints run unconditionally, and only the answer
varies. `execution.scope_enforcement_enabled` is GONE; it turned the
chokepoint off rather than the policy, so an unscoped deployment had no gate
at all.

**The failure direction inverts at the seam, deliberately.** OSS fails
**open** (`AllowAllAuthorizer` — unrestricted is the correct and only
semantics for a single-tenant open-source deployment); the plugin fails
**closed** (an unresolvable principal is denied). Getting it backwards makes
OSS unusable or premium insecure.

Four resource kinds are governed by one algebra: memory (`metadata.tags`),
skills (`Skill.ScopeTags`), agents (`AgentDefinition.ClassificationTags`), and
tools (`SystemTool.ClassificationTags` + closed-set `Effects`). A tag says
what a resource is ABOUT; an effect says what the invocation DOES to it, and
both must pass (ADR-0086). Write classification stays **kernel-derived** from
operator-set write tags; a principal may only narrow, never broaden.

Two properties are load-bearing and easy to lose:

- **Skill grants are INTERSECTED, never unioned** (ADR-0085 D4). A skill is
  gated on retrieval by its tags and loading it activates tool grants, so
  without the intersection skill VISIBILITY becomes a privilege-granting
  operation. A tool policy refuses is clipped (`skill_grant_clipped`), the
  rest of the skill still loads.
- **No silent empties.** A policy-caused zero-result carries its reason, on
  both `QueryMemoryResponse.policy_note` and the agent plane's
  `MemoryResponse.policy_note`, and `ExplainAccess` answers "why can/can't
  this principal see this?" without performing the access.

The **surface clamp** (`domain.SurfaceRef`) is the Windows loopback analogue:
the entry point a request arrived through can clamp what may be done
regardless of who is asking. It is derived from the TRANSPORT by a gRPC
interceptor, never from a payload. It is only as trustworthy as the
transport — **do not treat it as a security boundary on a remotely-reachable
deployment until SEC-03 (TLS + operator authn) lands.**

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
| **Policy cannot be authored against a human principal from the UI** (ADR-0085 / ADR-0091) | Deferred deliberately on 2026-07-27. A `principal:<operator>` link RESOLVES correctly today — `applicable()` matches a principal link by id regardless of principal kind — so per-user operator policy works on the wire. What is missing is authoring: the console's principal suggestions come from the AGENT list, so a human account cannot be selected as a link target. Consequence: per-operator permissions are API-only. Related and also open: operators are not exempt from closed tags (ADR-0091), so a newly closed tag denies operators until a grant is linked at `surface: operator` — correct as a failure direction, but nothing surfaces which closed tags are ungranted. |
| **The surface clamp engages only for registered ingresses** (ADR-0085 D7 / ADR-0090 D2/D3) | `domain.Session.Surface` documents itself as "the entry point this session was OPENED on, decided ONCE by the kernel", and `SessionManager.SessionSurface` + `authz.ResolveSurface` read it. It now HAS a writer (ADR-0090 D3): `SessionManager` stamps the surface of a **registered ingress** onto sessions that ingress opens, through the `domain.IngressResolver` port in OSS and the `authz_ingress` Postgres registry in premium (write-through + LISTEN/NOTIFY, same machinery as the policy store). **Only registered ingresses stamp**, and that is load-bearing: `ResolveSurface` prefers a session's stored surface on the assumption it is NARROWER, which is true for an outsider ingress and false for an operator-created session — so stamping every session would silently widen ordinary ones. Residual: a deployment that registers no ingress resolves every surface from the transport, so the clamp does nothing until something is registered; and whoever can write `authz_ingress` can mint a surface, hence it is policy-grade data. |
| `DialAgent` context timeout | `grpc.WithBlock()` with no deadline in `AgentConnector`; the `bootAgent` UDS poll is safe, but a long-lived hang on a stuck agent process has no upper bound. |
| Agent liveness | `grpc.health.v1` is wired; a proactive liveness probe (reap zombie processes) is not. A crashed agent with a still-open stream will not be detected until the next request fails. |
| Consolidation atomicity race | `consolidateCluster` does non-atomic `Ingest` + `DeleteBatch`. A crash between the two leaves duplicates; a crash before leaves the old memory plus a new one. Lives in `internal/memory/`. |
| Anomaly detection | Stub in `memory_agent.go`; the hooks are present, the logic is partial. Memory pressure events are emitted but not yet acted on. |
| `RunGrantOverlay.Clear` on session-end | Unwired. The session-keyed overlay is single-use, so there is no cross-run leak, but there is no clean session-completion hook either. Per-run grants accumulate as ephemeral state until the session ends. (ADR-0046) |
| Unary `Execute` HITL | The `PauseController` is only active on the streaming path. A unary `Execute` call cannot pause for human approval. |
| Tool sandbox depth | OS process + caps only (the Wasm layer was cancelled). rlimit / cgroup / seccomp is a Unix follow-up. |
| ADR-0037 `CapabilityBelief` loop | Unwired by design. The EFE-vs-auction A/B spike has not run; Phase-1 modules live in `internal/centralexec/` but the active-inference resource binding is not connected. |
| Access-policy persistence (ADR-0087) | Groups, policy objects, and links live in an IN-PROCESS store; a restart loses authored policy. Agent scopes and write tags remain durable (Postgres `agent_scopes`). The store is behind `PolicySource`/`EffectGrantSource`, so a Postgres implementation is additive — but until it exists this is a demo-grade authoring surface with production-grade semantics. |
| Surface clamp depends on SEC-03 (ADR-0085 D7) | The surface is derived from the gRPC service, which is sound on localhost (the transport is the process boundary) but only as trustworthy as the transport on a remote deployment. Not a security boundary off-localhost until TLS + operator authn land. |
| Tool effect migration (ADR-0086) | Shipped `tools/*tool.py` manifests live outside this repo, so effects are INFERRED for un-migrated tools and flagged `effects_inferred` on `ToolOp`. Flip `execution.tool_effects_strict` once the catalog reports none. |
| No access-policy benchmark | Access control has no suite. The DDD mandate's step 6 applies: deny/allow fixtures, an escalation corpus, and an explanation-completeness check are the honest follow-up. |
| Ownership ACL removed (ADR-0085) | The hardcoded `source_agent_id == callerID` filter is deleted. A deployment that relied on per-agent ownership isolation must express it as a policy over the `provenance:source=<agent>` tag the write path already stamps. |
| `embedder` 768→1024 dim migration | Resolved for recall (`bge-large`@1024 + query prefix, LoCoMo recall@100 0.47→0.94). Residual: the ADR-0044 tool menu is not re-embedded; `tool_retrieval_floor>0` would let the tool path benefit too. Stays sub-1B so it co-resides with `qwen3:8b` on 12 GB. |
| Stage-A blend weights (ADR-0054) | Tuned and adopted in `config.json` (cosine 0.40, lexical 0.20, coherence 0.05, confidence 0.10, pagerank/recency/activation 0.0). Coordinate-descent sweep over the operator plane (`SetRuntimeConfig` hot-swap), conv-split train/test: baseline @10 0.673/0.662 → 0.763/0.766. Takes effect on restart. |
| Stage-B reranker (ADR-0054) | Built and DEFERRED (`reranker_enabled=false`). bge cross-encoder as a warm system organ (`reranker_agent`, mirrors `kg_extractor`) regressed recall on LoCoMo (recall@1 0.168→0.089): the Stage-A blend already orders the top-50 well. Ollama can't serve cross-encoders; GPU revisit = CUDA torch + `bge-reranker-v2-m3`. Re-tested 2026-07-06 on a 2-PDF corpus: still net-negative (specific-fact top-5 0.50→0.30, the cross-encoder disagreeing with the correct chunk) and ~8 s/query on CPU. Two latent bugs were fixed while there and kept: `sentence-transformers` was uninstalled (agent crashed → silent fail-soft, never ran), and the model loaded in `__init__` blew the 10 s agent-boot socket timeout (`instance_manager.go`) → re-spawn per query; now **lazy-loaded** (`_ensure_model`), so it boots instantly and stays warm. Enable only with a GPU. |
| pgvector dim-migration is destructive | An embedding-dimension change recreates every EMBEDDING table — `chunks`, `tools`, `skills`, `agent_profiles` (drop+recreate; pgvector can't `ALTER` a VECTOR dim). ADR-0093: `documents` and `document_sections` are untouched, having no vector. The boot path now refuses to start if memory docs are present unless `ALLOW_DESTRUCTIVE_DIM_MIGRATION=1` is set. The guard only protects inside the running binary; rebuild before launch. |
| Model-failover requires ≥2 generators | Automatic model failover via `selectModelCandidates` only fills `Fallbacks` when `FindModelCandidates` returns ≥2. A single-generator config has one candidate and no rung to fall back to. |
| MCP tool OAuth / per-tenant budget | `mcp` tool price in the menu and per-tenant OAuth/budget are deferred. The `mcp:<server>/<tool>` shape is wired and injection-scanned. |
| Re-chunking backfill (ADR-0060) | When the operator changes the `chunker.*` config, existing documents are not re-chunked in v1. A backfill path is future work; operators re-ingest the affected sources to apply a new strategy. |
| Source-document entity size (ADR-0060) | Source-doc entities are GC-exempt (ADR-0060 D8) and accumulate; operators must size the `ContentStore` (CAS) and the entity table accordingly. A dedicated size-cap / tiered-storage ADR is a follow-up. |
| Late-chunker gate is implicit (ADR-0060) | Setting only one of `chunker.late.enabled` / `embedder.supports_long_context` silently routes to `OptionCChunker` (logged warn at resolve time, not a hard error). Intent is "fail to the known-good default," but it means the gate is *implicit* for any operator who only sets one of the two flags. |
| Future chunkers (ADR-0060) | **Implemented 2026-07-06 (default on) — see *Structure-aware ingestion pipeline* above:** PDF structure-aware parsing (via **Docling**, not Marker/LayoutLMv3), hierarchical chunk relations (`section_path` / `section_ltree` / `parent_section_id`), and chunk-level graph edges in `document_edges` (`part_of` / `next`). Still deferred: tree-sitter multi-language AST chunker (cgo-blocked; the v1 AST path is `go/ast`-only), `sibling_index` chunk relations, LLM-driven chunkers (Contextual Retrieval / Propositions / Small-to-big — explicitly rejected: no per-chunk LLM call), auto-router (LLM picks chunker per document), and re-chunking backfill when `chunker.*` config changes. |
| Reactive journal GC unscheduled (ADR-0061) | `ReactiveJournal.Prune(minAcked)` (drops acked + TTL-expired records) is implemented and unit-tested but **not yet called on a timer** — the journal grows until a manual prune. A periodic compaction goroutine (bounded, driven off the lowest per-watch cursor) is the follow-up. |
| Reactive replay drop-on-overload (ADR-0061) | Startup replay re-enqueues onto the same bounded fast/slow channels; a replay backlog larger than the queue dead-letters the overflow (`queue_full`) rather than blocking. Fine for the expected small since-cursor backlog; a blocking/paced replay feed is a follow-up. |
| Operator contract skew (ADR-0061 → 0079) | Kernel now serves contract `0059` (ADR-0079 adds `AgentLLMExchangeOp` + conditional cap `llm-exchange` — the LLM-exchange provider tap for benchmark loop review, gated `execution.capture_llm_exchanges`; the benchmark harness stub is re-vendored to it). Prior baseline `0058` added `AgentStepOp` + cap `agent-steps` — per-memory_query agent-loop observability. Prior baseline was `0057` (accreted caps `watch-deadletter`/`reactive-backpressure`/`watch-condition-guard`/`watch-observability`/`watch-schedule` over REACT-01→06, + `route-preview` for the ROUTE-07 gatekeeper `PreviewRoute` RPC, ADR-0077, + `memory-ingest-binary` for the operator document-upload lane). **`ui/` re-vendored to `0057`** (2026-07-17). `cli/` still trails at `0047`, and the benchmark harness stub is at `0055` (needs re-vendor to call PreviewRoute). Recorded skew — the handshake degrades clients gracefully; the remaining batch re-vendor is a follow-up. |
| DB migration runner residuals (ADR-0064) | The runner is forward-only (no down-migrations, no checksums). `storage.auto_migrate=false` means boot runs only the dimension guard and creates no schema — the operator must `migrate up` first. `migrate` is not yet wired into CI (no committed CI exists). |
| Reactive injection-guard residuals (ADR-0063) | The registration risk gate is a coarse boolean (`Approved`), not a per-*fire* HITL nor a content-risk score. Per-fire approval via the `ApprovalController` and the "deterministic co-condition" alternative are follow-ups. Prompt-injection defense is structural (payload confined to a JSON-encoded nonce fence, fail-closed true/false) — strong against delimiter/tag forgery, but not a total guarantee against an adversarially-reasoning model. Contract `0053` not re-vendored to ui/cli. |
| Reactive backpressure residuals (ADR-0062) | Debounce coalescing buffers are ephemeral (a crash mid-window loses the buffered burst — only the coalesced fire is journaled). The global budget is a single plane-wide bucket, not per-owner/tenant — a noisy watch can spend the plane's whole allowance (per-owner quotas = REACT-07). Circadian/energy is the "natural home" for the budget but that machinery doesn't exist yet, so it is a premium-side token bucket for v1. Contract `0052` (+ cap `reactive-backpressure`, `debounce_seconds` field, `ReactiveBudgetOp` feed op) not re-vendored to ui/cli. |
| Experiential memory removed (2026-07-18) | All agent-EXECUTION write-back is unwired: step results (`RecordExecution`), plan scenes (`WritePlanScene`), procedural (`Hippocampus.Store`), negative edges (`IngestNegativeEdge`), episodic (`EpisodicExtractor` + `MemoryLifecycleManager`, both removed), and ADR-0034 scope promotion (`Promoter`). Tool-output recording was already inert (no caller). Document INGESTION (the corpus) is unchanged; agents still READ memory. Component implementations are retained but unwired, pending an experiential-memory redesign. |
| Retrieval context poisoning | Agent `memory_query` retrievals cross sessions — measured `cross_session_retrieval_rate` ~1.0 in the orchestration suite (fixtures/corpus are ingested per-session, so a query bleeds other tasks' evidence). Now measurable via `AgentStepOp` / the `agent_loops` summary. Fix is scope-isolated retrieval (ADR-0034), NOT a store reset (rule: noise is intentional). |
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

**Reactive / durable execution** (ADR-0032 + ADR-0061)

- **Signal**, the canonical event envelope (`domain.Signal{StreamID, FromAgent, Payload, RawText, Timestamp}`) a daemon or filesystem watcher emits and the `ReactiveEngine` evaluates against `WatchConfig`s.
- **Plugin / Registry / Lifecycle** (ADR-0074, extended by ADR-0082), the compile-time extension system: a `Plugin` (`Manifest()` + `Register(*Registry)`, plus an OPTIONAL `Build(KernelServices)`) declares contributions to curated extension points — **replace-one** (`SetSignalReceiver`, `SetResourceSelector`; conflict = startup error) or **add-many** (gRPC services, trace wrappers, lifecycles, capabilities). Three explicit phases: **Register** (declare — no kernel exists) → **Build** (construct runtime objects from the capability bundle) → **Lifecycle Start/Stop** (run). Tiered: mechanism is pluggable, the security kernel + auction integrity are NOT. Not Go `.so` (CGO/Windows/version-lock); compiled into a distribution binary.
- **ReactiveEngine**, the premium rule engine (`cambrian-premium/reactive/`) that fans a signal out to matching watches, evaluates each condition, and executes the action; it implements `domain.SignalReceiver`, wired as the first **plugin** (ADR-0074) via `Registry.SetSignalReceiver` + a `Lifecycle` (start/stop) + the ADR-0073 `ReactiveControl` gRPC plane.
- **Reactive journal**, the durable append-before-eval log (ADR-0061); one entry per signal, keyed by a monotonic seq, so a signal survives a crash between receipt and action.
- **Ack cursor**, a per-watch high-water mark of processed journal seqs; conservative (never advances past an unprocessed signal) so replay never skips one. The cursor is an optimization — idempotency is the correctness primitive.
- **Idempotency key / exactly-once**, `sha256(watch_id | signal_fingerprint | time-bucket)`; `MarkExecutedOnce` is an atomic bbolt check-and-set, so a redelivered signal executes its action exactly once, surviving restart.
- **Dead-letter**, a durable record of an action that failed or a signal that expired/dropped, with a reason; surfaced by the `OperatorConsole.ListWatchDeadLetters` read RPC ("what did my watch fail to do").
- **Debounce / coalescing** (ADR-0062), a per-watch window (`WatchConfig.DebounceSeconds`) that collapses a signal storm into one fire per T seconds, carrying the coalesced batch in the fired signal's `Payload["_batch"]` + `["_coalesced_count"]`.
- **Reactive budget / shed order** (ADR-0062), plane-wide hourly token buckets for `llm`-condition evaluations and `start_plan` actions; on exhaustion those (expensive) lanes are shed first — skipped, dead-lettered, and surfaced by a throttled `ReactiveBudgetEvent` — while deterministic/pattern conditions keep flowing.
- **Payload-as-data / condition guard** (ADR-0063), the `llm`-condition injection defenses: the evaluator prompt fences the untrusted signal payload in a nonce-delimited, JSON-encoded data block separate from the trusted operator condition; `WatchConfig.ConditionPayloadKeys` allowlists which payload keys reach the prompt; and `WatchConfig.Approved` is the required operator acknowledgement for a high-risk `llm`→`start_plan`/`dispatch_agent` watch (enforced at `RegisterWatch`).

**Memory and context**

- **VectorStore**, the `domain.VectorStore` port over pgvector (HNSW cosine, query-time `TemporalDecay`, three-set/CNF scope predicate over `metadata.tags`).
- **Document**, a single LTM row typed by `DocType*`: `MnemonicFact` (knowledge), `MnemonicAction` (events), `MnemonicScene` (conditions), `MnemonicEntity` (world-model things), `EpisodicMemory` (session narrative), `AgentProfile` (cognitive fingerprint), `ProceduralTemplate` (Hippocampus replay), `NegativeEdge` (failure memory), `Tool` (vectorised menu), `Skill` (vectorised menu).
- **activation_strength**, the `[0,1]` lifecycle metric per Document (0.1 default, +0.05/retrieval to 0.8); replaces `ImportanceScore`. Effective relevance at read = `cosine × (α + (1−α)·activation) × e^(−λ·age)`.
- **WorkspaceStage**, the pre-planning enrichment: SCENE+FACT fetch + pairwise contradiction guard (cosine>0.85 → `[CONFLICT]`) + cold-start fallback; the single LTM-to-Planner gate.
- **SpreadingEngine**, the GraphRAG BFS over `document_edges`, `0.75^depth` attenuation; the Graph Coverage Guard is the 3-band gate before BFS enrichment.
- **Conversation / Message** (ADR-0084 D1), the first-class chat entities. A `Conversation` is a durable, owned, ordered exchange (`OwnerID`, `Status` open|closed, `ConversationProfile`); a `Message` is one turn with a server-assigned monotonic `Seq` and an optional `ClientID` idempotency key. A Conversation is **not** a Session: a Session is goal-scoped work that completes, and a turn that orders work *references* one (1:N), never *is* one. Persisted by the kernel (`domain.ConversationStore`) so state survives a restart of whatever drives the chat.
- **ConversationProfile** (ADR-0084 D7), the recall + scope posture of a conversation: `operator` (recall, no narrowing), `employee` (recall, caller_scope-narrowed), `customer` (**no shared-memory recall** — tools only). Deliberately a property of the conversation rather than an agent default, so an external user cannot pull on tenant knowledge because someone flipped `seed_recall`.
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

**Scope and access (ADR-0034/0035/0085/0086/0087)**

- **Authorizer**, the port the kernel asks before it acts (`Authorize` / `Filter` / `ReadFilter` / `ClassifyWrite`). OSS ships `AllowAllAuthorizer`; the premium plugin supplies the real one.
- **TagSet**, an AUTHORED three-set term (`RequiredTags` / `AnyOfTags` / `ForbiddenTags`) the kernel carries without interpreting.
- **TagPredicate**, the COMPUTED, ready-to-apply form the kernel applies (CNF any-of clauses + bypass). The kernel applies a predicate; it never composes two — composition is the decision point's job.
- **AccessDecision**, the explainable outcome: reason, the SPECIFIC tag/clause/effect responsible, which policy linked where contributed it, and the policy version.
- **ToolEffect**, the closed verb set `read|write|egress|spend|admin`. A tag says what a resource is about; an effect says what you are doing to it (ADR-0086).
- **DefaultWriteTags**, the operator-set ceiling the decision point derives a write's classification from. A principal may only narrow, never broaden.
- **ScopeSystem**, the explicit kernel-internal bypass sentinel; one grep enumerates every use (INV-7). Never produced by composition.
- **Surface**, the entry point a request arrived through (operator / agent / chat / reactive). Established by the KERNEL from the transport and clamps what may be done regardless of identity — the loopback analogue (ADR-0085 D7).
- **PolicyObject / Link / Group**, the Group-Policy-shaped authoring model: policy is authored once and LINKED to a container, never assigned to a person. Precedence is organisation → group (outermost to innermost) → principal → surface, every fold an intersection (ADR-0087).
- **Report-only**, a policy evaluated and journalled but not enforced; the decision carries `would_have_denied` so the blast radius is visible before the switch is flipped.

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
