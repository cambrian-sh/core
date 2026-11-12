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
| `prompts/` | Prompt library (ADR-0139 C4/D2; spec `prompts/SPEC.md`): package `github.com/cambrian-sh/core/prompts`, `go:embed` of `<id>/v<N>.md` + sibling `*.schema.json`. `Get` / `GetVersion` / `Prompt.Render` (errors on missing or extra slots); `Rendered.StaticHash` is sha256 over the body with every slot as `{{}}`, i.e. the static text actually sent. `lint_test.go` is the core runner of the shared lint and writes `testdata/hash_cases.json`, the fixture the SDK loader is held to. Migrated so far: wave 2a (2026-11-05): `router`, `planner` / `planner_cap`, `replan` / `replan_cap` and `clarifier` (v2), each with its `<id>_input` (rendered by `internal/router/default_router.go`, `internal/awareness/planner.go`, `replan_handler.go`, `clarify.go`; loaded as a `prompts.Pair` (`prompts/pair.go`), whose `StaticHash` — sha256 over both files' static hashes — is the planner's `PlannerPromptVersion` and the router's registered hash). These send the C1 request (cached system block, tagged data with the task last, `ResponseSchema`; `CacheTTL` long, replan short) and decode a validated reply strictly (only a code fence is tolerated when `SchemaUnchecked`); `ExtractJSONObject`/`ParseThoughts` are gone from them and the planner's `<thought>` preamble is a schema `reasoning` field. `*awareness.Planner` and the replan budget wrapper forward `RequestGenerator`, so replans reach C1. Wave 2b (memory, 2026-11-05): `fact_extract`, `fact_assess`, `kg_triplets` (structured, flag `execution.ingestion.kg_triplets_llm_format`, default `completion`), `kg_triplets_completion` (the default KG²RAG prompt, byte-identical to the old inline one) and `thought_synthesis`; each organ call is `<id>` (stable, slot-less, one cached system block) + `<id>_input` (volatile user message with tagged data), sent through `domain.GenerateVia` (`domain/llm_request_via.go`) with the response schema; `prompts.NeutraliseClosingTags` escapes data. Not migrated but deleted (owner, 2026-11-05, `decisions/scenes-cancelled.md`): the tier-2 memory judge and the scene generator. Wave 2c (Python system agents, 2026-11-05): `retrieval_<op>` + `retrieval_<op>_input` and `retrieval_synthesis_guidance` (the one shared first system block of `synthesize` and `synthesize_cited`), rendered by `agents/system/retrieval_agent/planner.py`, and `chat_agent` + `chat_agent_turn` (fragment sets) rendered by `agents/chat_agent.py`; both load this directory from disk with the SDK's `PromptLibrary.load` (`$CAMBRIAN_PROMPTS_DIR`, else the nearest ancestor `prompts/`), so the image copies it to `/app/prompts` and `setup` unpacks `prompts.Files` to `<prefix>/prompts` (`app/setup_tools.go` `unpackPrompts`). |
| `internal/metabolism/` | AgentManager, Gatekeeper, dispatch, DAG executor, A2A connector, `registration/` (ProfileSeeder: registration-time profile seeding, a deterministic description vector + neutral prior, no LLM; and the Layer-2 ProfileSearcher), `backfill/` (boot profile backfill), `yield/` (consumer of agent-yielded sub-goals: decorates the Dispatcher as `MetabolismStack.Selector`, binds the sub-goal through dispatch, resumes the yielding agent; ADR-0041 D5 / ADR-0100 D10). |
| `internal/memory/` | LTM: pgvector, chunking pipeline, structure-aware ingestion, document entities, KG²RAG. |
| `internal/evidence/` | Knowledge substrate evidence write path (ADR-0105). |
| `internal/infrastructure/postgres/query_plane*.go` | The knowledge substrate's ONE fenced read path (ADR-0111/0118/0128). `PgQueryPlane` is built once in `app/app.go` (`queryPlane`) and serves: the closed AST (`KernelServices.QueryKnowledge`); the names-and-counts catalog `domain.SubstrateCatalog` (`Catalog(scope)`: overview counts, entities, predicates, event types, item kinds, actors; `KernelServices.KnowledgeCatalog` via `authz.KnowledgeCatalogFunc`, principal in); and the link review inbox (`ReviewCandidates`, operator `ListLinkCandidates` at `ScopeSystem`). Every read applies the owner fence under plain Bypass through `evidenceScopeSQLOn`/`evidenceRowConds`/`textArrayScopeSQL`/`filterLinks`. `point` returns the latest VISIBLE row; the alias closure walks only readable links, so ClosureSize and the guards never count hidden aliases (2026-11-07, T-20261107-substratelane-pointlookup-history, T-20261107-ask-knowledge-catalog-reads-entity-ids-and, T-20261107-link-review-inbox-and-entity-op-candidates-skip). Cross-user Pg test: `fence_substrate_reads_pg_test.go`. Open: ConfirmLink/RetractLink by an unreadable id (T-20261107-operator-confirmlink-retractlink-act-on-and). |
| `internal/substrate/` | gRPC server, session, OperatorConsole plane, knowledge-graph extractor. |
| `internal/supervision/` | Gatekeeper, verifier pool, watcher (gates SignalStream; in OSS plans each accepted signal once and queues the plan for operator approval on the addressed approval hub (class `proactive_plan`, operator rule), running it via `domain.PlanExecutor` only if approved). |
| `internal/authz/` | Access-control enforcement (ADR-0085). Fail-closed; contains no policy. **Owner fence (2026-11-06, ADR-0141 position 3):** `domain.TagPredicate` admits an `owner:<X>` row (personal ingest, and facts derived from it) only when X is in `pred.Owners` — in `Check`/`CheckRow` and every SQL mirror (`scopeExpressionsOn` jsonb, `textArrayScopeSQL`/`evidenceScopeSQL`, `ListDocuments`) — and the fence survives `Bypass`: `ScopeSystem` (operator plane, kernel enrichment) is fenced, only `domain.ScopeMaintenance` (fact backfill/journal reload) reads every owner's rows and `domain.ScopeSystemFor(owners…)` exactly those owners'. Bypass shortcuts ask `Unfiltered()`. `RetagDocument` and the `SaveDocument` upsert never drop an owner tag (`domain/owner_scope.go`). |
| `internal/infrastructure/` | Adapters: `postgres/`, `llm/`, `mcp/` (outbound), `mcpserve/` (inbound MCP endpoint). |
| `internal/reactive/` | Reactive rule engine plug surface (premium provides the implementation). |
| `internal/chat/` | OSS turn path: pool-dispatched, retry-safe, planner-free. ADR-0145 phase 3 chat side (`grounding.go`, `transcript.go`, `summary.go`, `turnindex.go`, `turnstate.go`, `replyguard.go`; every key **default off**): token-budgeted transcript + kernel-triggered rolling summary, the turn index, ask-back state and the D8 reply check, turn state kept as `computed` run-ledger entries (`chat_turn_index`, `chat_conversation_summary`, `chat_reply_check`). |
| `internal/replycheck/` | ADR-0145 D8 reply check (2026-11-08, T-20261108-chat-replies-state-figures-dates-and-ids-no): pure and deterministic, no model call. `Extract` (money with currency either side and k/M/bn/milyon/milyar/bin scales, percents incl. `%12,5`, numbers with every EN/TR reading, ISO/dotted/textual EN+TR dates, letter-led ids; list positions, times and bare integers below a minimum skipped), `Check` (values backed by tool-result/computed/artifact entries, accepted item values, the person's words and today — never by a step output; rounding by written precision, so `£1.4M` is backed by 1,412,345.67; `[led_…]`/`[R#]` markers give miscitations and gap tags; every non-constraint checklist item answered or stated as a gap), `StripMarkers`. Fixtures: the recorded o3x and citizen-deadline replies (`testdata/recorded_replies.json`). Recalled memory (2026-11-09, T-20261109-chat-agent-answers-a-published-rule): a `memory_recall` ledger entry backs the values it holds, and `Report.MissingSource` (counted as a violation) lists a value ONLY recalled memory backs whose source the reply does not name — the chunk is split at its numbered headings and any label of the block holding the value counts (document title, section-path segment, the heading, its designator such as `MADDE 4`, a title of two or more words); enforce mode's retry and final note (`MsgReplySource`) name the source. |
| `internal/storage/` | BBolt adapter + durable reactive journal (ADR-0061). Since A2 (2026-11-03) the journal is `reactive_journal_v2/<stream>/<seq>` (16-byte header + signal JSON; a legacy flat `reactive_journal` is migrated on open), pruned per stream, with idempotency pruning and offline compaction before open (`bbolt_compact.go`; `storage.compact_min_bytes` / `storage.compact_free_ratio`; `cmd/bbolt-compact`). Gate: `bbolt_reactive_gate_test.go` (`CAMBRIAN_JOURNAL_GATE=1`), benchmark `reactive-journal-a2-gate`. |
| `internal/config/` | Koanf 12-layer loader (ADR-0024). `LoadConfigWithStore` = `loadMerged` (layers + per-layer v1→v2 migration) then `Config.Validate` ONCE: one aggregated `ConfigError` naming each key by full path, covering the secrets/provider/endpoint/MCP blocks, `ChunkerConfig.Validate` and `ExecutionConfig.Validate` (the last two had no production caller until 2026-11-06, T-20261106-executionconfig-validate-has-no-production). `ValidateOverride` re-runs the same rules with a pending store value overlaid; `configWriteSource.setOne` rejects a console write that would introduce a violation. |
| `internal/memorynotes/` | ADR-0139 D14 principal memory notes: one markdown document per (`personal`, principal) and (`org`, org id), sections = `## ` headings. `Service` (the only read/write path for both planes): `Snapshot` (frozen per run key = lease RunID else lease, bounded in-process cache), `Propose` (append / replace_section / remove_section, etag CAS), operator `List/Get/Set/Delete/SetBudget`. Budget in code points (config default < scope-default row < document row), over budget refused never trimmed; lowered budget serves the snapshot cut with `truncated`. Injection scan = `internal/contentscan` (any non-clean verdict refuses; operator override audited). C8 = owner rule + `domain.Authorizer` (resource `memory:memory-notes:<scope>:<owner>`); unreadable == NOT_FOUND. A personal document's tags carry `owner:<owner>` (ADR-0142 D12, 2026-11-07), so only its owner reads it (operator `Get/List` of another person's notes is fenced; `Set/Delete/SetBudget` stay authorized on classification tags as audited admin acts). Act-for turns carry the acting operator on the lease (`LeaseBinding.ActedBy`, from `domain.ActForActorFromContext`, audit only), and the writer of record reads `agent:<a> for <owner> (on behalf, by operator:<op>)`. D12 children (`LeaseBinding.ParentLeaseID`) are SCOPE_DENIED unless `MemoryWriteGranted`. Stores: `PgStore` (migration 0030: `memory_notes`, `memory_notes_budgets`, `memory_notes_history`; deletes keep the row so revisions never restart) and `MemStore` (no-DB fallback). RPCs: `network/memory_notes_rpc.go`, `operator/memory_notes.go`; composed in `app/memory_notes.go` (run principal = conversation owner → task beneficiary → `execution.memory_notes.default_principal`). Capability `memory-notes`, contract 0101. |
| `internal/substrate/network/request_guards.go` + `domain/request_guard.go` + `domain/request_guard_exec.go` | ADR-0145 D14 request-tied guards (2026-11-09, `execution.tools.{request_guards, write_targets, constraint_classes}`, **default off**; T-20261109-adr-0145-d14-fix-batch-remove-per-step). **DISABLED pending a redesign (owner 2026-11-09, ADR-0145 D14 item 7): off in every run configuration (the use-case suite pins and refuses it, replays drop it); the code stays; identical write retries are still answered by `idempotent_writes` (`app/evidence_pipeline_test.go`). `write_targets` is read regardless (2026-11-12): `ToolExecutor.WriteTargets` stamps every action-ledger write's target (`PlanAction.Target`), and coverage satisfies a write/document item only by a done call to one of the item's DECLARED targets (`internal/requirements/coverage.go` `targetActions`; T-20261112-coverage-settles-a-write-item-on-any-done, T-20261112-write-target-coverage-reads-title-pdf-from-a); a move/rename/copy (`*move*|*rename*|*copy*`) records its destination (`destination|dest|new_path|…`), never its source.** Replace the removed per-step effect menu. At dispatch the server binds a `domain.StepGuard` to every plan-step lease (`LeaseBinding.Guard`: request scope `turn:<conv>/<turn>` else `run:<root run>`, the checklist's constraints resolved to ADR-0086 tool classes, the OPEN write/document items the step answers, the repair-turn flag); `ExecuteTool` puts it on ctx (`StepCall.Guard`) and `ToolExecutor.requestGuards` applies, after authorisation and before hooks/approval: a constraint refuses every tool of its classes (`forbids: send` ⇒ every egress tool, the refusal quotes the person); in a repair turn a side-effecting call is refused once the step executed a write; a side-effecting call whose target (`WriteTargets.Key`: first matching rule's argument fields, else the default keys; e-mail values reduced to addresses) the request already wrote or is writing is REFUSED with the earlier result ids, whatever the wording (`WriteRecord`, in memory, per request); a side-effecting call on a step answering no open write/document item goes to approval with the notice "This write was not part of the request" (the approval item's existing `duplicate_notice` field; no proto change). `RefusalRecord`: an identical (tool, canonical args) call a step already had refused is answered with the same refusal and a note, without checks, approval or run. The validator gets the written targets (`coverage.written_target`). spawn_subagent (`network/subagent.go`, `SubagentService`, the gateway lease gate, `RollUpLease`, child leases, `subagent_*` config and hook points) was REMOVED the same day; the `SpawnSubagent` RPC is DEPRECATED in `cambrian.proto` and answers UNIMPLEMENTED. A worker hands extra work to the planner through Execute with its step lease (`worker_agent.py` `delegate_to_planner`); `Server.Execute` runs such a nested plan one level deeper (`LeaseBinding.DelegationDepth`, refused past `execution.plan.max_worker_delegation_depth`, default 1) on dedicated agent instances. **D14.6 refinements (2026-11-09, T-20261109-adr-0145-second-gate-fix-batch-constraint-scope):** `StepGuard.WriteItems` holds open WRITE items only, `StepGuard.Documents` the open document items (a side-effecting call on a step answering documents and no write item is REFUSED, `DocumentWriteRefusal`: a document is finished inside Cambrian, its text is the step's answer and Coverage accepts it from `answer_text`); `StepGuard.Carries` = the write class plus the classes of the write items' `RequirementItem.Does` terms — `ForbiddenBy` never refuses a carried class on that step and `PlanAction.Carries` keeps `Coverage.ObserveActions` from failing the constraint for the request's own write; `requirement_checklist` v2 (`does`, "exactly as drafted" forbids `edit`, outside saves are writes; term `edit`→write added to the defaults). Worker delegation: `domain.ActiveDelegations` (`Server.WorkerDelegations`, shared with `Dispatcher.Delegations`, wired in `app/composed_workers.go`) — while a step's worker waits on its own delegate_to_planner the dispatcher's step deadline re-arms (a full step timeout after it returns) and a re-dispatch of that lease is refused (`ErrStepDelegating`); a re-dispatch on a composed lease runs on the runtime under the existing composition (never a pre-built agent); the nested run carries `domain.WithParentRun`. D6: `admitRequestEvidence` admits cited entries of earlier stages/generations, the delegating run or the same turn (read through the enforced reader); `requirements.resolveRecordIDs` turns a non-ledger ref/evidence_ref into the ONE step entry containing it (else the item stays open listing the citable ids); the repair turn's `_repair_prior_calls` is rendered by the SDK (react-turn v8). |
| `internal/stepoutput/` | The live step-output stream (ADR-0139 amendment 2026-11-06, superseding ADR-0047 D12's token tier; T-20261106-native-react-path-emits-no-live-token-events). `Stream`: ONE stream per step keyed by the lease; producers `EmitGateway` (both model lanes, via `network.gatewayForwarder`: text deltas + the final-answer argument decoded by `Extractor`) and `EmitAgent` (agent-plane `EmitStepOutput`: lease-bound, seq dedupe, `final`); kernel `seq` per lease; caps `execution.step_output.max_chunk_bytes`/`max_step_bytes`; sinks fixed at construction (the operator feed, `TokenChunkEvent`, attached late via `app/step_output_wiring.go`) and per-session subscribers (MCP `notifications/progress`). Constructor-injected policy: `streams_output` agents and any `post_model` hook whose static scope MATCHES the step (`HookChain.HasFor(post_model, agent, "agent_step")`, header and lease-bound agent) suppress gateway deltas (fail-closed; an unscoped hook matches every step, a planner-only one none). No goroutines. The native lane streams internally through `llm.Provider.GenerateOnObserved` / `SubstrateLLMGateway.GenerateWithToolsObserved` (RPC stays unary); adapters emit `domain.ToolCallDelta` (Anthropic, OpenAI-compatible; Gemini none). `Server.TokenSink` is gone. Capability `step-output-stream`, contract still 0101. |
| `internal/hooks/` | ADR-0139 D11 lifecycle hooks built from `execution.hooks`: built-ins `deny_args_regex`, `require_approval`, `redact_output`, and the `command` hook (argv, JSON event on stdin / JSON `HookResult` on stdout, ctx timeout kills it, `procenv.Allowlisted` env). `NewChain` merges them with plugin hooks (`app.Registry.AddHook`). The port and runner are `domain/hooks.go` + `domain/hook_chain.go`. |
| `internal/migrate/` | Pure-Go DB migration runner (ADR-0064). Boot goes through `migrate.Boot` with the `database.migrations` guard (2026-11-04): `mode` `auto` (default) / `refuse_pending` / `off`, `protected_dbnames` (default empty; a listed database with any pending migration or concurrent index refuses to boot, read-only, before any write, naming the pending changes), `allow_pending` (one-boot opt-in, `CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true`). `migrate up` is the unguarded deliberate path. Applied migrations and index builds log at WARN with the database name. |
| `internal/agentplane/` | Agent-plane gRPC connection pool (renamed from auctioneer by ADR-0100 P3). |
| `internal/agentpool/` | Bounded pool of interchangeable daemon workers (ADR-0084 D4). |
| `internal/tool/` | System-tool lifecycle (Python, `ToolRegistry`, confined `ProcessHandler`). |
| `internal/skill/discovery/` + `skills/` | System skills (ADR-0046, format ADR-0139 C3): `skills/<name>/SKILL.md` discovered from the cwd-relative `skills/` dir at boot, indexed `DocTypeSkill`, served by `ListSkills`. Ships `file-report` (2026-11-04) and, with agent stack v2 (2026-11-05), `summarise`, `web-research`, `exploratory-analysis` and `chart-report` (the starter profiles' skills), plus `organise-files` (2026-11-06, assistant); `skills/README.md` covers the frontmatter and how a deployment points at the dir. The Docker image does not copy it yet (T-20261104-docker-image-does-not-ship-cambrian-core-skills). |
| `internal/skill/lifecycle/` | ADR-0139 D13 skill lifecycle (2026-11-05). `Service`: `Propose` (agent plane; never active until approved; trust stamped `agent-proposed`; id `sp-<hash(name, base_version, content)>` so an identical re-proposal is the same row; refusals INVALID_FORMAT / NAME_TAKEN / LOCKED / SCAN_DANGEROUS / QUOTA / DENIED; dangerous proposals are not stored), `Decide` / `SetLock` / `Rollback` (operator; idempotent on command_id at the ledger; stale `base_version` and dangerous content are FAILED_PRECONDITION; rollback restores content+version+state, keeps the lock, and is a new entry), `RecordUse` / `RecordRunOutcome` (use_skill Tier-2 loads in `network/skills_rpc.go`; success credited once per run after `DAGExecutor.ExecuteFrom`), `Curate` + `Curator` worker (deterministic, no LLM: active →stale →archived on idleness; pinned exempt; default manages `agent-proposed` only). Approved agent skills live in Postgres and are materialised into the SAME `InMemorySkillRegistry` + `DocTypeSkill` index file discovery fills (`SyncBuiltins`, `Materialize`) — never written into `skills/`. Shipped skills get a `builtin` record (version, usage, ledger) but are served from disk and locked to agents. Store: `postgres.PgSkillLifecycleStore` (migration 0029: `skill_proposals`, `skill_versions`, `skill_records` — not `skills`, which is 0007's descriptor index — `skill_ledger` append-only by trigger, `skill_run_uses`). RPCs: `network/skills_lifecycle_rpc.go` (ProposeSkill; source run/agent from the lease, principal from the D14 resolver), `operator/skills_lifecycle.go` + ListSkills lifecycle fields; composed in `app/skill_lifecycle_wiring.go`. Config `execution.skills`. Capability `skills-lifecycle`, contract 0101. |
| `internal/skill/access/` | Per-agent skill access (owner decision 2026-11-05, `decisions/skill-access-grants.md`; amends ADR-0139 D13). A PROFILE agent's effective skill set = profile `skills` + durable grants keyed by agent id; `network/skills_rpc.go` `ListSkills` enforces it (no body outside the set, fail closed; full menu = the set; find_skills shows other visible skills as `requestable`, Tier-1). `Service.Request`: use_skill outside the set raises an `ApprovalRequest{Kind: skill_access}` on the ONE addressed hub (operator rule) like dangerous tools (`toolApproval`, never `toolApprovalCtrl`), waits `execution.skills.access_request_timeout_seconds`; approved ⇒ grant + body in the same call, no ruling (`ApprovalDecision.Decided=false`) ⇒ pending (request row stays open), denied ⇒ denied. `access_auto_approve` grants without asking. `GrantProposer` is the lifecycle's `ProposalGranter` (approve ⇒ proposer grant). Store `postgres.PgSkillGrantStore` (migration 0031: `skill_grants`, `skill_access_requests`, append-only `skill_grant_events`). Operator: `operator/skill_grants.go` ListSkillGrants / GrantSkill / RevokeSkillGrant, capability `skill-grants`. Wired in `app/skill_access_wiring.go`. Non-profile agents are unbounded (mirrors `ProfileToolCeiling`). |
| `internal/scheduledtask/` | Principal-owned recurring tasks, the kernel GATE (2026-11-06, T-20261106-the-assistant-cannot-set-up-a-recurring-task). `Service.Propose`: owner = the lease's run principal (resolved by `network/scheduled_tasks_rpc.go`, never a request field), validate (goal ≤ 4000 chars, exactly one of cron / RRULE subset → cron via `domain.RRuleToCron`, delivery conversation/notify/email), caps through the store's `Check` BEFORE asking, then `ApprovalController.Request` kind `schedule` (tool `schedule_task`, args = `for <owner>: ...`; the console Inbox), create ONLY on an explicit approve (denied ⇒ DENIED, no answer within `execution.schedules.approval_timeout_seconds` ⇒ NO_DECISION). `List/SetActive/Delete` are owner-scoped. The store is the port `domain.PrincipalSchedules` (premium `reactive.PrincipalSchedules`, schedule watches with `WatchConfig.Owner` + action `scheduled_turn`); a run executes through `KernelServices.RunOwnedTurn` (`app/scheduled_tasks.go`: conversation `sched:<id>` created owned by the principal, refused under anyone else). Agent plane `ProposeScheduledTask` / `ListScheduledTasks` / `ManageScheduledTask`; operator `ListScheduledTasks` (operator-only read; pause/delete reuse `SetWatchActive`/`DeleteWatch`). Config `execution.schedules`. Capability `scheduled-tasks`, contract 0101. |
| `internal/knowledgecommit/` + `domain/kinds.go` (case fields) | Knowledge-substrate case modelling (2026-11-07, T-20261107-knowledge-substrate-extensions-for-case). KIND DATA: `ValueSpec.Enum`; `KindSpec.Lifecycle` (state predicate, initial, transitions, required — checked by `PgKnowledgeStore.PutItem` against the current resolution INSIDE the advisory lock); `ResolutionScope=entity` (a cross-actor resolutions row, actor `*`, under the kind policy, plus per-actor latest_assertion rows; entity lock); the built-in `designated_actor` authority over `Designated` {principals, sources, approved_via_commit}; `EventTypes` validated at `PgEventStore.RecordEvent`; `CommitClass`. All boot-validated by `NewKindRegistry`; a kind using none of them resolves byte-identically (golden `TestPg_CaseExtensions_DefaultKindResolutionsUnchanged`). BRIDGE: `knowledgecommit.Service` (built in `app.go` over the RAW store, which it must be: it is the kernel path that stamps actors) — `Propose` writes a CANDIDATE asserted by the lease's agent (designated_actor kinds only; an eligible proposer is refused), `RequestCommit` raises an async `knowledge_commit` approval (Object = candidate id; Args are not persisted), and `Apply` (the hub handler, registered at boot) writes the committed item asserted by `ApproverActor(ResolvedBy)` with source ref `knowledge_commit:<approval id>`; deny/expiry write nothing, standing approvals are refused. `ProposeLink` writes a `scored` candidate link. Served as agent-plane `Orchestrator/ProposeKnowledge` (`internal/substrate/network/knowledge_propose_rpc.go`; child leases need the memory-write grant). `authz.DesignatedActorGuard` wraps `KernelServices.Knowledge` (inside the reserved-kind guard) and refuses a plugin item asserted by a designated principal or carrying the commit marker. E2E: `TestPg_DecisionCaseAsData_EndToEnd` (a decision kind from JSON, no decision-specific Go). |
| `internal/ledger/` + `internal/kernel/ledger_stack.go` | ADR-0145 D1 run ledger (2026-11-08, `execution.ledger.{enabled,retention_hours,max_entry_bytes,preview_chars,sweep_interval_minutes}`; its page size is `execution.content.page_bytes` since 2026-11-12, **default off**; T-20261108-build-the-run-ledger-adr-0145-d1-every-tool). Port `domain/ledger.go` (entry: deterministic id `led_<sha256(conversation,turn,run,step,call key,content hash)>`, kind `tool_result|step_output|computed|artifact|scratch`, provenance, owner, kernel tags, body, preview). `kernel.NewLedgerStack` is the ONE composition point: raw `postgres.PgLedgerStore` (migration 0034 `ledger_entries` metadata + bodies in the artifact vault byte store) → `ledger.Recorder` (writer: `authz.ClassifyArtifactWrite` with the tool's `AuthzTags` as narrowing hint, D12 owner stamp, `max_entry_bytes` cap keeping the head + size + content hash; an unclassifiable entry is not written) and `authz.EnforcingLedger` (the only reader: scope from ctx, `ErrScopeMissing` without one, forbidden = absent, owner fence under bypass). Writes at two choke points: `domain.ToolExecutor.Ledger` (every result that ran, read/write/handler error, after post_tool hooks; not denials, not operator ScopeSystem calls; promoted tool files as `artifact`) and `DAGExecutor.Ledger` (`executer/ledger_steps.go`, every step output). Memory recalls are ledger entries too (2026-11-09, `network/recall_ledger.go`): every `QueryMemory` by a caller holding a kernel-issued lease (the SDK seed recall of a chat turn or step, every memory_query) records ONE `tool_result` of pseudo-tool `domain.RecallToolName` (`memory_recall`) under the lease's provenance, body `{query, hits:[{text, document, section, id}]}`, each recalled chunk's own tags carried as `LedgerRecord.SourceClassifications` (→ `SourceTags`, so never wider than any chunk); each hit's metadata gets `_ledger_ref`, `_source_document`, `_section_path` (the SDK renders them as `ledger=`, `document=`, `section=`). Kernel-served tools `ledger_get(id,path,rows,page,find)` (every page answer carries `total_chars` and a `note`: "N chars total, showing a–b (page k of P); next: …; search: …"; `find` returns capped snippets with their pages — the ONE bounded read, which the SDK's `resolve_cid` uses for `led_` ids since 2026-11-12) / `ledger_find(scope run|turn|conversation, step, tool, kind, text)` (`domain.KernelTool`, run in `ToolExecutor` under the caller's ReadFilter predicate, no grant, appended to every `ListTools` menu; scope resolved from the caller's lease, never an argument). `ledger.Sweeper` purges expired rows (and vault bytes no row or artifact names) and deletes a turn's scratch via the `TurnDelegations` turn-end hook. Wired in `app/ledger.go`. Postgres tests `postgres/ledger_store_pg_test.go` (PG_TEST_DSN). |
| `internal/requirements/` + `internal/planvalidate/` + `internal/toolargs/` | ADR-0145 phase 2 (2026-11-08, T-20261108-adr-0145-phase-2-requirement-checklist-with; every flag **default off**). `requirements.Deriver`: the D5 checklist, ONE model call (prompts `requirement_checklist` v1 + `requirement_checklist_input` v1, schema `checklist.schema.json`) over the user's own words (`domain.UserRequestText`: the `<user_request>` region a chat delegation carries, else the request); items `answer|figure|document|write|constraint` with a quote code checks verbatim (`domain.QuoteAnchored`), one repair call for unanchored quotes, then dropped; nothing left = the whole request as one item. `requirement_checklist` v3 (2026-11-12, schema `checklist_v3.schema.json`; owner decision 'explicit targets + expects'): write/document items declare `targets` `[{kind: file|folder|recipient|record, value, match: exact|glob}]` (`anchorTargets`: an exact value must be verbatim in the request, a glob must compile and have a wildcard, else dropped and logged; a template such as "YYYY-MM-DD Title.pdf" is a glob, never a name), answer/figure items declare `expects` from the envelope value vocabulary (`domain.ValueTypes` or `any`; none: an answer takes any type, a figure number/money/date). Coverage matches calls' write targets against the declared targets (exact: equality/path-suffix/address, a folder also by what is written inside it; glob: `path.Match` over lower-cased `/`-separated paths, a relative pattern against the trailing segments); no declared target = any done write of the step; item text is never parsed for targets (`itemTargetTokens` deleted). A value counts as stage progress only once the judge accepted its result (`ItemValue.Refused`). `requirements.ValidateEnvelope`: the D6 checks of a step's answer envelope (types, evidence only from the step's own tool calls or its inputs incl. upstream steps' tool results, figures never from a step output, ids/numbers/dates must appear in the cited entry). `requirements.Coverage`: item statuses `open → satisfied/not_found/not_permitted/failed`, changed only here (per-type rules, not_found only with real searches and a second attempt after errored ones, not_permitted only with a kernel refusal, conflicts keep an item open, for_each = one accepted result per fan-out member, and a write item answered by ONE ordinary step over a fan-out set (`StepObservation.SetMembers`, from the fan-out children it depends on) needs a done write naming each member once any write names one (`coverMembersByWrites`; T-20261109-plan-identifies-4-october-receipts-but-the-move), a joined `evidence_ref` (`led_a, led_b`) of the step's own entries is split deterministically, constraints fail on a forbidden done/attempted call; `EndStage` progress fingerprint, `CloseGaps`). `planvalidate.Validate`: the D7 rules (structure/bindings/fan-out/for_each, coverage, constraints, effects incl. write-after-read and one write per item, dates from today/request/earlier results), structured violations. `toolargs.Validator`: D9 argument coercion + JSON-Schema validation at dispatch (anyOf/oneOf branches too: an optional `list[str] | None` sent as a JSON string). The composed runtime's per-step tool budget is `execution.workers.max_tool_rounds` (env `CAMBRIAN_COMPOSED_MAX_TOOL_ROUNDS`, 0 = the SDK's 12). `tools/code_sandbox/server.py`: llm-sandbox's MCP server plus `SANDBOX_MOUNTS` (read-only bind mounts of the person's files, Python starts there; opt-in, configs/MCP.md); `configs/mcp.json.example` servers carry `catalog_id`. Wired in `app/evidence_pipeline.go`. |
| `internal/metabolism/executer/envelope_steps.go` + `internal/substrate/network/coverage_stages.go` | The executor and server halves of phase 2. Executor (`DAGExecutor.Requirements`): stamps `_answer_items` / `_step_inputs` on steps that answer items, validates the envelope with ONE repair turn (`_envelope_violations`), feeds `Coverage`, registers fan-out members, never serves such a step from the step cache. Server: derives the checklist before planning (`Server.Checklist`), records it and the final coverage as `computed` ledger entries, `planChecked` (D7 validator with repair rounds, else legacy `planWithValidation`), and the coverage-stage loop after the first plan: settled ⇒ done, no progress ⇒ stall ⇒ gaps, else the next stage (own run id `<run>-s<k>`, planner told the open items via `domain.CoverageStage`) up to the runaway cap. `ExecuteResult.Requirements` carries the checklist + coverage; the text payload ends with `REQUIREMENT COVERAGE`. |
| `internal/stagedinputs/` + `domain/staged_inputs.go` + `app/staged_inputs.go` + `tools/code_sandbox/` | Code sandbox per-run input folder (owner decision 2026-11-10, option C, `cambrian-knowledge/decisions/code-sandbox-files-per-run-mount.md`; T-20261110-code-sandbox-cannot-see-the-person-s-files-in; `execution.tools.staged_inputs`, **default on**: it acts only on a tool whose MCP policy says `staged_inputs`). A tool marked `staged_inputs` (catalog/`mcp.json.example`: code-sandbox `execute_code`; `MCPToolConfig.StagedInputs` → `mcp.ToolPolicy` → `SystemTool.StagedInputs`) takes `inputs` (ledger ids / the person's paths). `ToolExecutor.Execute`, after grant/scope/effects/request guards and BEFORE approval, hands them to `domain.ToolInputStager` with the caller's read predicate on ctx (`callerReadScope`, shared with kernel tools): a `led_` id is read through the run ledger's ENFORCED reader (forbidden = absent ⇒ the call is refused, nothing staged; a truncated entry is refused), a path through `path_reader` (default `mcp:filesystem/read_text_file`; `binary_path_reader` `read_media_file` for `binary_extensions`) via `ToolExecutor.Execute` as the same caller (grant, scope, per-user server, ledger record). Exactly those files go into a fresh `<root>/run-*/inputs` (+ empty `outputs`), sent to the server ONLY as request `_meta` (`cambrian/inputs_dir`, `cambrian/outputs_dir`, `cambrian/inputs` name map; `ToolCall.Meta`, never model args). `tools/code_sandbox/server.py` mounts inputs read-only at `/inputs` (Python starts there, `INPUTS` dict) and outputs rw at `/outputs` for that one container; `inputs` without the kernel's `_meta` is refused. After the call every output file becomes an `artifact` ledger entry `DerivedFrom` the inputs (as does the call's `tool_result`), the result gains a note naming the ids, and the folder is deleted (also on refusal/error). Ledger off: `led_` inputs refused, outputs named but unrecorded. Caps: 20 inputs / 64 MiB, 20 outputs / 32 MiB each. The server also runs Python on `SANDBOX_IMAGE` (`cambrian/code-sandbox:py311-1`, `tools/code_sandbox/image/Dockerfile`, `make sandbox-image`: llm-sandbox's python-3.11 image by digest + pandas 2.2.3, numpy 2.2.6, matplotlib 3.10.3, openpyxl 3.1.5; owner option a, T-20261107-code-sandbox-cannot-serve-as-a-compute-engine) with no venv/pip setup, refuses a library the image's `cambrian.sandbox.preinstalled` label lacks, errors (never pulls) on a missing image, and gives each call scratch `/sandbox` + `/tmp` binds so `SANDBOX_READ_ONLY=true` works (upstream failed EVERY call under it: "container rootfs is marked read-only"). The catalog launches it with `uv run --directory ${CAMBRIAN_KERNEL_DIR} … python tools/code_sandbox/server.py` (`mcp.KernelLaunchValue`: kernel-provided, absolute `config.ResolveBaseDir`). `SANDBOX_MOUNTS` (fixed ro mount) is tests/local development only. Gap: a docker-runner MCP server cannot see host staging paths. |
| `internal/calc/` + `app/calc_tools.go` | ADR-0145 D4 calculation tools (2026-11-08, `execution.tools.calc_tools`, **default off**; T-20261108-adr-0145-d4-kernel-calculation-tools-working). Kernel tools (`domain.KernelTool`, beside `ledger_get`/`ledger_find`, always on every menu and always allowed by the effect menu): `calc_date_window` (named windows from the kernel `domain.Calendar`), `calc_date_add` (calendar units or working days), `calc_working_days` (with the organisation's holiday calendar: `organisation.holidays` YYYY-MM-DD or recurring MM-DD, `organisation.weekend`, default Sat+Sun; `calc.WorkCalendar`), `calc_aggregate` (sum/count/min/max/avg/list, group_by, where filters over a list inside a ledger entry by reference: `ref` + JSON `path`, a JSON string is walked into; money strings parsed; identifiers never read as numbers) and `calc_compare` (thresholds and rules, sides literal or `{ref,path}`). References are read through the ENFORCED ledger reader under the caller's predicate; every result is recorded as a `computed` ledger entry `{tool, inputs, input_refs, result}` derived from the referenced entries, and its id returned as `ledger_ref` for a figure to cite. Without the ledger the tools compute but neither read references nor record. `ledger.SelectPath` is shared with `ledger_get`. |
| `internal/substrate/network/staged_planner.go` + `internal/awareness/staged_block.go` | ADR-0145 D11 staged planner + D5a ask-back (2026-11-08, `execution.plan.{staged_planner, stage_ledger_read_rounds=25, stage_ledger_index_entries=200, ask_back_enabled, ask_back_max_rounds=5, ask_back_other_paths=proceed|report}`, **default off**, needs `checklist_enabled`; T-20261108-adr-0145-d11-staged-planner-returns-next-stage). Every planner call (`planner_cap` v11 + `planner_input` v4, schema `plan_cap_v6`; v11, 2026-11-12: ONE input reference grammar `s<N>|e<K>[.<name>][[*]]` (`domain.InputRef`), where `e<K>` labels an earlier stage's completed step (`requestRequirements.addEarlier`, resolved by `StepRequirements.ArchiveStage`/`resolveInputs`; the validator lists the valid references and the closest on rejection; T-20261112-staged-planner-binds-earlier-stage-results-with) gets `domain.StagePlanning`: stage number, whether the person can be asked, a compact ledger index of the request (turn entries, else every stage run's; caller's read predicate, or the run owner's scope for a caller with no agent id), the earlier stages' step refs; after stage 1 no LTM/prior plan (fresh context). Reply `outcome`: `stage` (steps), `read` (`ledger_reads` served in-kernel by `ledger_get`/`ledger_find` only, results in `<ledger_reads>`, bounded rounds), `input_required` (`questions` -> `domain.InputRequiredError`) or `nothing_more` (`domain.ErrNoFurtherStage`: open items close as gaps). The planner never declares done; thought steps are retired (forced to agent steps); the validator's staged rules require each stage to answer at least one open item and no settled one. The validator also enforces the D13 decomposition rule with a checklist (`decomposition.split_judgement` J1, `fan_out_judgement` J2, `partial_set_inputs` J3; T-20261108-plan-validator-enforce-the-d13-decomposition), and planner_cap v8 states it. Ask-back: on the conversation path (lease-bound chat turn) with the flag on and rounds left, the run stops with `domain.InputRequired` (questions, stage, round, partial coverage, ledger ref of the recorded state) on `ExecuteResult.InputRequired` and metadata `_input_required`; the chat side sets `_ask_back_round` when it delegates the answer. Elsewhere the planner is re-called told the person cannot be asked and its `assumptions` go into `RequirementChecklist.Assumptions` (`report` returns the questions instead). |
| `internal/metabolism/compose/` + `app/composed_workers.go` + `network/composed_workers.go` | ADR-0145 D12 composed workers (2026-11-08, `execution.workers.{composed,runtime_agent_id,max_skills}`, **default off**, needs the ledger; T-20261108-adr-0145-d12-kernel-composes-plan-step-workers). After the gatekeeper's eligibility (capabilities + effects), `dispatch.Dispatcher.Composer` turns a plan step whose winner is a PROFILE agent into one composed worker: professions = the eligible profile candidates declaring a required capability (the winner alone when the step requires none); skills = their effective skills (profile + grants, served by the kernel, sorted, capped); tools = registry tools inside some profession allowlist AND the act-for scope check, minus tools with resource arguments (the step's declared effects no longer narrow them, ADR-0145 D14); brief = the step query; ledger refs = the step's dependency/input `led_` ids. `domain.WorkerComposition` is bound to the step lease in `domain.ComposedLeases` (chained into `ToolExecutor.LeaseCeiling`; composed tools are the lease's run-scoped grants), stamped as `_composition` (data), recorded as ledger kind `composition` (migration 0036), and the step is called on the runtime agent `composed_worker` (`worker_agent.py --composed`, registered by profile discovery only with the flag on, `System` so never a candidate, prompt `prompts/composed_worker` v1 with the D13 worker rules). `ListTools`/`ListSkills` serve the composition as the menu and skill index; a skill outside it is denied with no operator request; the runtime without a composed lease gets no tool, menu or skill. A class-(b) winner (no profile) is dispatched as before; a composed step has no runner-ups. |
| `internal/contentscan/` | Shared deterministic instruction-text scanner (D13 skills, D14 notes): prompt-injection markers, credential requests, exfiltration, tool-policy override, invisible characters; verdict `clean` / `suspicious` / `dangerous` + findings. A danger phrase inside double quotes is downgraded to caution (shipped `web-research` quotes one as a warning; `TestScan_ShippedSkillsAreNotDangerous` guards false positives). |
| `internal/health/` | gRPC health check + the `/healthz` handler `app` serves under the listener policy (PLAT-03 / ADR-0065, ADR-0130 D3). |
| `internal/ratelimit/` | Per-principal token-bucket interceptors on both gRPC planes (`server.rate_limit`; ADR-0047 / ADR-0130). |
| `internal/telemetry/` | OTel bridge — the only package importing OTel. |
| `api/proto/` | The gRPC/protobuf contract: `cambrian.proto`, `operator.proto`. |
| `agents/` | Production Python agents auto-discovered by BBolt: the chat front desk (`chat_agent.py`), the system organs (`agents/system/`) and, since agent stack v2 part B (2026-11-05), one agent per declarative profile in `agents/profiles/<name>.yaml` (starter profiles `assistant` / `researcher` / `data-analyst`; format and the kernel contract in `agents/profiles/README.md`). Every profile agent runs the one runtime `worker_agent.py` as `--profile <name> --agent-id <id>`; the runtime file itself is not an agent. The thin per-role agents (analyst, calculator, code_executor, code_generator, research, summariser, terminal) were deleted. |
| `internal/agentprofile/` | Go port of the SDK's profile parser and validator (`sdk/cambrian_agent_sdk/profiles.py`: the prompts/SPEC.md §2.2 strict YAML subset, not yaml.v3). Parity is held by the Go-written fixture `testdata/profile_cases.json` (`TestProfileCasesFixture`, `-update-profile-cases`) that `sdk/tests/test_profile_parity.py` checks; `TestShippedProfiles_ToolIDsMatchTheCuratedMCPSet` holds the starter profiles to `configs/mcp.json.example`. |
| `internal/agentcard/` | ADR-0139 D4/C2 A2A 1.0 Agent Cards: `Build` derives a card from a registered agent's definition + manifest (profile capabilities become skills with the profile `examples`; trust tier, allowlist globs, skills and source hash in the `urn:cambrian:a2a:ext:agent:v1` extension; Bearer security scheme; `supportedInterfaces` only from `server.a2a.interfaces`), `Signer` signs a detached EdDSA JWS over the RFC 8785 canonical card (`jcs.go`, in-house, duplicate member names refused, RFC vectors in tests), `Verify` checks EdDSA/ES256 against a pinned JWKS, `RemoteFetcher` applies `a2a.signature_policy` to remote cards, `Publisher` serves `/a2a/{id}/.well-known/agent-card.json`, `/a2a/agents`, `/.well-known/jwks.json` on the `server.a2a` listener (`app/agentcard_endpoint.go`). |
| `scripts/` | Build, test, CI helpers. |
| `configs/` | Committed starter config; real `*.json` files are gitignored. |

---

## Key Invariants

- **Hexagonal boundary**: every layer below `domain/` is replaceable from its ports.
- **The step cache serves only read-only steps, only to the same session and principal** (ADR-0145 D9,
  2026-11-08, `T-20261108-step-cache-key-omits-principal-scope-and`). `executer/step_cache_guard.go` salts the
  ADR-0026 key with the session, principal, run principal, conversation owner, act-for actor and the scope and
  isolation predicate hashes on ctx; a run with none of these caches nothing. Only thought steps and steps whose
  declared `required_effects` are all read-only vocabulary terms (`StepEffectVocabulary.ReadOnly`; nil effects,
  unknown or write/move/delete/execute/egress/send/spend/admin ⇒ never cached) are served or stored. The fallback
  TTL is `execution.plan.step_cache_default_ttl_minutes` (default 60; was a hardcoded 7 days). Cross-session reuse
  is gone by design (a reproduction showed principal B served principal A's dependent-step result).
- **One result pathway when the run ledger is on** (ADR-0145 D1, 2026-11-08). With `execution.ledger.enabled`
  the plan's DAG executor gets NO content store (`Server.planContentStore`): step results are `step_output` ledger
  entries, `step_N_cid` is the ledger id, and a dependant's upstream content is read FROM THE LEDGER
  (`buildStepDependenciesFrom` + `ledgerResolver`, under the run owner's bypass-with-owner-fence predicate); an entry
  that cannot be read is listed `unavailable` with the reason, never replaced by the masterContext string. A tool
  result over the inline threshold returns its ledger id as `ResultCID` instead of a content-store cid, and
  `GetContextNode` resolves `led_…` ids through `EnforcingLedger` under the caller's predicate, so the SDK's
  `resolve_cid` works unchanged. The bbolt content store then holds only worker scratch offloads (`PutContextNode`),
  tool jail files and ingestion bytes. Flag off = the legacy plan-scoped content-store path, kept until the ledger
  passes the ADR-0145 phase-1 gate and defaults on; then the content-store branch of `mergeStepResult`, the plan-end
  GC of `step_result`/`tool_result` and the `ToolExecutor` CAS offload are deleted (ticket
  `T-20261108-delete-the-legacy-plan-scoped-content-store`).
- **Done is decided in code** (ADR-0145 D5-D7/D9 phase 2, 2026-11-08, all default off:
  `execution.plan.{checklist_enabled, coverage_max_stages, plan_validator, plan_validator_max_repairs}`,
  `execution.tools.{validate_args, idempotent_writes}`; the `effect_menu` flag was removed by ADR-0145 D14). `checklist_enabled` (requires the ledger): a
  checklist from the user's own words; planner arm `planner_cap` v7 + `planner_input` v3 (steps carry `answers`,
  `inputs`, `for_each`; no thought steps); every answering step returns the fixed envelope, validated against the
  ledger with one repair; item statuses change only in `requirements.Coverage`; the run continues in coverage stages
  until every item is final or a stage makes no progress (stall ⇒ open items close as gaps), with only a runaway cap;
  tool results carry their ledger id (`ToolExecutor.ReturnLedgerRef`) so workers can cite them. `plan_validator`:
  `planvalidate.Validate` replaces the DAG-only check, violations back to the planner; the planner-call budget is
  `max_replan_attempts + plan_validator_max_repairs`. The D9 per-step effect menu (a step's tools limited to its
  declared `required_effects`) and the D7 effect rules were REMOVED by ADR-0145 D14 (2026-11-09): `required_effects`
  stays only as an agent-selection hint (effect-checked dispatch), the step cache's read-only test and the
  side-effecting fan-out test; the request-tied guards replace it (`execution.tools.request_guards`, see
  `network/request_guards.go` above). `validate_args`: coercion + schema validation before
  authorisation (`internal/toolargs`; fixes the `mail_draft to` JSON-string defect). `idempotent_writes`:
  `domain.IdempotencyCache` keyed `hash(run, step, tool, canonical args)` for side-effecting calls; a retry or a
  concurrent duplicate gets the stored result; failures are not stored. The REQ-CACHE-1 exact-match plan cache is
  removed unconditionally (`Hippocampus.Store` had no production caller). Legacy paths (flags off) are listed in
  `T-20261108-delete-the-pre-checklist-prose-step-path-once`. Ledger step outputs inherit the classification of
  what they were built from: `LedgerEntry.SourceTags` (migration 0035), read gate = conjunction
  (`domain.LedgerEntryReadable`).
- **Stages, not replans, under the checklist** (ADR-0145 D5.5/D11, 2026-11-08). With `checklist_enabled` the DAG executor gets no replan handler: a failed step ends its stage (`stageStoppedPartway`; an effect-check refusal and a plan ceiling keep their own endings) and the next coverage stage plans the open items with `answers` (`network/coverage_stages.go runStages`). A plan generation after a replan, hot swap or injection (legacy path) records to the ledger under `domain.LedgerRunID(run, generation)` = `<run>-g<n>` (the generation rides the step ctx to `LeaseBinding.PlanGeneration` and `PlanAction.Generation`), so step indices never collide across replans. Refused READ calls are kept per run (`planActionLedger.recordDenial`) and reach coverage as `StepObservation.Denials`, so `not_permitted` is accepted on an access refusal of a read.
- **One content budget** (owner decision 2026-11-12, T-20261112-worker-resolve-cid-inline-has-no-size-cap-a /
  T-20261112-summarise-a-document-step-gets-only-the-first; `internal/config/content.go`, `domain/content_shape.go`).
  Every limit on how much content reaches a prompt is `execution.content` (`inline_result_bytes` 65536 — was hardcoded in
  `app.go` —, `preview_chars` 600, `page_bytes` 16000 — moved from `ledger.page_bytes` —, `tool_output_max_chars` 24000,
  `worker_prompt_budget_tokens` 32000, `dependency_budget_fraction` 0.5, `dependency_max_bytes` 0; keys moved by
  `relocatedExecutionKey` in `execution_migrate.go` for files, store and env). The SDK limits are served to every spawned
  agent as `CAMBRIAN_CONTEXT_*` (`kernel.contentEnv`). At record time the `ToolExecutor` turns every binary block of an MCP
  content array (image/audio/blob) into `[image/png image, 236 KB -> ledger led_…]` (reusing the staged-output artifact
  when the bytes match, else one artifact entry; text blocks kept); a result over the inline threshold travels as its
  reference WITH `domain.OffloadSummary` (`{"_offloaded":{ref,size_bytes,content_type,shape,preview,pages,read}}` in
  `result_json` beside `result_cid`, `network.wireResultJSON`; `ToolCallResponse.ResultJSON` stays nil for kernel
  consumers); with the ledger on a result longer than its preview also carries its ledger id. A worker may answer with
  `answer_ref` (handoff metadata `_answer_ref`): the DAG executor accepts only a `tool_result` of the same ledger run and
  step (`executer/answer_ref.go`) and makes its text the step output; anything else fails the step deterministically.
  A prompt over a model's window is `domain.ErrContextOverflow` (provider 400/413 + error code first, message text only as
  the fallback, incl. a proxy's streamed `server_error`; `StreamChunk.Err` keeps the identity across the stream): no
  fallback, no breaker event, gateway `codes.OutOfRange`, self-healer deterministic; the SDK answers a worker that died of
  it with OUT_OF_RANGE (was INTERNAL, which was retried). `GetContextNode` on a `led_` id still returns the whole body,
  for the SDK's budgeted `assemble_context` only.
- **Execute returns a structured result** (ADR-0145 D8, 2026-11-08). Besides the report text (payload, unchanged),
  the response metadata carries `_execute_result` = `domain.ExecuteResult` {run_id, session_id, report, actions (plan
  action ledger), steps [every step of the FINAL plan: status, ledger `ref`, preview, bytes, fan_out_item], partial,
  timed_out, timeout_cause, failed_step, error, ledger}; built by `network/execute_result.go` from the executor's
  `executer.PlanStepsKey` index (consumed, never returned). Fan-out children depend on the source step and carry the
  whole source element (`Step.FanOutItemJSON`, stamped as handoff key `_fan_out_item`; `domain.FanOutItemObjects`).
- **The chat turn is grounded in the ledger** (ADR-0145 phase 3 chat side D2/D3/D5a/D8, 2026-11-08, agent:claude-p3chat;
  every key **default off**, all but the budget need `execution.ledger.enabled`; wired in `app/chat_grounding.go`).
  `TurnDelegations` keeps each DIRECT delegation's `_execute_result` until the turn ends (`DelegatedRun.SetResult`
  from a deferred capture in `Server.Execute`; `TurnResults`), so the chat lane reads the runs kernel-side.
  `execution.chat.transcript_budget_tokens` (>0 replaces `DefaultHistoryLimit=40`): newest messages within the
  budget (bytes/4), the overflow folded by the kernel into a rolling summary (prompts `conversation_summary` v1 +
  `_input` v1, organ call over the planner generator, `computed` entry `{through_seq,text}`, each message once).
  `turn_index` (+`turn_index_turns`, 8): one `chat_turn_index` entry per delegating turn (runs, items with status and
  values + refs, actions numbered per tool, step refs; `DerivedFrom` every ref), shown as `<turn_index>`.
  `grounded_reply`: `delegate_to_planner` returns an `<evidence>` block (chat_agent_turn v7) and chat_agent v5
  `rules-grounded` asks for inline `[led_…]` ids and `[R#]` item tags (the proxy has no native citations).
  `reply_check` = observe | enforce (`internal/replycheck` in `TurnService.Turn`, evidence through the enforced
  reader under the owner's bypass-with-owner-fence predicate): observe logs a `reply_check` event, counts
  (`ReplyCheckStats`), records `chat_reply_check` and emits a `reply_check` run-trace span (incl. `delegated`, the D3
  measure); enforce re-dispatches ONCE with `reply_revision` + Go-rendered `reply_evidence`, then states what is still
  unbacked in the turn language (`MsgReplyUnverified`/`MsgReplyUncovered`). Markers are always stripped before Emit.
  `delegation_rule`: chat_agent v5 `rules-delegation` + every delegation carries the message verbatim in
  `<user_request>`. `ledger_tools`: chat-agent `ledger_get`/`ledger_find` via `ExecuteTool` on the turn lease.
  `ask_back`: an `InputRequired` run ends the turn with its questions (chat_agent_turn v7 `ask-person`), the pending
  state rides the turn index; the next message gets `<pending_request>`, and the turn lease carries
  `LeaseBinding.AskBackRound`/`AskBackPendingRef`, which `Server.Execute` (`stampAskBack`) stamps as
  `_ask_back_round`/`_ask_back_pending`, replacing anything a caller sent. Guards retired under these keys (not run;
  deleted when the keys default on, `T-20261108-delete-the-english-only-chat-guards-once-the`): `_NO_ACCESS_CLAIM`
  report retry when `reply_check=enforce`; `_ACTION_PROMISE` force-delegate retry when the SDK's
  `CAMBRIAN_EVIDENCE_REPROMPT` is on.
- **Decision-level run trace** (ADR-0145 D10 / ADR-0139 C6, 2026-11-08, `execution.trace.{enabled,dir,max_content_chars}`,
  default off). One `domain.RunTracer` port (`domain/run_trace.go`, OTel GenAI semconv names), one adapter
  (`internal/infrastructure/runtrace`: one JSONL file per process, strings bounded, system prompt and tool menu in
  full once per file then by `<attr>_sha256`, trace id = sha256(session)). Fed by: the LLM Provider's `decorate()`
  (`llm/run_trace_layer.go`, so every organ call AND every agent-plane gateway call is a `chat {model}` span with
  system, messages, tools offered and the response), the DAG executor (`executer/step_trace.go`, one
  `invoke_agent` span per step with query, deps, effects, output, cached flag) and `ExecuteTool`
  (`network/run_trace.go`, `execute_tool {name}` with exact arguments and result). Wired in `app/run_trace_wiring.go`;
  agents get `CAMBRIAN_TRACE_DIR` via `kernel/trace_env.go`. The use-case harness turns it on and copies the files to
  `runs/<dir>/traces/`. START EVENTS (2026-11-09, T-20261109-live-run-viewer-watch-model-calls-prompts-tool): every
  model call (all five `runTraceGenerator` entry points, so planner/thought/router organs and gateway calls), every
  DAG step (`executeStepTraced`, before the step runs; agent not yet known → `invoke_agent step|thought`) and every
  kernel-executed tool call (`traceToolCallStart`) first writes a line with the SAME span id (`domain.StartTraceSpan`,
  `TraceSpan.SpanID`/`Phase`), the request attributes, `"cambrian.phase": "start"` and `end_time_unix_nano` 0; the
  finished span is unchanged and completes the pair. Every line is flushed as written, so a follower sees a call while
  it runs (the reader is cambrian-benchmarks `usecases/watch.py`; finished-span readers skip `phase=start`). Model
  calls also name `cambrian.request.response_schema_name` (the prompt id: `requirement_checklist`, `planner`,
  `replan`, …). `domain.WithTraceAttributes` puts attributes on every span recorded under a ctx (the span's own win):
  the agent-plane gateway stamps the lease, bound agent, run, step and conversation on its model calls
  (`leaseTraceAttrs`, read under the gateway lock), so the kernel's `chat` span of an agent call joins the agent's.
- **ONE addressed approval hub, answered only by an addressee** (ADR-0142 D11 + owner acceptance 2026-11-06,
  stage 1B). `domain.AddressedHub` (`domain/approval_hub.go`, policy `approval_policy.go`, store port
  `approval_store.go`; Postgres `internal/infrastructure/postgres/approval_store.go`, migration 0033) replaced the
  operator broadcast hub (`InMemoryApprovalController`, deleted: `appr-<n>` ids, any operator answered anything) and
  the CL-2 consent controller (`InMemoryConsentController`, deleted). Every kind rides it: dangerous tools (wire kind
  `""`, class `tool`), `skill_access`, `schedule`, proactive plans (class `proactive_plan`), machine consent / choice
  (`AddressedHub.Consent()` is a VIEW implementing `domain.WorkerConsentHub`), premium `account_confirm`, and D12
  `publish`. A request is routed by `domain.ApprovalPolicy` (OSS `ConfigApprovalPolicy` over the top-level
  `approvals` block, `internal/config/approvals.go`) to approver rules (any-of): `self` (the beneficiary; dropped for
  `never_self_approvable` classes), `person:<id>`, `role:<name>`, `operator`, `relationship:<rel>` (declared;
  resolved only by an installed `ApprovalRelationships` — stage 2 premium — else a relationship-only request fails
  closed as `needs_hierarchy`, never falling back to self). Ids are `apr-<hash(natural key, round)>`; pending items
  survive a restart (a decision recorded while nobody waited is handed to the next identical request; the boot +
  periodic sweep expires orphans and publishes them). `Answer` authorises at ANSWER time (`ErrApprovalNotAddressee`
  for anyone else, an operator included). Standing approvals (`StandingApproval`, `stand-<hash(class, resource,
  scope)>`) pre-approve until revoked/expired, cap `approvals.standing_max_ttl_hours`, and never cover a never-self
  class when the beneficiary granted them. Every resolution publishes `HITLResolvedEvent` → feed `HITLResolvedOp`.
  Delivery is addressed (`ApprovalWatch`: an approver identity, the bridge, or the operator audit view); a blocking
  request with no eligible approver connected fails closed at once (`unroutable`, the old "no subscriber"), logged
  (`approval_unroutable`); a request naming no beneficiary under only self/relationship rules fails closed with that
  reason; `ApprovalDecision.Detail` reaches the agent via `domain.ApprovalDenyReason`. The tool beneficiary comes from
  the lease ExecuteTool receives in its BODY (`network.withPresentedLease` → `withCallerSession`, conversation via the
  step session's ADR-0084 link) — the SDK sends no lease header (T-20261107-tool-approvals-raised-by-chat-delegated-plan).
  A tool denial names the check that refused (`domain.ToolAccessDenyReason`, `domain/hook_tool.go`): "effect not
  permitted: <effect>" only for an effect denial, "classification not permitted (<reason>): …; the tool's
  classification tags: …" (+ the act-for owner) for a tag denial, "data scope not permitted: …" for Regime 1
  (T-20261107-acting-for-user-is-denied-their-own-finance; every denial used to read "effect not permitted"). Planes:
  `WatchToolApprovals`/`WatchApprovals`/`ResolveHITL`/`SubmitApprovalDecision` keep their shape (pending-only,
  addressed to the caller; ResolveHITL refuses a non-addressee with PERMISSION_DENIED before auditing); new
  `WatchMyApprovals` / `ListApprovals` / `AnswerApproval` / `ListStandingApprovals` / `RevokeStandingApproval`
  (`operator/approvals_hub.go`, open to every role incl. member; capability `addressed-approvals`). The operator
  interceptor stamps the full role set (`domain.WithPrincipalRoles`) for `role:` rules. Wired once in
  `app/approvals.go` (`buildApprovalHub`) and exposed to plugins as `KernelServices.Approvals` /
  `CredentialServices.Approvals`. **Stage 2 seams (2026-11-06):** the premium org directory installs its resolver
  (`SetRelationships`) and a policy decorating the config one (`Policy()` / `SetPolicy`, Tier-1 replace at Build;
  the hub is never unsafe without it); with a resolver installed, a request whose relationship rules all resolve to
  nobody fails closed AT ONCE as `needs_hierarchy` naming the missing edge; `WithdrawForBeneficiary` withdraws a
  deprovisioned person's pending approvals; `ApprovalRequest.Args` (the call's full argument JSON, set by the tool
  executor) reaches `ApprovalPolicy.Route` for approval limits and is never persisted or delivered; the natural key
  hashes the full Args and the routed rules, so an above-limit request never joins (or reuses the answer of) a
  self-approvable one. `domain.Identity.Claims` carries a VERIFIED OIDC token's claims (login-claim mapping);
  `KernelServices.OperatorCaller` / `RunOperatorMutation` / `RecordAudit` let a premium plane run the
  OperatorConsole mutation contract; `OrgDirectoryAdmin/GetMyOrgChain` is open to the member role and its SCIM /
  reconcile reads are Operator-only (`operator/authz.go`).
- **Private by default** (ADR-0142 D12, closes T-20261106-agent-writes-during-a-conversation-owner-s-turn). When
  the kernel resolves the conversation a lease works for (`network.withConversationOwner`) and
  `privacy.private_by_default` (absent ⇒ on) holds, the context is MARKED (`domain.WithPrivateWrites`) and every
  write chokepoint — `authz.EnforcingStoreWriter`, `EnforcingKnowledgeWriter`, `memory.EnforcingDocumentStore`,
  `authz.ClassifyArtifactWrite`, tool-artifact promotion (`ArtifactTags`) — appends `owner:<owner>` AFTER the
  decision point classifies (`domain.StampPrivateOwner`). `IngestMemory` and `UploadArtifact` resolve the owner
  too. The OSS `AllowAllAuthorizer.ReadFilter` now fills `Owners` (the reader itself when a person, plus the
  conversation owner) so the owner fence admits a person's own material in OSS. A sender bound to a person opens
  person-owned conversations (`ingress/inbound.go open`); unbound senders keep the pseudonym. Sharing is the
  explicit `ShareToTeam` RPC (`operator/share_to_team.go`, `app/share_to_team.go`): owner-only, target tags must
  survive the caller's `ClassifyWrite`, other owners' fences are kept, routed through the hub as class `publish`
  (default self ⇒ applies at once), audited (capability `share-to-team`). Memory NOTES keep their own
  personal/org model (personal is owner-keyed; org needs `agent_org_write`).
- **Act-for conversations and the conversation owner** (2026-11-06, T-20261106-conversations-cannot-be-opened-as-a-given).
  An operator may open/use a conversation ON BEHALF OF a principal (`on_behalf_of` on OpenConversation, SendTurn,
  CloseConversation, ListConversations, ListConversationMessages; `internal/substrate/operator/actfor.go`). Only
  `RoleOperator` (`operator.CanActFor`); the principal must be known to the premium `domain.PrincipalDirectory`
  (no directory ⇒ FailedPrecondition, unknown ⇒ NotFound, directory error ⇒ Unavailable); audited with the operator
  as actor (`open_conversation_on_behalf`, `send_turn_on_behalf`). The owner is that principal, so the ADR-0139 D14 run
  principal (notes, skill grants, per-user credentials) is theirs. Agent-plane reads and tool calls of a chat turn
  carry `domain.WithConversationOwner` (stamped from the LEASE in `network.withConversationOwner`, called by
  `withCallerSession` before its no-session return and by `QueryMemory`; `app.conversationOwnerOf`, no
  beneficiary/default fallback); the premium Authorizer intersects the owner's resolved predicate into the agent's.
  Capability `conversations-act-for`. End users still have no login of their own
  (`T-20261106-end-users-have-no-operator-plane-login-a-person`).
- **Step and turn time limits** (T-20261105-slow-model-reliability-hardcoded-20-s, 2026-11-05):
  - A plan step's deadline is set by the Dispatcher alone (`dispatch.stepTimeout`):
    observed median latency × `execution.plan.step_timeout_multiplier` + `step_timeout_base_buffer_ms`,
    floored at `execution.plan.step_timeout_floor_ms` (default 180000).
  - A winner with no latency profile gets no per-step deadline.
  - The whole plan has a PROGRESS-AWARE ceiling (owner decision 2026-11-05,
    T-20261105-owner-decision-needed-120-s-plan-timeout-ms; `internal/substrate/network/plan_watchdog.go`):
    an idle watchdog cancels it after `execution.plan.plan_idle_timeout_ms` (180000; 0 disables) with no progress, and
    `execution.plan.plan_max_duration_ms` (1800000) is the absolute cap. Progress = a step starting/finishing, a model,
    tool or recall call in flight on one of the plan's step leases (`gateway.RunActivity(runID)`, the same
    `InFlightCalls` counter the chat watchdog reads; `ExecuteTool`/`QueryMemory` mark it via `TrackLeaseWork`), an
    in-kernel thought or replan call, or a progress event for the plan's conversation. Both causes wrap
    `context.DeadlineExceeded` and name their key; `SelfHealer` reports them as "step N timed out … (plan idle for …
    / plan exceeded max duration …)" and stops retrying; `Execute` stamps `_plan_timed_out` / `_plan_timeout_cause` on a
    partial plan or returns `DeadlineExceeded`. A third cause (2026-11-07,
    T-20261107-a-delegated-plan-stopped-by-its-caller-s): a caller with a deadline (a chat turn's delegation) has the
    plan stopped `execution.plan.caller_deadline_reserve_ms` (5000; 0 disables; at most half the caller's time) BEFORE
    it, so `Execute` answers in time with the timed-out partial payload ("The plan did not finish: it ran out of time at
    step N: …", the completed steps' report and the `COMPLETED ACTIONS` ledger) instead of dying with the RPC. The reserve
    instant is fixed once per `Execute` (`callerReserve`, plan_watchdog.go) and also bounds the router's Layer-3 call and
    the planner's call; a cut there answers in time "The plan did not start: it ran out of time while planning the
    request, so nothing was executed" as a timed-out partial result (T-20261107-planner-llm-call-is-not-bounded-by-the-caller). A plan
    cancelled while paused now leaves the executor through its completion block as a `PartialPlanError` (FailedStep -1)
    instead of returning the bare ctx error, and `RecordPlanOutcome` runs on `context.WithoutCancel`. It replaced the fixed `plan_timeout_ms` (120 s), which is now a deprecated
    key: an explicit value loads as `plan_max_duration_ms` (per layer, `deprecatedExecutionKey` in
    `internal/config/execution_migrate.go`).
  - Repeated timeouts are never reported as "loop detected".
  - The Dispatcher stamps `DispatchTask.Deadline` with the deadline the step actually runs under. It used to be a
    hardcoded `now + 20 s` that nothing read.
  - The chat stall watchdog's probe is `chat.LatestActivity(progress, gateway.ConversationActivity)`. A model call in
    flight on the turn's lease (`BudgetLeaseState.InFlightCalls`) counts as liveness, so one long unary generation is not
    a stall. A turn with no call in flight and no progress for `execution.chat.chat_stall_timeout_seconds` (90 s) is
    still cancelled. The turn bound itself is `execution.chat.chat_turn_timeout_seconds` (240 s; also the turn lease's
    TTL; 0 = the package default), set on `TurnService` in `app/app.go` (was the hardcoded `chat.DefaultTurnTimeout`,
    T-20261107-chat-turn-timeout-is-hardcoded-at-240-s).
  - A chat turn's delegated plan lives no longer than the turn and counts as its liveness (2026-11-06,
    `internal/substrate/network/turn_delegation.go`, `TurnDelegations`; T-20261106-a-chat-turn-that-fails-deadline-exceeded-or,
    T-20261106-chat-stall-detection-cancels-a-turn-whose). `Execute` registers a run under the caller lease's
    conversation + origin message (a nested Execute inherits through its step lease's RunID) and runs under a context
    the turn's end cancels; `TurnService` calls `EndTurn` on every exit (reply, deadline, stall, failure,
    `CloseConversation`, which `conversationOps.Close` now routes through). `EndTurn` cancels the runs, revokes the turn
    lease and the runs' step leases (also once Completed) and cancels tool calls in flight on them, which withdraws a
    pending approval. `ExecuteTool` refuses an ended turn's lease before authorisation, so no approval can be raised
    after the turn ended; a late Execute is refused `FailedPrecondition`. The chat probe is
    `LatestActivity(progress, gateway.ConversationActivity, TurnDelegations.ConversationActivity)`, the last reading each
    delegated plan's own `planLivenessProbe`; an approval wait is a tool call in flight, so it is not a stall, bounded by
    the approval's own timeout. A withdrawn approval publishes `HITLResolvedEvent{outcome: withdrawn}` (ADR-0142 D11), and the bypass-selection arm does
    not register (tickets below in the ticket log).
- **Premium never leaks in**: `check-no-premium.sh` audits imports.
- **Memory is an Engram engine**: typed documents, graph layer (BFS with 0.75^depth attenuation), content-addressed `ArtifactVault`.
- **No scenes, no precedents, no induction, no memory judge** (owner decision 2026-11-05,
  `cambrian-knowledge/decisions/scenes-cancelled.md`, `T-20261105-remove-scenes-completely-owner-decision-2026-11`).
  Deleted: the ADR-0028 ingest `SceneGenerator`; per-step and plan `mnemonic_scene` chunks (`DocTypeMnemonicScene`),
  `discussed_in` / `engaged` / `follows` edges and the `EdgeWriter` port; the ADR-0015 Tier-1 channel and tier-2 judge
  with their only feeders (`RecordExecution`, the read-output fact path) and `Agent.Query`; precedents
  (`SearchPrecedents`, `SearchScenes`, x-lanes `scenes`/`precedents` now fall through to facts, planner `<PrecedentLTM>`);
  WorkspaceStage scene query priming (the FACT query uses the raw embedding); ADR-0094 induction (`ProcedureScheduler`,
  `ProcedureInducer`). Kept: `memory.Agent.RecordPlanOutcome` (was `WritePlanScene`) closes the ADR-0095 episode, writes
  the A2.3 failure record (text from the `PlanRecord`, now also for plans that engaged no entity) and feeds stored
  procedures (`FeedProcedureOutcome`); mutations still become `mnemonic_action` records (`RecordToolOutput`); stored
  procedures are still served. Migration `0028_delete_scenes.sql` deletes stored scene rows, their embeddings and their
  scene-only edges. Retired config keys (accepted and ignored): `scene_gen_on_ingest_enabled`,
  `experiential_memory_enabled`, `tier1_channel_capacity`, `tier2_batch_size`, `tier2_llm_timeout`,
  `tier2_max_idle_seconds`, `procedure_induction_interval_hours`, `procedure_min_samples`,
  `procedure_max_episodes_per_pass`, `workspace_enable_drift_guard`, `workspace_drift_threshold`.
- **Dispatch is unconditional**: ADR-0100 P3 deleted the auction; selection is capability-typed. The deprecated
  `RequestProposal` RPC has no kernel caller; the SDK answers it with a neutral proposal (confidence 0) and never
  runs an agent's `propose()` (agent stack v2).
- **Specialists are declarative profiles, and their tool allowlists are enforced kernel-side** (agent stack v2
  part B, owner directive `decisions/agent-stack-v2-mcp-tools-and-profiles.md` D3,
  `T-20261105-agent-stack-v2-worker-agent-runtime-plus`). Discovery (`internal/storage/profile_discovery.go`)
  registers one agent per valid `agents/profiles/*.yaml`, validated against the skills the kernel serves; an invalid
  profile is a boot ERROR and is skipped, never fatal; `reconcileProfileAgents` prunes profile-runtime records not
  discovered this boot. `AgentDefinition` carries `LaunchArgs`, `Profile`, `TrustTier` and `ModelPreference`; the
  manifest carries `tool_allowlist` and `skills`, never `tools`. The allowlist is enforced twice
  (`app/profile_agents.go`, `domain/tool_ceiling.go`): as grants (the globs' expansion over the tool registry,
  re-synced by the MCP tool sink on every server connect/reconnect/drop) and as a glob ceiling
  (`ToolExecutor.Ceiling`) consulted wherever the ADR-0051 `RestrictedTools` ceiling is, before the
  `tools_unrestricted` bypass and the skill overlay, so a profile agent stays inside its allowlist in unrestricted
  mode too. Only profiles get allowlist grants (an ordinary agent's self-declared `tool_allowlist` is not a grant).
  A profile's `model:` is the first rung of its agent's lease model chain (`SubstrateLLMGateway.
  SetAgentModelPreference`, resolved per call; a dangling id is a boot WARN, `resolved=false`). `trust_tier` is
  stored only (see Known gaps). `domain.IsSystemAgent` no longer lists the never-implemented `operator_agent`.
- **Plan fan-out: every sibling expands, an untemplated step runs once, children run in parallel** (2026-11-07,
  `T-20261107-only-the-first-fan-out-step-over-a-source`, `T-20261107-fan-out-step-without-an-item-placeholder`,
  `T-20261107-plan-fan-out-steps-run-one-at-a-time-on-a`). When a step completes the DAG executor expands EVERY
  parametric step over it (`domain.PendingFanOuts`; `PendingFanOut`, which returned only the first and left sibling
  fan-outs and their dependents undispatched, is deleted). A parametric step whose query lacks its `{item}`/`{var}`
  token is DEMOTED by `domain.ExpandFanOut` to one ordinary step over the whole set (FanOutOver cleared, source
  added to DependsOn, logged WARN) instead of N identical children that repeated side-effecting writes N times;
  operator-authored plans get a non-fatal `IssueBadFanOut` warning for it. Children carry `Step.FanOutItem` and run
  under `execution.plan.fan_out_concurrency` (default 4; 0/1 = one at a time on the shared instance): above 1 each
  child runs on a dedicated agent instance (`domain.WithDedicatedInstance`, the ADR-0139 D12 mechanism), because an
  SDK agent process serves one Execute at a time and every call to an agent id went to its one cached instance —
  19 children ran serially at ~11 s each and overran the 240 s chat turn. Cost: one agent boot per child. The wait
  for a slot happens before dispatch stamps the step deadline. The planner prompts now prefer one bulk/list call over a
  per-record fan-out (planner v2, planner_cap v5, replan v2, replan_cap v5; not yet measured in a recorded run).
- **A side-effecting fan-out expands only over a structured list** (2026-11-07,
  `T-20261107-fan-out-over-a-prose-source-splits-markdown`). The executor's one fan-out decision is
  `domain.ResolveFanOut`. A step that is side-effecting (`Step.FanOutSideEffecting`: not a thought, and any declared
  effect other than `read`, or no `required_effects` at all) expands only when the source output is structured
  (`domain.ExtractStructuredFanOutItems`: the whole output is a JSON array / known wrapper object, or it holds exactly
  one fenced code block of such JSON); otherwise it is DEMOTED to one step over the whole source, its `{item}` token
  rewritten to "(each item listed in the result of step N)" plus an exactly-once instruction (WARN log). Splitting a
  thought step's markdown into lines had made 15 write children (headings, table header rows) and 54 issues for 3
  incidents. A read-only/thought fan-out may still fall back to free-text lines, but only list lines: headings,
  table header/separator rows, rules, fences and lead-in lines ending in ':' are dropped and bullets stripped. An
  empty source still expands to no children. Consequence: a write fan-out over a plain newline list (a tool's
  file listing an agent echoed as lines) now runs once rather than per item until the source emits JSON
  (planner-prompt follow-up `T-20261107-planner-prompt-does-not-steer-fan-out-no-rule`). Since that follow-up the source
  emits JSON by construction as far as the kernel can make it: the executor appends the `fanout_source` prompt
  fragment to the dispatched task of every step a templated `fan_out_over` step reads from
  (`executer/fanout_source.go`: end the reply with one fenced JSON array of strings, naming the per-item query it
  feeds; agent steps get it on the payload only, so the auction's task description stays the planner's text; thought
  steps on the ctx step), and the planner/replan prompts ask the source step's query for a JSON array and a
  parametric query that describes one item.
- **JSON fan-out sources are read as JSON, never as lines** (2026-11-08,
  `T-20261107-read-only-fan-out-over-an-unrecognised-json`). o3 #1's source answered with a JSON object holding two
  arrays, a blank line, then the requested name array unfenced (1170 B); no rule matched and the read-only line
  fallback made 26 children of `{`, `],`, `"contact_email": …` that ran the plan into its caller's deadline.
  `ExtractStructuredFanOutItems` now scans the JSON values that start a line (bare or fenced): the reply's TRAILING
  list wins (the `fanout_source` contract), else exactly one list, else none; wrappers descend one single-object
  field (depth 3); object rows are never dropped (known key, then `*name`, then `*id`, else the compact row). A
  read-only step over JSON with no recognisable list is demoted to one step (`FanOutDemotedUnrecognisedJSON`,
  `domain.SourceHoldsJSON` also catches truncated JSON); free text keeps the list-line fallback. Expansion logs
  `source_bytes` and the first items.
- **The kernel tells every model today's date** (2026-11-08, `T-20261107-planner-and-chat-agent-are-not-told-today-s`).
  `domain.Calendar` is the one source, built once in `app` from the new config key `organisation.timezone` (IANA,
  validated at load, empty ⇒ UTC; `time/tzdata` embedded) and shared: planner (`planner_input` v2 `<today>`),
  replans (`replan_input` v2), the executor (stamps `_current_date` = `Calendar.Block()` on every step, strips it
  from responses, and scopes the step-cache key to the date), thought steps (`thought_synthesis_input` v2, stamped
  value else the server calendar), the chat turn handoff (`current_date` metadata, rendered by `agents/chat_agent.py`
  through the new `current_date` prompt) and SDK workers (`react-turn` v5 `current-date`, `react.with_current_date`).
  The block carries today's ISO date and weekday, the timezone, yesterday/tomorrow, this week/month/quarter and
  7/14/30 days ago, so no model subtracts dates itself (o1 delegated "since November 24" for "the last two weeks"
  on 7 Nov). Not covered: the exact-match plan cache can still replay a plan whose queries hold absolute dates
  from an earlier day (follow-up ticket).
- **A plan whose context has ended is not replanned** (2026-11-08,
  `T-20261108-replanhandler-replans-a-step-that-the-caller`): when the caller-deadline reserve, max duration or
  idle watchdog (or the caller) ended the plan context, the executor skips `ReplanHandler` and returns the
  partial-plan report; the replan's planner call had always failed on the dead context (19 ERROR rows in run
  20261107T191751Z).
- **Self-dependencies are repaired as one-based slips** (2026-11-08,
  `T-20261108-planner-writes-depends-on-1-based-step-n`): `domain.RepairSelfDependencies` (planner and replan)
  rewrites a step naming itself in `depends_on` / `fan_out_over` to the previous step (only that case is
  unambiguous; WARN log). Planner v3, planner_cap v6, replan v3, replan_cap v6 add the numbering rule: steps are
  numbered from 0 and queries cite earlier steps by the same numbers.
- **A plan's result reports every final step and every side-effecting call** (2026-11-07,
  `T-20261107-plan-result-is-only-the-last-topo-step-a-plan`). `_dag_final_result` is `executer.buildPlanReport`:
  the result of every completed step no other completed step consumed (the sinks; the completed frontier on a
  partial plan; inert fan-out nodes excluded) — verbatim when there is one, labelled `### Step N — <objective>`
  sections (with the fan-out item) when there are several. It used to be the step Kahn's order put last, so a plan of
  four independent create-issue steps reported one and the chat agent re-created the other three. The kernel also
  keeps a per-run ledger of side-effecting tool calls (`network/plan_actions.go`: recorded in `ExecuteTool` against
  the lease binding's `RunID`/`StepIndex` while `Execute` runs; a dangerous tool or effect write/spend/admin, or an
  unregistered `local:` tool; status done/failed/refused, redacted args, ids pulled from the result by
  `domain.PlanActionIDs`). `Execute` appends it to the payload as a `COMPLETED ACTIONS` block
  (`domain.RenderPlanActions`, worded "already happened: do not repeat") and as JSON under metadata
  `_plan_actions`. A partial plan's payload is now "The plan did not finish: step N failed: …" plus the completed
  steps' report and the ledger (`PartialPlanError.Report`), not the bare error. The ledger also bounds repeated
  unroutable approvals (2026-11-07, part 2 of T-20261107-chat-agent-re-delegates-after-a-deterministic): once a run
  has `execution.tools.max_unroutable_approvals_per_run` (3; 0 disables) calls of a tool refused as unroutable /
  needs_hierarchy (`ToolCallResponse.ApprovalState`), `ExecuteTool` refuses further calls of it in that run without
  raising an approval, in the final-refusal wording (event `approval_unroutable_bound`); human denials and expiries
  are not counted. No duplicate-write guard
  (decision in the ticket). Prior writes ARE now surfaced to the approver (2026-11-07,
  `T-20261107-approver-is-not-told-that-an-identical-side`): `domain.ActionHistory` (`domain/action_history.go`) is a
  conversation-scoped (else session-scoped), bounded, in-memory record of side-effecting calls kept by
  `ToolExecutor.Execute` (awaiting approval → approved/running → done with ids; dropped when refused or when it
  FAILED — a handler error incl. an MCP isError result, or an in-band error/denied result — owner decision 2026-11-10,
  T-20261109-duplicate-notice-fires-on-the-retry-of-a-write: only successful earlier writes, plus calls still in
  flight, are matched, so the corrected retry of a rejected write carries no notice). Before
  raising an approval the executor matches the call deterministically (same tool; identical normalised args, or no
  differing target field such as to/project/path plus a similar subject, or near-identical body, or — 2026-11-08,
  T-20261108-duplicate-notice-misses-a-repeated-record-write — the same record key (ref/case_id/request_id/…, now
  targets) with every structured list outside the targets equal, whatever a free-text note says) and attaches
  `ApprovalRequest.DuplicateNotice` ("Possible duplicate of <tool> (approval <id>) — <state> at <UTC>, ids: …") →
  `ApprovalItem.DuplicateNotice` → `ApprovalOp.duplicate_notice` (22) / cambrian `ApprovalRequest.duplicate_notice` (9)
  → the console HITL card. Annotates, never refuses. Window `execution.tools.duplicate_notice_window_minutes` (1440;
  0 disables). Postgres does not persist the notice (no column); the hub overlays it on Get/List while pending.
- **A step's routing contract reaches every step the executor runs, and the planner sees agents' tools**
  (2026-11-06, `T-20261106-planner-routes-a-file-move-step-to-an-agent`). The executor hands the step it dispatches,
  from its CURRENT plan, to the step callback on ctx (`domain.WithPlanStep` in `executer.executeStep`;
  `network/server.go` stepFn and thoughtFn read `domain.PlanStepFrom`). The callback used to index the plan it
  captured before the run, so every ADR-0078 fan-out child (index past its end) lost `RequiredCapabilities`, pin
  and budget and routed unconstrained, and a replanned step of an equal-length plan routed with the OLD step's
  contract. Under `capability_contract` the planner's `<capability_clusters>` also lists each profile agent's
  allowlist grouped by server (`planner.go` `agentToolLine`), and `planner_cap`/`replan_cap` v2 say to tag an
  acting step (move, rename, create folder, run, send) only with tags whose agents hold a tool for it. The shipped
  `assistant` profile (1.1.0) now organises files: `file_organisation`, `create_directory` + `move_file`, skill
  `organise-files` (no separate agent: it would bring nothing a skill cannot). SDK `react-anti-patterns` v2
  (native) forbids imitating a missing tool's action destructively; the agent yields (`yield_subgoal` + capability
  hint) or says so. The kernel now checks the step's EFFECTS against the agent's tools (next bullet).
  **Since 2026-11-07 the tool lines are the LIVE callable set, not the declared globs**
  (`T-20261107-world-system-asks-are-planned-as-local-file`): `awareness/tool_menu.go` renders each profile agent's
  `ToolExecutor.AvailableTools` (the same lister the effect source reads; `ToolMenuSource`, wired in
  `app/effect_check_wiring.go`) grouped by server, plus a "What the tools do" section with each tool's first
  description sentence once. A glob whose server is not connected shows nothing; `agentToolLine` (globs) is only the
  nil-source fallback. Glob rendering had shown unconnected filesystem tools by name and a live world MCP server as
  "(all tools)", so an AR ask was planned as a file search. `planner_cap`/`replan_cap` v4 add "Plan from the tools
  the agents have": name the tool that reaches the information in the step; search files only for file requests.
- **The chat front desk sees the same live menu, and steps know whom they act for** (2026-11-07).
  `T-20261107-chat-agent-tells-the-user-it-has-no-tool-for`: `awareness.Planner.WorkSystems` renders the MCP
  servers (and their reachable tools, union over planner agents, no native tools) from the planner's own
  `ToolMenuSource`/`liveToolMenu`; `chat.TurnService.SetWorkSystems` (wired in `app/app.go` after `wireEffectCheck`)
  puts it on each turn as handoff key `work_systems` (not for a `customer` conversation). `chat_agent` v2 renders it
  in a `<work_systems>` system section and adds rules: the front desk's skills menu is not the agents' capability
  list, a work-system ask is delegated (never "no tool"), a delegated result is reported (never "no access"), and a
  first-person ask stays first-person in `task_description`. `agents/chat_agent.py` also guards deterministically
  (English-only, like the narration guard): a reply claiming no tool/access after `delegate_to_planner` returned a
  non-denial result is retried once with the result as data (`chat_agent_turn` v2 `report-delegated`); a repeated
  delegation in that retry is served from the turn's result, never a second plan.
  `T-20261107-gov-use-case-attempts-end-with-an-empty`: the front desk budgets against the turn deadline
  (`task.deadline_remaining_ms`, i.e. `execution.chat.chat_turn_timeout_seconds`, default 240 s): each `delegate_to_planner` passes Execute a
  timeout of the time left minus `reply_reserve_s` (25 s); with under `min_delegation_s` (30 s) to give it starts no
  plan (`chat_agent_turn` v4 `no-time-to-delegate`), a plan that hits its timeout returns `delegation-timeout`, and the
  narration/report retries run only with time left, so a long turn ends with a reply instead of none.
  `T-20261107-chat-agent-answers-turkish-government-asks-with`: an exception from `run_think` (provider auth
  failure, lost lease) now fails the turn (SDK INTERNAL -> `humanFailure` status line, nothing stored) instead of
  storing "Sorry — I didn't quite catch that"; the fallback remains for leak hits, empty replies and `ReActLoopError`.
  2026-11-08 (agent:claude-chat-fix): `internal/chat/disclosure.go` is the reply disclosure guard, run in `Turn`
  before Emit: a paragraph that declares redaction/withholding never states a personal identifier (e-mail, IBAN, NINO,
  SSN, card, 11-digit national number, phone), and a customer conversation never gets a strong identifier; values the
  user wrote are kept; masked values get a category label plus one held-back line in the turn language. Names are not
  detected (chat_agent v4 drafting rule). `chat_agent.py`: inside the reply reserve, or after a plan with no time for
  another, the loop ends (`_ReplyNow`) and ONE bounded call writes the reply (`chat_agent_turn` v6 reply-now; fixed
  `_OUT_OF_TIME` text with the results if that call cannot run); the turn's `language` reaches the model by name
  (reply-language); the first delegation carries the latest message verbatim (delegation-scope); a `ReActLoopError`
  returns the drafted answer the SDK now carries (`ReActLoopError.draft`) or a reply over the turn's results, and the
  fallback only when the turn has neither.
  `T-20261107-chat-failure-texts-are-english-only-in-non` (owner decision 2026-11-07: failure and fallback texts follow
  the conversation's language, never model-translated): `internal/chat/locale.go` holds a message catalogue keyed by
  `MessageID` and language (en, tr) and a deterministic detector (distinctive letters + function words) run on the
  latest user message, then earlier user messages, then `execution.chat.chat_default_language` (default `en`).
  `humanFailure(err, lang)` reads it; `buildHandoff` sends the turn's `language` metadata, and `chat_agent.py` picks its
  fallback from `_SAFE_FALLBACKS`. `chat_agent_turn` v5 tells the model to write the time-note replies in the latest
  message's language. A new language = a `languageProfile` + catalogue entries in Go + one `_SAFE_FALLBACKS` entry
  + a `domain/progress.go` `progressPhrasings` entry. `T-20261107-chat-progress-status-line-phases-are-english`: the
  status-line phases render in `ProgressUpdate.Language` (`Text()`, `StillText()`); the chat turn stamps it and
  `app/progress_holder.go` fills it for emitters that lack it (planner step progress) until the turn's final update.
  `T-20261107-delegated-steps-name-the-conversation-owner-in`: `Server.Execute` resolves the run principal once
  (`actingForRun`, `network/acting_for.go`: the same `RunPrincipal` ExecuteTool uses for per_user credentials,
  asked with the step lease's binding shape) and `stepFn` stamps `domain.HandoffKeyActingFor` (`_acting_for`) and
  `_acting_for_name` (ADR-0142 registry display name via `Server.PrincipalNames`) on every step handoff, always
  overwriting or deleting a caller-supplied value. SDK `react-turn` v4 `acting-for` renders it after the step
  objective in the Task region. Not covered: the `routing.bypass_selection` benchmark arm.
- **Effect-checked dispatch: a step runs only on an agent whose tools can do it** (2026-11-06,
  `T-20261106-kernel-does-not-check-that-a-declared`, owner option B, fail-safe). Vocabulary is config,
  `execution.capability.effect_vocabulary` (default read, write, move, delete, execute, egress, send, spend, admin): a
  class term maps an ADR-0086 tool effect class one to one; a refined term (`move`/`delete`/`execute` refine `write`,
  `send` refines `egress`) also needs a tool base-name glob, because the classes cannot tell a move from a write
  (`domain/step_effects.go`, `config.DefaultEffectVocabulary`, validated at load). An agent's effects are DERIVED from
  `ToolExecutor.AvailableTools` (grants x profile ceiling x live registry) at dispatch, cached by tool-set signature
  (`gatekeeper/effects.go` `ToolEffectSource`, wired in `app/effect_check_wiring.go`); a tool with no operator policy
  (inferred effects, or `SystemTool.EffectsDefaulted` for an unlisted MCP tool) contributes nothing. The planner
  (`planner_cap` v3, `replan_cap` v3, `plan_cap_v3.schema.json` + a per-call vocabulary enum) declares
  `Step.RequiredEffects` per action step, seeing the vocabulary and each agent's derived effects in
  `<capability_clusters>`. Gatekeeper L1 admits only agents whose effects cover the step's (hard and soft pins
  included); an unknown effect matches nobody; no eligible agent → `domain.NoEffectMatchError` → SelfHealer stops at
  once → the DAG executor replans ONCE with the reason → a second refusal ends the plan and the server returns a
  `clarification` payload (`effectRefusalClarification`), never an arbitrary agent. A nil `RequiredEffects` (legacy
  plan, non-contract arm) dispatches as before, logged `effect check unverified`. Observability: `Gatekeeper: effect
  check` log per step, `DispatchTask.EffectCheck`, funnel L1 reason, candidate rationale, selection ErrorMsg. A yield
  whose sub-goal cannot be dispatched says so in the step error, and `replan_cap` v3 adds the missing effect. The
  planner's exact-match plan cache now compares the ACTIVE arm's prompt hash (it compared the default arm's).
- **Step failures are classified; capability gaps go to the re-planner** (owner 2026-11-12;
  T-20261112-self-healer-retries-a-step-whose-yield-chain, T-20261112-yield-sub-goal-on-a-composed-lease-runs-under;
  research note `2026-11-12-agent-harness-recovery-practices`). `domain/step_recovery.go`: classes transient /
  deterministic / needs_replan (`domain.ClassOfStepError`, `harness.ClassifyStepFailure` adds gRPC codes). The
  SelfHealer retries only transient and unclassified failures; deterministic (yield `ErrMaxDepth`/`ErrMaxSubgoals`/
  `ErrRepeatedIntent`, dispatch refusals, InvalidArgument/PermissionDenied…) and needs_replan
  (`domain.CapabilityMissingError`, `domain.StepStuckError`) stop at once (`HealingExhaustedError.Class`), and
  needs_replan skips the runner-up fallback. The yield sub-goal budget is per STEP (`yield.WithStepBudget`, set by
  the server before the healer). A worker ends a step with the `capability_missing` outcome (payload type +
  `_capability_*` metadata, the yield's sibling; turned into the error by `dispatch.Dispatcher`); the composer
  refuses a yielded sub-goal on a composed lease whose capability hint the composition does not cover
  (`Composer.subgoalGap`, reading `DispatchTask.CapabilityResolution`). ListTools under a composed lease names
  matching tools outside the step in the `x-tool-outside-step` response header (`Composer.OutsideStep`), never on
  the menu. `domain.StepFailures` (`execution.tools.repeated_failure_stop`, default 3) stops a plan step after
  identical (tool, args, error) failures, per step lease. The replan loop guard accepts a same-words step with
  changed requirements after a capability_missing; under the checklist the next stage's planner gets a
  needs_replan failure as a note and such a stage is not a stall (bounded by `max_replan_attempts`).
  Python half done 2026-11-12: the SDK's `report_capability_missing` verb emits the outcome, find_tools reads
  the header, `agents/worker_agent.py --composed` offers the verb and `prompts/composed_worker` v2 names it.
  The per-run SDK find_tools budget (3) and recurrence veto stay per run (the kernel owns the per-step stop).
- **Config store layer 11**: durable operator writes in bbolt (ADR-0101). Since D8/D9 (2026-11-01) a console write to a key a writable file layer already sets goes INTO that file (format-preserving, atomic; `internal/config/file_layers.go` `ResolveFileSetting`/`WriteFileSetting`, one `persist()` in `app/config_write_source.go` for every write RPC) and the store override is removed; committed `tuning.json` and read-only files are never rewritten — the value is stored and the ack/log names the file it overrides. Boot WARNs once per store key that beats a different file value (`config.StoreOverrideReport`, `app/config_store.go` `warnStoreOverrides`); `orchestrator config overrides list|unset` (`app/config_cli.go`, offline, `list` opens the store read-only) inspects and clears overrides. Generators are stored per id (`llm_provider.generator_overrides.<id>`, tombstone `removed:true`) and merged by id over the file list (`config.MergeGenerators`); a pre-D9 stored whole list is migrated at boot. The console marker / ack fields need a proto bump — T-20261101-config-write-outcome-does-not-carry-d8-written.
- **Contributed machine tools are opt-in per agent**: an agent reaches a user's machine only through an explicit `local:*` (or exact `local:<machine>/<tool>`) grant; ownership picks the machine (ADR-0127 CL-4, `domain/contribution.go` `grantCovers`).
- **`app.KernelServices` is the plugin capability seam**: premium receives functions, not internals — e.g. `VerifyMachineToken` lets the `machine_token_v1` ingress verifier check a token without reading it (ADR-0127 CL-5). `PlanExecutor` (`domain.PlanExecutor`, `app/plan_executor.go`) runs a plugin-generated plan on the real DAG executor via the Orchestrator.Execute authored-plan branch — the premium `start_plan` action uses it (PREM-0032).
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start — and, since ADR-0130, mints each discovered agent's plane credential (`agent:<id>:token`) into the ADR-0101 store.
- **Identity: one principal registry, one person kind, a pluggable Authenticator** (ADR-0142 stage 1, 2026-11-06). Persons live in the core principal registry (`domain.PrincipalRegistry`; `internal/identity` Postgres over migration `0032_principal_registry.sql`, in-memory fallback): deterministic ids (existing persons keep their bare id; local accounts = username; OIDC first login `oidc-<hash(iss,sub)>`), status active/suspended/deprovisioned, (issuer, subject) identities, and `agent:<id>` aliases for audit back-compat; kind `actor` is reserved. A person is `user` on every door: `domain.PersonPrincipal` is the one rule used by the MCP binding hop, worker-machine owners, `VerifyMachineToken` and the chat ingress (an unbound `mcp:<client>` stays an agent); a suspended person is refused at the door (`domain.PersonMayAct`). The operator plane resolves bearers through `domain.Authenticator` (replacing `OperatorIdentity`/`StaticIdentity`, deleted): the OSS default `identity.Chain` = local accounts (argon2id; the env bootstrap operator is re-asserted as a local account each boot) + at most one OIDC issuer (`identity.oidc`, stdlib JOSE, JWKS rotation, `require_provisioned` default), replaceable via `app.Options.NewAuthenticator` / `Registry.SetAuthenticator`. Roles are a set; `operator.RoleFromRoles` derives operator/viewer/member (member = own conversation lane + own accounts only). `DescribeAuth` (unauthenticated) advertises password/OIDC. Act-for's OSS directory is the registry. Offline admin: `orchestrator principal …` (`app/principal_cli.go`). Gaps: console PKCE flow not built (the UI still uses password Login); multi-issuer/SAML/SCIM are premium, later stages.
- **The agent plane authenticates and every listener shares one policy** (ADR-0130, 2026-11-01). Every `Orchestrator` RPC and every premium service declared agent-facing requires the per-agent named secret as `authorization: Bearer` with `x-agent-id` as the assertion it must match (`internal/authz/agent_auth.go`; one constant-time compare, `authz.VerifyNamedSecret`, shared with the MCP endpoint); the principal is established there, never from the header alone. The kernel mints missing credentials at boot and injects `CAMBRIAN_AGENT_TOKEN`/`CAMBRIAN_AGENT_ID` into each spawned agent's environment (`agentmgr/agent_credential_env.go`, the one credential SEC-01 admits). `server.insecure_localhost=true` waives agent auth (D2) — the benchmark harness runs under it — but since ADR-0142 only for loopback/in-process peers (a routable peer still needs the credential), and the flag is a boot error together with `identity.oidc`. Every HTTP listener is built by `app/listener_security.go` (`serveHTTP`; plugins use `app.ServeHTTPListener`): loopback default, routable-bind refusal without TLS, header/idle timeouts, a body cap; `/v1/admin/*` (and `/v1/ingest` when `ingest_token` is empty — an empty token no longer disables auth) takes the operator bearer through the same `domain.Authenticator` as gRPC (`app/admin_auth.go`), and `app/listener_policy_test.go` refuses any other `http.Server` literal. Operator `Login` locks a peer out after 5 failures in 15 min and tokens expire after 12 h (`operator/login_limiter.go`); every attempt is logged and handed to `domain.LoginObserver` plugins (`Registry.AddLoginObserver` — the premium receipts plugin chains failures). The `/healthz` shim is a handler served by `serveHTTP`; only two operator-run tools (`cmd/pagerank-recompute`, premium `cmd/cambrian-runview`) stay on the listener allowlist, each with an ADR citation. Spawned agents dial the address the listener policy bound, and on a pure-TLS bind receive `CAMBRIAN_TLS_CA` (+ `CAMBRIAN_TLS_SERVER_NAME`) beside their token (`app/agent_transport.go`). Both gRPC planes carry a per-principal token bucket after authentication (`internal/ratelimit`, `server.rate_limit`, default 200 req/s burst 400, `ResourceExhausted` on overflow).
- **Launched processes, MCP tool policy and leases fail closed** (ADR-0139 D8 Phase 0, 2026-11-04). One environment allowlist, `internal/procenv`, serves agent spawns (`procenv.Allowlisted`, strict) and MCP stdio servers (`procenv.AllowlistedNamedSecrets`: OS essentials plus the server's `env` list; a credential-shaped name passes only when named, `CAMBRIAN_*`/`LANGFUSE_*` never). A discovered MCP tool with no `tools` entry and no server `default_tool_policy` is Dangerous with effects read+write+egress (`mcp.UnlistedToolPolicy`); `ToolPolicy.Effects` declares effects explicitly. Lease ids are 128-bit crypto/rand hex (`newLeaseID`, `llm_gateway.go`). Every lease resolution goes through `Server.resolveLeaseFor` (`session_identity.go`), which fails closed for agents: an agent-plane caller resolves only a lease bound to itself, so a lease bound to another agent OR to no agent is refused (operator and other non-agent principals are exempt and resolve any live lease; T-20261104-leases-minted-with-no-agentid-are-usable-by-any); `ExecuteTool`, `GenerateViaModelStream` and `GenerateWithTools` refuse such a lease outright. Every site that hands a lease to an agent binds it: plan steps (dispatcher), the bypass arm, retrieval dispatch, verification, chat turns, and plugins via `app.KernelServices.AcquireAgentLLMToken` (mints and binds atomically; `AcquireLLMToken` stays unbound for in-process use such as the reactive start_plan budget). Plan-step leases are minted before selection and bound to the executing agent by the dispatcher immediately before each `CallAgent` (`dispatch.Dispatcher.bindLease` → gateway `BindLeaseAgent`, port `domain.LeaseAgentBinder`); the AgentID moves with a runner-up fallback or yield hand-off. Chat-turn leases are bound to the pool's worker agent. Retrieval spend is charged to a caller's parent lease only when the caller holds it (`callerLease`). The raw `x-session-id` fallback is open only to the operator surface, the MCP task lane and non-agent in-process calls; an agent-plane caller with an unknown value gets no session.
- **Tools come only from MCP servers; the shipped set is curated and pinned** (owner directive `decisions/agent-stack-v2-mcp-tools-and-profiles.md`, 2026-11-05; T-20261105-curate-verify-and-pin-the-mcp-server-set). `configs/mcp.json.example` is the keyless core set, pinned to exact versions (`npx pkg@x.y.z` / `uvx pkg==x.y.z`):
  - fetch, DuckDuckGo search, filesystem, a Docker-backed code sandbox (`llm-sandbox` wrapped by `tools/code_sandbox/server.py`: offline pre-built image, per-run read-only inputs; see `internal/stagedinputs/` above), a shell (`mcp-shell-server`, which replaces the dead super-shell), git, SQLite (Google MCP Toolbox), time, a pure calculator (`mcp-server-calculator`, added 2026-11-05 for exact arithmetic; the assistant profile reaches it and its role says to use it), pdf-reader, markitdown and sequential-thinking.

  `configs/mcp.integrations.json.example` is a NOT-loaded file of keyed entries to copy into `mcp.json` (the console path is the integrations catalog below): GitHub, Brave, Tavily, Exa, SearXNG, Google Workspace, Microsoft 365, Slack, Postgres, Notion, Atlassian, Linear, Playwright and Office authoring.

  `configs/MCP.md` holds the catalog, prerequisites (node 22+, uv, Docker), trust notes and the merge procedure. Policy rules:
  - Servers that can write, send or execute list their read tools explicitly and carry no `default_tool_policy`, so new tools fail closed.
  - Network reads declare `egress`.

  Gaps:
  - Every row is registry-verified only; the live handshake is pending (T-20261105-live-install-and-handshake-verification-of-the).
  - Auth is one static credential per server, with no per-principal OAuth (T-20261105-mcp-auth-is-one-static-credential-per-server-no).
  - stdio servers are not process-sandboxed (T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s); the catalog records each server's trust tier for when they are.
- **stdio MCP servers are contained by trust tier** (ADR-0139 D8, T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s, 2026-11-06). `internal/sandbox` (stdlib + x/sys only): `Policy` (built once at boot by `app/mcp_sandbox.go` from `mcp.sandbox` + the integration catalog) resolves each server's tier — server `trust_tier` → catalog tier via `catalog_tiers` (official→vetted, community→third-party) → plugin servers first-party → `default_tier` third-party — into a `sandbox.Spec` carried on `mcp.ServerConfig.Sandbox` (`mcpServerFromConfig(s, policy)` takes the policy as a required argument; a zero Spec launches at strict third-party). Every stdio launch (connect, Probe, per-user dial, register_mcp) goes through `mcp.(*Connector).stdioTransportFor` → `sandbox.Start`, which plans the tier's wanted controls against the platform (`on_unavailable` refuse|degrade) and runs the native runner (Windows: suspended start into a Job Object with memory/process/CPU-time limits, kill-on-close, a completion-port watcher that kills at the memory limit, and for third-party a low-integrity token with a low-labelled workdir; Linux/Unix: `/bin/sh -c 'ulimit …; exec'`, process group, optional cgroup v2 parent, bubblewrap for ro-root/rw-workdir/no-net, uid 65534 for a root kernel) or the Docker runner. `sandbox.Process` adds the stall kill and the per-message output cap. The report is `Connector.Containment(id)` and is appended to `IntegrationOp.policy_summary`; refusals land in health `last_error`. `ContainedStdioTransport` is the exported, credential-agnostic entry for the per-user path as it moves to premium. Matrix and caveats: `configs/MCP.md` "Sandboxing by trust tier".
- **Integrations: MCP servers are managed from the operator plane** (T-20261106-integrations-manage-mcp-servers-from-the, 2026-11-06). `internal/integrations` is the curated catalog as DATA (`catalog.json`, embedded; 29 entries generated from the two example files, each with launch spec, credential/setting fields, default tool policy, trust tier, runtimes/platforms, optional read-only probe tool), extended by `mcp.catalog_file`; `Load` refuses an entry with no declared policy (or explicit `fail_closed`), a non-dangerous default that writes, a non-secret credential-shaped field or a non-read-only probe. MCP servers are stored PER ID in the ADR-0101 store (`mcp.server_overrides.<id>`, `internal/config/store_mcp.go`, the D9 generator pattern) and merged by id over the file layers' `mcp.servers`; tombstones hide file servers; a contract-0097 whole-list entry is migrated at boot; console writes never rewrite `mcp.json`. `MCPServerConfig` gains `enabled` (nil=true; disabled ⇒ not dialled, not watched, tools pruned), `env_values` (literal non-secret env; credential-shaped and `CAMBRIAN_*`/`LANGFUSE_*` names refused at boot and write, `ValidateMCPServer`), `secret_env` (store `mcp:<id>:env:<NAME>`, injected at connect into that server only via `procenv.ServerEnv`, D5 env-first when passed through), `catalog_id`/`catalog_version`/`display_name`/`category`; args `"${NAME}"` take an `env_values` value. `mcp.Connector` keeps per-server health (stopped/starting/ready/error, last handshake, tools, redacted last error, restarts; generation-guarded), published as `domain.MCPServerHealthEvent` → `OperatorEvent.integration_health`; `StopServer` (disable) vs `RemoveServer`; `TestServer` (live session or ephemeral launch, per-tool policy report, probe only when non-dangerous read/egress). `app/integrations.go` (`integrationManager`) implements the ten operator RPCs over `configWriteSource.saveMCPEntry`/`removeMCPEntry`/`setMCPSecret`/`revertMCPEntryToFile` (`RevertIntegrationToFile`, 2026-11-06: drops the store override or tombstone of a file-declared server and re-applies the file entry live; secrets kept), the same path the contract-0097 `SaveMCPServer`/`RemoveMCPServer`/`SetMCPServerToken` RPCs now use. `BoltConfigStore.SecretUpdatedAt` (bucket `secret_meta`). Capability `integrations`, contract still 0101. Stdio sandboxing is the next-but-one bullet; per-principal credentials are the next bullet (ADR-0140). **Health liveness** (`internal/infrastructure/mcp/liveness.go`, T-20261104-mcp-health-ping-drops-pdf-reader-and-fetch): go-sdk's `Ping` omits the SEP-2575 per-request `_meta` that 2026-07-28 (stateless) servers require, and in that protocol `ping` is not a method at all; the watch loop therefore sends ping with `_meta` (`pingParams`) and counts ANY JSON-RPC answer, error included, as alive (`classifyPing`); a closed connection drops at once; silence drops only after `pingTimeoutsBeforeDrop`=2 consecutive timeouts (serial stdio servers busy with a long call). `ListMCPServers.tool_count` counts by `mcp.ParseToolName` (`app.countMCPTools`; it was always 0).
- **Per-user integration credentials (ADR-0140, proposed) — account layer moved to PREMIUM by ADR-0141 (proposed; T-20261106-external-accounts-and-consumers-one-outbound, 2026-11-06).** Core keeps: the server credential model `MCPServerConfig.user_credential` `{scope shared|per_user|either, provider, scopes, inject env|bearer|header, name, shared_fallback}` (validated in `ValidateMCPServer`; catalog: microsoft-365, slack, github, github-write `per_user`, google-workspace shared); the port `domain.CredentialSource` (`domain/credential_source.go`: `Credential(ctx, CredentialRequest{ServerID, PrincipalID, Purpose})`, `ConnectedPrincipals`, `ErrCredentialNotConnected`/`ErrCredentialReconnectNeeded`, `ServerCredentialModel`, `SealedRecordStore`); per-call selection: `ExecuteTool` stamps `domain.WithRunPrincipal` from the lease via the D14 resolver (`network.Server.RunPrincipal`), operator `ExecuteToolOp` runs for the operator; credential DELIVERY in `internal/infrastructure/mcp/peruser.go` (`Connector.SessionForCall`: per_user ⇒ a session per (server, principal), http per-request token round tripper, stdio per-principal sandboxed process relaunched after a refresh, lazy/idle-closed/LRU-capped by `mcp.user_sessions.{max,idle_seconds}`; no principal/account ⇒ `NotConnectedError`, never another credential; `either` falls back to shared only with `shared_fallback`; health `awaiting_user` until the first connected principal discovers tools). The provider seam: `app/credential_provider.go` `Registry.SetCredentialSource` / `Options.NewCredentialSource` + `CredentialServices` (live server models, sealed records namespace `accounts` via `BoltConfigStore.SealedRecords`, named-secret read, `CredentialChanged`, late-bound `RecordAudit`/`RunMutation` = `operator.Service.RecordAudit`/`RunPluginMutation`, `Caller` = `operator.CallerFromContext`), built in `startKernelServices` before plugin lifecycles and MCP Watch. With no provider (OSS) per_user servers fail closed; shared servers use `mcp:<id>:token` / `mcp:<id>:env:*` as before. Store: bucket `sealed_records` (keys `<namespace>/<key>`); the ADR-0140 bucket `user_credentials` is migrated into `accounts/` on open and deleted. The AccountsAdmin plane RPC names are role-gated in `internal/substrate/operator/authz.go` (full-method match). Removed from core: `internal/usercreds`, `app/user_credentials.go`, `internal/substrate/operator/integration_accounts.go`, six OperatorConsole RPCs + capability `integrations-per-user`, config `mcp.oauth`. **ADR-0141 phases 2/3/5 (2026-11-06, accepted):** the port also serves ORG credentials — `CredentialRequest` gains `Slot` (`token` | `env:<NAME>` | an ingress binding name) and `AccountID` (`<provider>:org:<x>` or `owner:<provider>`), purpose `ingress`, sentinels `ErrCredentialRevoked` / `ErrCredentialPendingConfirmation` / `ErrCredentialNotPermitted`, optional `domain.OrgCredentialWriter` (Set/Clear/Import/Status) and `domain.PrincipalIdentities`. When the installed source implements `OrgCredentialWriter`, `mcp.SetOrgCredentials` makes shared credentials resolve env → org account and the consumer-named store entries are no longer read (phase 5); `app/shared_credentials.go` routes `SetMCPServerToken` / `SetIntegrationSecret` / `InstallIntegration` secrets / the clears through it (a clear also deletes the legacy copy). `CredentialServices` gains `StoreNamedSecret`/`ClearNamedSecret`/`NamedSecretStatus`/`NamedSecretUpdatedAt` (provider-client writer), `LegacySharedCredentials` (boot migration input), `PrincipalIdentities` (from the registered principal directory), `SharedCredentialChanged` (bounce a shared server); `KernelServices` gains `ResolveCredential` and `OrgCredentials` (late-bound, for plugin consumers such as ingress pollers). `NotConnectedError.Pending`. **Org OAuth install + grant scopes + system catalog (2026-11-06, additive):** `CredentialRequest.Scopes` (a consumer's declared scopes, e.g. an ingress binding's) and sentinel `ErrCredentialInsufficientScope` (an account not granted the consumer's declared scopes; `peruser.go` maps it to `NotConnectedError{NeedsConsent}`, never a fallback); `ServerCredentialModel.CatalogID` and `CredentialServices.Servers` (every installed server's model, for premium's `ListSystems`); `ListAccountConsumers` is Operator-only in `authz.go`. OSS unchanged. `CredentialServices.RecordAudit` queues (≤1024) entries recorded before the operator plane is bound and flushes them on bind, so the provider's boot migration and legacy cleanup are audited (they were dropped before). `SetMCPServerToken`/`ClearMCPServerToken` are marked Deprecated in `operator.proto` (aliases of the org account on premium; removal is an owner decision). Gaps: device-code flow, persisted tool list for never-connected per-user servers; legacy `mcp:<id>:*` values on premium kernels are deleted by premium's release-gated cleanup one release after migration.
- **Every LLM call is metered by one ledger** (ADR-0131 D1-D3, 2026-11-01). `llm.SpendMeter` (`internal/infrastructure/llm/spend_meter.go`) is fed by the two doors every call passes: the outermost decorator in `Provider.Acquire` (all organs) and `SubstrateLLMGateway` (all agent streams and tool turns, including the `retrieval_agent` chain, whose lease is now bound to the caller's session and charged back to the asking step's lease). Usage is provider-reported — adapters fill a ctx capture cell (`usage_capture.go`) from their own response — and `len/4` is used only as a fallback flagged `Estimated`. `execution.session.max_session_tokens` (> 0) refuses calls with `llm.ErrSpendCeiling`; the plan ends with gRPC `ResourceExhausted` and a `System_SpendCeiling` TaskEvent. Wired in `internal/kernel` `ProvideServer`. Each Execute response carries `_llm_*` spend metadata, router classification included. Attribution is a property of the call's ctx on the same meter (owner decision 2026-11-01): a session in ctx is booked and enforced; `domain.WithBackgroundSpend` + the originating session (the verifier call via `VerificationRequest.SessionID`) is booked, never refused — for gateway calls that property rides the lease: a lease `Acquire`d under the marker is `BudgetLeaseState.Background`, admitted without the ceiling check and booked to its bound session, which is how each verifier call's lease (minted by `VerificationWorker` through the constructor-injected gateway, bound to the originating session, completed after the call) is metered; Execute routes under the caller's existing live session (`liveCallerSession`, never creates one) or on an `llm.WithSpendTab` tab settled into the session it opens; anything else lands in the reported, unenforced kernel bucket `llm.NoSessionBucket` (`kernel:no-session`, logged by the gateway's eviction sweep). Step TaskEvents bill the step lease's provider usage (`llm.StampStepUsage` → `domain.HandoffKeyStep*` → DAG executor), with `TaskEvent.UsageEstimated` flagging the `len/4` fallback. Lease completion runs one gateway hook (`SetCompleteHook`), which clears ADR-0046 run grants for every lease site. One plan gets `1 + max_replan_attempts` planner calls in total (validation retries and replans share the budget, `plan_call_budget.go`).
- **One LLM request contract, one constructor** (ADR-0139 C1/D1 Phase 1 wave 1a, 2026-11-04, T-20261104-llm-transport-has-no-system-role-or-message). `domain.RequestGenerator` (`domain/llm_request.go`) takes a structured `GenerateRequest` (system blocks with cache marks, messages, tools, tool choice, response schema, options, cache TTL, `<provider>:<field>` extensions) and returns text, tool calls, a normalised stop reason, usage (input/output/cache read/cache write) and `Ignored[]`. Each generator declares per-field `Capabilities` (enforced / best_effort / unsupported), overridable per generator in `llm_provider.generators[].request_capabilities`; an unhonoured field is reported in `Ignored`, and is `domain.ErrUnsupported` only under `Options.Strict` / `ResponseSchema.Strict`. The adapters are on the official SDKs (anthropic-sdk-go v1.84.0, openai-go/v3 v3.74.0, genai v1.77.0); Ollama goes through its `/v1` endpoint and the OpenAI adapter (the hand-written `/api/generate` client is gone; `OllamaEmbedder` is unchanged). `llm.NewClient(GeneratorConfig)` is the ONE constructor (the Provider registry and `ProbeGenerator` use it; `config.ModelConfig` was deleted; the gateway streamers and the `ProviderRegistry` stub were deleted in wave 1c). The legacy ports (`Generate`, `GenerateWithTools`, `GenerateStream`) are shims over `GenerateRequest`. Every Provider decorator forwards the C1 port; `schemaGenerator` (outermost, `schema.go`) validates a best-effort response schema with santhosh-tekuri/jsonschema/v6 and repairs once, then fails with `domain.ErrSchemaValidation`; purpose/model handles fall back to the next healthy generator on 429/5xx/transport errors and on 401/403 after one same-generator retry, never on a refusal, a schema failure or a 4xx. SDK retries are off; `sharedLLMTransport` is the only retry layer. Anthropic caching: breakpoint on the last cache-marked system block plus the last three messages (max 4), TTL 5m/1h from `CacheTTL`, opt-in. Not yet done: the premium Langfuse wrapper does not forward the C1 port (C1 calls are then untraced, logged once).
- **The agent plane is on C1; one pathway, one breaker** (ADR-0139 D1 Phase 1 wave 1c, 2026-11-05, T-20261104-llm-gateway-silently-drops-generateoptions-sdk). `cambrian.proto` (additive): `GenerateStreamRequest` gains `system` (5, `SystemBlock{text, cache}`), `messages` (6), `response_schema` (7, `ResponseSchema{name, schema_json, strict}`), `cache_ttl` (8, enum `CacheTTL`); `GenerateWithToolsRequest` gains `system` (6), `response_schema` (7), `cache_ttl` (8); `GenerateOptions.max_tokens`/`temperature` became `optional` and gained `strict` (4); `GenerateChunk` (final chunk) and `GenerateWithToolsResponse` gain `ignored`, `usage` (`GenerateUsage`, with cache read/write and `cache_write_known`), `stop_reason`, `model`. `prompt` is deprecated on both requests; a `"system"` role message is refused (system text has one way in). `network/gateway_request.go` builds ONE `domain.GenerateRequest` for both RPCs and maps errors (`FailedPrecondition` = no native tools only; `InvalidArgument` = strict field unsupported; `Aborted` = schema failure after repair). `SubstrateLLMGateway` serves every call through `network.StepModels`, implemented by `llm.Provider.GenerateOn`/`StreamOn`: the step chain (winner → fallbacks → default, generator ids) on the Provider's own decorator stack (breaker, tracing, concurrency cap, schema layer) minus the spend meter, which stays lease-scoped in the gateway (admission, `RecordSessionTally`), with the D1 fallback policy (transport/429/5xx only). The gateway's second health cache (hardcoded 30 s cooldown), its client table (`RegisterModelClient`/`SetClientFactory`), `llm.NewStreamersFromGenerators`, `llm.ProviderRegistry` and `Server.ModelRouter` are deleted; a live generator reload needs no gateway rewiring. A Pass-1 budget cut now cancels and drains the stream (the Provider's concurrency slot is held until the stream ends). The ADR-0079 exchange lane now also captures `GenerateWithTools` turns (rendered system/messages/tool calls). Test double: `internal/testing/fakemodels`.
- **Lifecycle hooks narrow, never widen** (ADR-0139 D11 / C5, 2026-11-05, T-20261105-adr-0139-d11-lifecycle-hooks-pre-post-tool-pre). `domain.Hook` (`domain/hooks.go`) gets a deep-copied `HookEvent` (principal chain, agent, tool + args + effects or a model-call summary, lease, run id, current verdict) and returns a `HookResult`: narrow (`ask`/`deny` + reason), rewrite input (`pre_tool` args / `pre_model` messages) or output (`post_tool` result / `post_model` text and final-answer deliverable) only when the spec is `may_rewrite`, and annotations. `domain.HookChain` runs hooks in (priority, name) order on the caller goroutine with a ctx timeout (a late answer is discarded), folds them so one hook's narrowing feeds the next, and is the only place the rule lives: a verdict moves only allow → ask → deny, a looser answer is ignored and logged `widen_ignored`; an error/timeout/panic/malformed result follows `on_failure` (default `block` at pre_*/*_start, `allow` at post_*/*_end). `ToolExecutor.Execute` runs `pre_tool` AFTER grant/resource/scope/effects (now one `authorize` function, `domain/hook_tool.go`) and re-runs `authorize` on a rewritten call, so a rewrite cannot reach an ungranted path; a hook `ask` goes to the approval controller (nil ⇒ deny; under `tools_auto_approve` it is approved); `post_tool` may redact or withhold the output (withheld outputs are not recorded to memory). `pre_model`/`post_model` run at the `llm.Provider` entry points (purpose/model handles incl. legacy `Generate`/`GenerateWithTools`, and `GenerateOn`/`StreamOn`), outside the fallback walk; a stream a `post_model` hook's scope matches (`HookChain.HasFor` with the purpose label and the context's agent, decided before the call) is served BUFFERED (`Provider.bufferedStream`/`bufferedPromptStream`: consumed internally, post_model on the complete reply, the rewritten text delivered as one chunk + the C1 final chunk, nothing on a deny or a broken stream; this covers the SDK text protocol via `GenerateViaModelStream`, T-20261106-text-protocol-agent-replies-never-reach-post), any other stream fires `pre_model` only; `post_model` sees the whole reply — text, tool calls (`HookEvent.ToolCalls`, read-only) and the native lane's deliverable (`HookEvent.FinalAnswer`: the `execution.step_output.final_answer_tool`/`_arg` call's string argument, `llm.WithFinalAnswer`), which a `may_rewrite` hook rewrites (`HookResult.FinalAnswer`) and `HookChain` writes back into the call's JSON args (`domain/hook_reply.go`); `redact_output` redacts both; an unreadable deliverable (non-object args, missing/non-string arg, two final-answer calls) makes every `may_rewrite` post_model hook deny the reply whatever its `on_failure` (T-20261106-post-model-hooks-never-see-native-tool-call); generalised to GUARDED ARGUMENTS (2026-11-06, T-20261106-post-model-hooks-do-not-guard-control-verb): `execution.hooks.guarded_arguments` (default `yield_subgoal.intent`, `delegate_to_planner.task`, `propose_memory_note.content`, `propose_skill.skill_md`; `llm.WithGuardedArgs`) plus the final answer appear as `HookEvent.GuardedArgs` and are rewritten via `HookResult.GuardedArgs` through the same write-back (`FinalAnswer` kept for back-compat), `redact_output` covers them, an unreadable one (non-object args, present non-string value) fails closed; other calls' args are left to `pre_tool`; hooks carry a static SCOPE (2026-11-06, T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or): `HookSpec.Agents` (every point; never matches an event with no agent) and `HookSpec.Purposes` (model points; labels router/planner/verifier/memory/facts/thought/agent_step/model; `thought` since 2026-11-06 = in-kernel thought-step synthesis, `domain.PurposeThought`, previously labelled planner — a deliverables scope is `[agent_step, thought]`; routing unchanged: with no `llm_provider.roles.thought` it resolves through the planner's role, `llm.RoleFallbacks`; the role is assignable from the console; T-20261106-thought-steps-call-the-model-under-the-planner), config `entries[].agents`/`purposes`, checked by `HookSpec.Matches` before a hook runs (non-matching = skipped, not recorded); on the model path ask = deny (`domain.ErrHookDenied`, never a fallback trigger); pre_model annotations join the request as a trailing system block. The chain is built once in `bootstrapKernel` (`hooks.NewChain`: `execution.hooks` + `Options.Hooks`) and constructor-injected into the Provider (`llm.WithHooks`) and the ToolExecutor (`Hooks`). Fire today: pre/post_tool and pre/post_model (`subagent_start`/`subagent_end` were removed with spawn_subagent, ADR-0145 D14); `pre/post_compact`, `run_start/end` are declared for D3/D7. Every invocation is slog-logged and handed to `Options.HookObserver` / `Registry.AddHookObserver`.
- **Per-request LLM call reporting** (T-20261104-benchmark-runs-record-no-kernel-llm-calls-per, 2026-11-04). The agent-plane `IngestMemory` and `QueryMemory` handlers open a `domain.LLMUsageTally` on ctx (like `domain.StageClock`) and return it as the `cambrian-llm-usage` gRPC trailer (`internal/substrate/network/llm_usage_trailer.go`; diagnostic, no proto change): `calls#,input#,output#,cache_read#,estimated#,cost_usd` for the request plus the same keys prefixed `<purpose>.`. The tally is fed ONLY by `SpendMeter` (`Record` reads it from ctx; `RecordSessionTally` takes it from the gateway), and a lease captures the tally of the ctx it was `Acquire`d in (`BudgetLeaseState.Tally`), so the `retrieval_agent` chain's calls (purpose `agent_step:retrieval_agent`) count to the QueryMemory that minted the lease. The trailer is always sent (`calls#=0` is an observed zero); calls finishing after the reply (async KG lane) are not in it. Fact extraction and assessment are both purpose `facts` today. The operator plane's memory RPCs send no such trailer yet.
- **Operator contract 0100** (2026-11-04, ADR-0138 Amendment B1, additive; `SetHandshake`): `duplicate_of` /
  `version` on `IngestMemoryOpResponse`, the `upload_name` / `upload_tags` replace-by-name lookup on
  `ListDocumentsOpRequest`, `updated_at_unix_ms` / `version` on `DocumentSummaryOp`; capability `upload-replace`.
  Vendored to `ui/proto/operator.proto` (UI pins 0100); benchmark `_stubs` not yet regenerated (`T-20261104-benchmark-operator-stubs-predate-contract-0100`). ADR-0137's
  erasure surface, which planned 0100, takes the next number.
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
  per document (DEBUG; INFO `(SLOW)` over 1 s or `(FAILED)`) with `<stage>_ms` for evidence, chunk,
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
  three times during kernel boot). Root cause and live numbers: ADR-0053 observations/measurements. (The backfill's
  `exec: no command` WARNs for `llm:*` model agents are gone: TraitModel agents skip the backfill,
  2026-11-04, T-20261102-interview-backfill-tries-to-boot-llm-model.)
- **The LLM agent interview is DELETED** (owner decision 2026-11-05, ADR-0139 Q3 applied ahead of D5;
  T-20261105-delete-the-llm-agent-interview-now-owner). `internal/metabolism/interview` (worker, examiner, question
  generator, HybridJudge/inline judge, scenario runner, unexaminable backoff), the evaluation-session set and the
  ToolExecutor's eval deny branch, the kernel-side `RequestProposal` caller (`Transport.RequestProposalFrom`,
  `domain.ProposalRequester`), the `interview` LLM purpose/role, `domain.ProfileHistory` and the release-notes merit
  decay are gone. `internal/metabolism/registration.ProfileSeeder` makes an agent routable at registration: the
  profile vector is a deterministic embedding of the agent's Description (A2A: Agent Card description + skills when a
  CardFetcher is wired; fallback: capabilities, then name), and the profile is the NEUTRAL prior SR=Trust=0.5 (tool,
  daemon and system agents: 1.0, verified by default; a same-hash re-seed keeps earned outcomes; a changed agent
  starts neutral, nothing inherited). The Gatekeeper scores a still-provisional agent (registered, not yet seeded) on
  the neutral prior too (the flat 0.1 `DefaultProvisionalScore` is gone) and it keeps the L2 bypass, so every agent
  is routable from boot even with the embedder down; the ×0.6 profile cold-start penalty is gone. Retired keys:
  `execution.agents.disable_interviews`, `execution.gatekeeper.cold_start_penalty_multiplier`, the
  `llm_provider.roles.interview` role (dropped at load with a log line, `llm.RetiredRoles`). Proto: `AgentReadyOp.interview_ms`
  is deprecated (never filled), `AgentService.RequestProposal`/`Orchestrator.RequestProposal` deprecated in comments,
  nothing removed. Phase 3 still owes D5 qualification (probe suites) and L2 calibration (D6).
- **Routing learns from step outcomes** (ADR-0139 Phase 0, 2026-11-04).
  The Dispatcher stamps the dispatched definition on the response (`domain.HandoffKeyWinnerSourceHash`), so step
  TaskEvents carry `SourceHash` and aggregate into the profile the Gatekeeper reads. With no verified event the
  aggregator's SR is the neutral 0.5 (was 1.0 per unverified event). Missing latency takes the slate's mean latency
  term (was the full w3). (The 2026-11-04 interview fixes, `unexaminable` backoff and evaluation-session denial, went
  with the interview on 2026-11-05.) Gaps: yield/fallback outcomes still unstamped
  (T-20261104-yield-resume-and-runner-up-fallback-step); pre-fix orphan rows (T-20261104-orphan-profile-agent-rows-from-pre-fix-step).
- **Outcome-based merit, ADR-0139 D6 (2026-11-05, `execution.gatekeeper.merit_mode`, default `posterior_lcb` by owner decision 2026-11-05; `legacy` stays selectable).**
  Step latency: the Dispatcher times the winner's `CallAgent` and stamps `domain.HandoffKeyWinnerLatencyMs`; the
  DAGExecutor books it as `TaskEvent.ComputationLatencyMs` (wall time; 0 = unknown) and the aggregator medians only
  known samples. Evidence: the aggregator writes `domain.OutcomeEvidence` (verified success/failure, fractional by
  verifier score; unverified count) on `AgentProfile.Outcomes` and per capability on `CapabilityStat.Outcomes`; the
  prior (`merit_prior_alpha/beta`, 1/1) and `merit_unverified_weight` (0) apply at scoring time. Modes
  (`internal/supervision/gatekeeper`): `legacy` = unchanged hand weights / learned scorer; `posterior_lcb` = Beta
  quantile `merit_lcb_quantile` (0.1) over the step's capability evidence (none ⇒ prior, never the global record),
  ties by cost then latency (missing = slate mean); `thompson` = one posterior draw per candidate per dispatch
  (seedable `Gatekeeper.Rand`), and the Dispatcher's epsilon exploration is skipped. TraitModel sub-selection stays
  legacy. `gatekeeper.ScoreSlate` + `MeritRanksBefore` are shared by FindCandidates and PreviewRoute (arms
  `hand_weights`/`learned_scorer`/`posterior_lcb`/`thompson_mean`); boot logs `Gatekeeper: merit mode`, per-candidate
  scores at debug. Promotion needs the A/B/A study `T-20261105-a-b-a-routing-study-legacy-merit-vs-posterior`.
- **Stage-A recency weight is 0 by default** (owner decision 2026-11-03; benchmark longmemeval-s-temporal-ku: weight 0 = 195/211 vs 0.1 = 188-190, 0.3 = 164). The term still exists and is configurable (`blend_weight_recency`, `blend_recency_mode`).
- **Stage-A recency curve is selectable** (2026-11-02, ADR-0054 Amendment A1, proposed):
  `execution.retrieval.blend_recency_mode` = `minmax` (default, unchanged: pool-relative) | `decay`
  (`2^(-age/blend_recency_half_life_hours)`, default 2160 h; age from `reference_time`, else from the newest
  candidate). `QueryService.SetRecencyCurve` (`internal/memory/query.go`), wired in `app/app.go` next to
  `EnableBlend`; the boot line "Stage-A multi-signal blend ENABLED" logs `recency_mode`. Min-max evicted the
  oldest relevant session on LongMemEval gpt4_f420262c; measured by temporal-ku arms g/e/f before any default
  change.
- **Ranking v2 Phase 0** (2026-11-03, ADR-0136 D1 + D3, proposed; both switches default OFF, gate pending).
  `memory.RankingV2` (`internal/memory/ranking_v2.go`), installed by `QueryService.SetRankingV2` in `app/app.go`,
  which logs `ranking v2: stage_a_calibrated=<b> temporal_intent_enabled=<b>` at boot.
  - D1 `execution.retrieval.stage_a_calibrated` (+ `lexical_saturation_k`, default 1.5 — was 0.1, which made any
    one query word worth half the lexical credit and lowered gate evidence recall; see ADR-0136 observations;
    `logCalibratedFeatures` logs per-query feature means at DEBUG): `applyStageABlend` hands off
    to `applyCalibratedStageA` / `calibratedFeatures` — pool-independent features: lexical = the adapter's
    `ts_rank_cd` (now carried through `rrfFuse` as `domain.SearchResult.LexicalRaw`) saturated s/(s+k); coherence =
    raw IDF seed overlap (`chunkCoherenceRaw`) over a fixed cap, seeds ranked without RRF rank; PageRank = interim
    x·N/(x·N+1) from `PageRankMeta`; recency = decay from `reference_time` only, and only when D3 asks for an order.
  - D2 (a per-document cap, `execution.retrieval.max_per_document`) was **rejected by the owner 2026-11-03 and
    removed**: once it bound it lowered gate evidence recall (A1r 0.966 → D2r 0.9266; TK-Ar 0.9749 → TK-Cr 0.9253,
    benchmark `adr0136-gate`). `assembleLanes` is back to its pre-D2 shape and `reassertDocumentCap` is gone. A
    config that still sets the key has it ignored as an unknown key (no warning). MMR is noted in ADR-0136 as a
    possible future soft-diversity option, not built.
  - D3 `execution.retrieval.temporal_intent_enabled` (+ `temporal_fit_weight`, default 0.2): `memory.TemporalIntent`
    (`temporal_intent.go`, go-dateparser v1.2.4 behind a mutex — the library is not concurrency-safe) reads a target
    band, an order and `explicit` from the query against `reference_time`. Band → temporal-fit feature exp(-(Δ/σ)²);
    order → recency direction (needs `blend_weight_recency` > 0 to matter); an absolute range → hard window via
    `SearchOptions.Since/Until` when the caller sent none. Query-inactive features leave the combination entirely,
    so a query with no time phrase ranks as with the flag off. Fingerprint lines only when a switch is on.
- **Document identity and version chain** (2026-11-04, ADR-0138, proposed; on by default). The natural key of an
  ingested document is (writer, source key); the writer is the kernel-stamped `ExternalDocument.Author`. v2 ids
  (`internal/memory/document_identity.go` `documentIdentityOf`): a `source_document` name verbatim; tag / thread /
  unnamed ids `<group>:<16 hex of sha256(writer, Body[, Data])>`; a named SourceURI `<uri>#w=<writer hash>`; a
  writer-less text ingest keeps its legacy id. Binaries digest `Data`; a bare `scheme://` URI is not a source. Resolved
  ONCE per ingest (`resolveDocumentID`), with a grandfathering lookup: a stored legacy id written by the same writer is
  reused, never re-keyed. `execution.ingestion.document_identity` = `v2` (default) | `legacy` (old ids, WARN).
  Before any write, `DocumentLifecycleStore.ReviseDocument` (`postgres.PgDocumentLifecycle`, composed in
  `NewMemoryStack` from `FactSubstrate.Documents`) refuses another writer of a held id
  (`domain.ErrDocumentOwnedByAnotherWriter` → `PermissionDenied`) and, when the content digest changed, retires the
  stored version in one transaction (`retireDocumentsTx`: memory_fact items via `EraseItemsTx`, assessments, markers,
  extraction journal, triplets, PageRank, edges, sections, chunks) and records a content-free `document_versions` row.
  Migration 0026 (`documents.written_by/content_digest/version`, `document_versions`).
  **Amendment B1** (2026-11-04, owner "go B1, per writer"; contract 0100): an operator upload
  (`ExternalDocument.UploadName`, set only by `app/operator_ingest.go` from the filename) is rule U:
  `operator_ingest://<normalised name>[#s=<scope hash>]#w=<writer hash>`. The name is normalised as last path
  segment, NFC, whitespace folded, lower-case; the scope is the request tags minus markers, sorted. A changed
  re-upload is a new VERSION through the same `ReviseDocument`. An exact re-upload (same writer + revision digest,
  any session/tag/name) writes nothing: `FindByContent` before evidence capture, plus a `RecordAssociation` row in
  `document_associations` (migration 0027; FK cascade; never changes tags or access). Index
  `idx_documents_writer_digest` is built concurrently. `IngestionManager.MatchUpload` answers the operator
  `ListDocuments` `upload_name` lookup with the ingest's own resolution (capability `upload-replace`);
  `IngestMemoryOpResponse` carries `duplicate_of` / `version` via `domain.IngestOutcome` on ctx. Off under
  `document_identity=legacy`. Near-duplicates (B1.4) are not built.
- **Fact layer on the knowledge substrate** (2026-11-03, ADR-0136 D4 + D5 as amended by A1 and A2, proposed; every switch
  default OFF, gate pending; the memory_facts build of the same day is retired). Block `execution.facts`
  (`config.FactsConfig`): `extract_enabled`, `lane_enabled`, `supersession_enabled`, `assessments_enabled` + tunables
  (`queue_size` 1024, `workers` 2, `llm_timeout_ms` 120000 (base) + `llm_timeout_per_kchar_ms` 2500 per 1000 prompt chars, capped at
  `llm_timeout_max_ms` 300000, `parse_retries` 1 (same-prompt re-ask of an unreadable answer; `internal/memory/fact_llm_call.go`), `max_facts_per_document` 64 (per window), `max_prompt_chars`
  48000 (per window), `max_calls_per_document` 8 (windows per source per run; 0 = no cap), `max_facts_per_source` 512,
  `lane_top_k` 20, `lane_scoped_documents` 0 (A4, off) / `lane_scoped_top_k` 20, `fact_match_weight` 0.2, `current_fact_weight` 0.1, `single_attributes` [] / `multi_attributes` []
  = DECLARED profiles, `fuzzy_candidates` 10, `cardinality_min_votes` 3); `execution.ingestion.sync_ingest_wait_for_facts`
  (+ `_timeout_ms` 120000). Boot line `fact layer: extract_enabled=<b> lane_enabled=<b> supersession_enabled=<b>
  assessments_enabled=<b> store=<substrate|none> lane_scoped_documents=<n> lane_scoped_top_k=<k>` (effective values; n is 0
  unless the lane acts), plus `SYNC_INGEST_WAIT_FOR_FACTS: ENABLED`.
  - Data: a fact is one append-only `memory_fact` knowledge item (core-declared kind, `domain.MemoryFactKindSpec`,
    appended in `app.go` before `NewKindRegistry`); entity id `<scope_key>/<subject_key>/<predicate_key>`, actor = the
    writing principal (chunk `source_agent_id`, else the ctx principal), id `ki_fact:<sha256(evidence|chunk|subject|
    predicate|value)>`, evidence id threaded from `ProcessSync`. Its chain is DERIVED by the pure, order-independent
    `domain.ValidTimeSupersessionAuthority` (an `InputResolutionAuthority`) from the key's items + stored assessments +
    the predicate profile; `resolutions` versions the answer (with `valid_to`).
  - Schema: `0021_drop_memory_facts.sql` (drops memory_facts / memory_fact_extractions; 0020 kept), `0022_fact_items.sql`
    (`resolutions.valid_to`, `idx_statement_values_entity`, `knowledge_assessments` — keyed judge verdicts,
    `predicate_profiles` — versioned learned/declared cardinality, `knowledge_item_embeddings` — item projection,
    `knowledge_extractions` — re-ingest marker), `0024_extraction_versions.sql` (A2.4: `extractor_version`
    `<prompt id>/<generator model>`, `passages_covered`/`passages_total`; 0023 is reserved for ADR-0137 erasure).
    Per-dimension partial HNSW `idx_knowledge_item_embeddings_hnsw_d<dim>` built by `migrate.ensureConcurrentIndexes`.
    After this binary boots on a database, older binaries refuse it.
  - Chokepoints (composed ONLY in `kernel.NewMemoryStack`, which takes the raw substrate as `kernel.FactSubstrate`):
    `authz.EnforcingKnowledgeWriter` (ClassifyWrite per item, memory_fact re-keyed on the final tags) and
    `authz.EnforcingFactItemReader` (fail closed) over `postgres.PgKnowledgeStore` / `postgres.PgFactItemReader` (scope IN
    SQL on the item classification). The plugin seam `KernelServices.Knowledge` is `authz.ReservedKindGuard` (no
    memory_fact through the raw seam).
  - Ingest: `memory.FactExtractor` on its own worker pool (Start/Stop, goleak), items + embeddings through the writer.
    A2 (source-agnostic, `decisions/ingestion-prompts-are-source-agnostic.md`): the prompt (`fact-extract-v3` since A3,
    `fact_prompt.go`) names no source kind; per-source data arrives as `domain.SourceMeta` (type, title, author, reference
    time + basis event|authored|ingested from the document's `TimeSource`, `domain.SourceMetaOf` / the lister) and only an
    event/authored time dates "now" facts or bounds a cue; guarded by `TestFactPrompt_SourceAgnostic`. A2.3 sectioned extraction: chunks over 6000 chars are split at
    sentence/whitespace boundaries, passages packed in order into windows of `max_prompt_chars`, one call per window up
    to `max_calls_per_document`; the marker records coverage after every window, and a partial, legacy or other-version
    marker is PENDING (backfill resumes at the first uncovered window). The prompt carries `supersedes_prior` cues and
    `speaker`. With assessments: at most ONE batched judge call per window over unknown-cardinality exact-key pairs and fuzzy same-subject candidates
    asserted earlier; each verdict stored as a keyed assessment first (a replay reuses it, no call); exact-key verdicts
    vote on the profile (learned single/multi at `cardinality_min_votes`, declared always wins).
  - Backfill concurrency (`T-20261103-fact-backfill-processes-each-principal-s`): `execution.facts.backfill_group_concurrency`
    (default 1 = the serial ordered backfill; `cmd/facts-backfill -group-concurrency N` overrides) runs one ordered group in
    two phases (`internal/memory/fact_backfill_staged.go`): extraction and judge calls N at a time, then the assessments,
    votes and markers committed window by window in event-time order, re-planned against the live store with the
    not-yet-written items hidden (`domain.FactStagedReader`: `FactsByID`, `SimilarFactsHiding`) — the serial run's final
    items/assessments/resolutions/profiles. Uncommitted windows are journaled in `knowledge_extraction_windows`
    (migration 0025; `domain.ExtractionWindowJournal`, deleted on commit and by facts-reset), so a stop between phases
    resumes with no extraction call. `SimilarFacts` now orders ties by item id. Live ingest is unchanged.
  - A3 (agent-plane source metadata): `IngestMemoryRequest.source_kind`/`title`/`author` (9-11) and
    `MemoryPart.speaker` (3) are DESCRIPTIVE. `ingest_memory.go` trims them, refuses control characters
    (INVALID_ARGUMENT) and cuts them to `execution.ingestion.source_kind_max_chars` (64) / `source_title_max_chars` (256) /
    `source_author_max_chars` (128) / `part_speaker_max_chars` (64) (0 = default). They land on
    `ExternalDocument.SourceKind` / `Title` / `AuthorLabel` and `DocumentPart.Speaker`; `SourceType` ("ingest_memory") and
    the stamped `Author` (= `source_agent_id`) stay kernel-owned. `SourceMetaOf` / `FactBackfillCandidate.SourceMeta`:
    type = SourceKind else SourceType, author = AuthorLabel else Author. SourceKind/AuthorLabel persist in
    `documents.metadata` (`source_kind`, `author_label`; no migration). The body join is unchanged (byte-identical);
    `domain.ChunkSpeakers` maps the ADR-0134 chunk spans onto the part spans and writes chunk metadata
    `speaker_at_start` / `speakers`; the extractor renders `[i] (speaker at start: X) …` from the stored chunk, so live
    ingest and backfill see the same input.
  - Query: unchanged lane (`fact_lane.go`, `FactReader` port) over the substrate reader; `fact_match` / `current_fact`,
    `fact_matches` metadata, history/earliest chains. A4 (experimental, default off): with `lane_scoped_documents` N > 0
    the lane runs a second search restricted to the facts of the first N distinct source documents (chunk metadata
    `document_id`) of the fused dense+lexical pool, via `domain.FactSearchOptions.SourceDocumentIDs` through the same
    enforcing reader; `PgFactItemReader.scopedSearchSQL` ranks those documents' facts EXACTLY (not HNSW) using the
    concurrent companion index `idx_statement_values_source_document` (`migrate.go`; no migration file). Scoped hits are
    merged into the global ones by fact id and reach their chunks by the same enforced `GetBatch`.
  - Backfill: `memory.FactBackfill` (`MemoryStack.FactBackfill`) + `cmd/facts-backfill` (lister
    `PgKnowledgeStore.ListFactBackfillCandidates`, told the running extractor version).
  - Reset (A2-Q3): `cmd/facts-reset` (`-namespace` principal prefix, `-principal`, `-all`, `-dry-run`, `-keep-learned`) →
    `MemoryStack.FactReset` → `PgKnowledgeStore.ResetFacts`: one transaction deleting the selected principals' items,
    their resolutions, citing assessments, their documents' markers and (default) learned profiles, then re-deriving
    surviving keys. A hard reset for re-runs, NOT ADR-0137 erasure (no sentinels).
  - Erasure: `PgKnowledgeStore.EraseItems` deletes assessments citing erased items and leaves targeted content-free
    sentinels so a closed value stays closed (ADR-0137 Q5).
- **Agentic retrieval is OFF by default** (owner decision 2026-11-02,
  `cambrian-knowledge/decisions/agentic-retrieval-default-off.md`): `execution.retrieval.agentic_retrieval_enabled`
  defaults to false (`DefaultConfig`, `internal/config/config.go`); every recall is one plain pass unless a deployment
  opts in. `agentic_decompose_enabled` stays true so opting in restores decomposition. Re-deciding it needs a
  multi-hop dataset, not LongMemEval (ADR-0135 gate: off 7/10 at p50 0.58 s vs adaptive 8/10 at 5.7 s).
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

## Agent inventory and the D12 migration (ADR-0145, 2026-11-08)

Every agent that exists today, classified for D12 ("workers are one ReAct runtime the kernel
composes with skills and tools; separate agents only for encapsulated technical jobs"):
**(a)** fits the ReAct runtime as skills + tools (becomes a *profession*: a profile's skills and
tool allowlist, composed per step), **(b)** a code or service agent for encapsulated technical work
(stays an agent, callable through the kernel), **(c)** other.

| Agent | Where | Class | Why |
| --- | --- | --- | --- |
| `assistant`, `researcher`, `data-analyst` profiles | `agents/profiles/*.yaml` | (a) | Already skills + MCP tool allowlists over one runtime; their `role` lines carry no procedure that is not in a skill or the composed-worker rules (arithmetic → "exact work is done by tools"; move-not-rewrite → `organise-files`; cite/data-not-instructions → `web-research`; numbers-from-code, caveats → `exploratory-analysis`/`chart-report`), so nothing had to move. |
| `worker_agent.py` | `agents/` | (a) runtime | The ReAct runtime itself; `--composed` is the D12 runtime. |
| `chat_agent.py` | `agents/` | (c) | The conversation front desk: turn-level control code (delegation, deadlines, reply guards, language), not a plan-step worker. Its D3 fix is SDK-side (`T-20261108-chat-agent-pass-delegation-tool-and-turn`). |
| `docling_agent` | `agents/system/` | (b) | Document-structure parsing (docling), kernel-invoked on ingest. |
| `kg_extractor_agent` | `agents/system/` | (b) | Deterministic, no-LLM triplet extraction on ingest. |
| `reranker_agent` | `agents/system/` | (b) | Cross-encoder scoring service on the recall path. |
| `retrieval_agent` | `agents/system/` | (b) | Fixed typed LLM ops of the Go-driven agentic retrieval loop; an organ, not a profession. |
| `source_discovery_agent` | premium `plugins/ingressstudio/agents/` | (b), candidate for (a) | Own Python tools and typed action proposals, kernel-invoked (System). Its procedure could become a skill once its tools are kernel tools (`T-20261108-premium-source-discovery-agent-move-its`). |
| `airline_chat_agent` (+ `airline_chat_ingress`) | premium `agents/` | (c) | A domain chat front desk for one benchmark domain; the ingress is a transport. |
| `telegram_ingress_agent` | premium `plugins/telegram/agents/` | (c) | Ingress transport. |
| `tool_tutorial_agent`, `pulse_agent`, `example_ingress_agent` | `sdk/examples`, `sdk/example-agents` | (c) | SDK examples: `@tool` tutorial, daemon demo, ingress demo. |
| Planner, requirement deriver, verifier, memory/fact organs | Go, `internal/` | (c) | Kernel organs that call models directly, not agents. |

Migration: with `execution.workers.composed` on, every plan step whose selected executor is a
class-(a) profile runs on the composed runtime; class (b) keeps being dispatched to its agent.
The legacy agent-per-step path is deleted when the flag defaults on
(`T-20261108-retire-agent-per-step-dispatch-once-composed`).

---

## Known gaps

- **Upstream results of a dependent plan step (fixed 2026-11-06, T-20261105-a-planner-thought-step-that-depends-on-two).** One manifest, `domain.StepDependency` (`domain/step_dependency.go`: step, objective, status `completed|empty|unavailable`, cid, bytes, content, shortened), built per dispatch by `executer.buildStepDependencies` (`internal/metabolism/executer/dependencies.go`) from `DependsOn` and carried in the handoff metadata under `_step_dependencies` for both workspace modes and both executors (SDK agent, in-kernel thought step). Content is bounded fair-share by `execution.content` (since 2026-11-12: a token budget sized from the receiving worker, `dependency_budget_fraction` x `worker_prompt_budget_tokens`, 16000 tokens = 64000 bytes by default; the old `plan.dependency_results_max_chars` is migrated to the optional byte ceiling `content.dependency_max_bytes`), and a shortened result keeps its beginning AND end (`tail`), the omitted byte range, an outline of its headings with ledger_get pages and the exact read call (`read`), never a head-only cut (T-20261112-summarise-a-document-step-gets-only-the-first). The thought step renders it (`thought_synthesis.go`, prompt `thought_synthesis` v2; the old "payload is the prompt" branch that hid every result since 2026-07 is gone); the step cache keys dependent steps on the full upstream results; an empty step is recorded as empty (no stale `step_N_result` after a replan); an empty fan-out reports its node as an empty dependency. Since 2026-11-06 (`T-20261106-an-injected-injectcorrection-step-cannot-see`) a replan (InjectCorrection, error replan, hot-swap) captures the swapped-out plan's completed and failed steps (`priorPlanResults` in `dependencies.go`, `StepDependency.PriorPlan`/`Error`, status `failed`) and hands them, bounded, to every replacement-plan step with no in-plan dependency, labelled `prior_step_N`; the step cache keys on them. Open: operator narrowing via an explicit `depends_on` on InjectCorrection (`T-20261106-injectcorrection-has-no-way-to-name-which`, needs a proto change).

- ADR-0145 D14 request guards (2026-11-09; default off, unit-tested only, not replayed or run live): the write record and the refusal record are in memory (a restart forgets them); a target is the EXACT normalised value set of the first matching key (a mail to [a,b] and a later one to [a] are different targets); a tool whose arguments carry none of the configured keys has no target (only D9 idempotency and the duplicate notice apply); the validator's written-target rule matches a target value in a writing step's query text; a nested worker delegation of a non-conversation run gets its own write-record scope (its own root run). D14.6 (2026-11-09): the constraint exemption is per class on the step answering the write item (a step that sends the requested drafts may also edit them; "send only to Ali" is not argument-checked); a document is satisfied by any non-empty `answer_text` (presence, not adequacy); the missing-source check matches heading labels textually and is unmeasured live; coverage-stage checkpoints fail the run FK (T-20261109-checkpoints-of-a-coverage-stage-run-fail-the); nested-run WorkspaceStage enrichment has no principal (T-20261109-nested-worker-delegated-run-s-workspacestage).

Each has a ticket; close the ticket and update this list in the same change.

- Content budget (2026-11-12; unit-tested only, not run live): no component knows a model's context window and token estimates are len/4 (base64 under-counted 2-3x) — `T-20261112-pre-send-context-window-check-per-model-with`; the dependency outline lists markdown (ATX) headings only; a shortened dependency's page numbers assume the ledger pages the stored body as text (a JSON body is re-marshalled by `ledger_get`, so pages can shift); `answer_ref` is offered on the plain final answer only, not inside a D6 envelope.
- Run ledger (ADR-0145 D1, 2026-11-08; default off): prior-plan results handed to a replanned plan's root steps are still the captured masterContext strings, not ledger
  reads; the chat agent reaches `ledger_get`/`ledger_find` only with `execution.chat.ledger_tools` (its own @tools
  over `ExecuteTool`; the staged planner reads the ledger since 2026-11-08); `ledger_find`'s text filter covers preview, tool, arguments and the
  first 16 KiB of a text body only. The legacy content-store path is deleted once the ledger defaults on
  (`T-20261108-delete-the-legacy-plan-scoped-content-store`).
- ADR-0145 phase 3 chat side (2026-11-08; default off, unit-tested only, not replayed or run live;
  `T-20261108-measure-the-chat-reply-check-and-grounded`): the reply check does not match rules or articles in free
  text, counts a bare integer below `reply_check_min_integer` (10) as unchecked, and can tell a stated gap only by its
  `[R#]` tag (an untagged gap is `unverified`, not a violation); its recorded-reply fixtures use synthetic evidence (the
  runs kept no tool results). The two English-only guards it and the SDK re-prompt replace are skipped, not yet
  deleted (`T-20261108-delete-the-english-only-chat-guards-once-the`).
- ADR-0145 D12 composed workers (2026-11-08; default off, not yet run on a live kernel or replayed): tools with resource
  arguments are left out of compositions (`T-20261108-composed-workers-carry-no-tool-resource-policy`); the runtime
  principal has no premium scope of its own (`T-20261108-premium-authz-the-composed-worker-runtime`); the professions'
  model preference is not applied (`T-20261108-composed-worker-does-not-apply-the-professions`); the skill index is
  capped by name, not relevance (`T-20261108-composed-skill-index-is-all-profession-skills`). D3 evidence re-prompt
  (`execution.workers.evidence_reprompt`, default off) needs the chat agent to name its delegation tool
  (`T-20261108-chat-agent-pass-delegation-tool-and-turn`).
- ADR-0145 phase 2 (2026-11-08; default off, not yet run on a live kernel or replayed): the validator approximates "a read of the same system"
  as "a read upstream" and relies on the tool executor's approval gate for the approval path; the envelope repair turn
  re-runs the whole step (the first attempt's lease usage is not billed to the step's TaskEvent; since D14 it is handed
  the step's earlier calls as `_repair_prior_calls` and a write the step executed is refused in it); the idempotency
  cache is in memory (a restart forgets it); `Hippocampus.Store` is dead
  (`T-20261108-hippocampus-store-has-no-production-caller`).

- ADR-0145 D11/D4/D5a (2026-11-08; default off, not yet run on a live kernel or replayed): an ask-back answer starts a NEW Execute (a new checklist from the answer turn); resuming from the recorded `InputRequired` state by its ledger ref is not built (`T-20261108-ask-back-answer-does-not-resume-the-recorded`); escalating a question to the operator approval queue on non-conversation paths (D5a.3) is not built; the checklist deriver does not ask back itself (only the planner does); the planner's ledger index lists previews only, the planner must `read` for bodies.

- Knowledge case modelling (2026-11-07): the plugin seam still stores classification verbatim, reads unfenced, and lets
  a plugin assert as any NON-designated actor (`T-20261103-kernelservices-knowledge-hands-plugins-the-raw`); designated
  actors cannot be roles (authorities are pure — use the approvals policy + `approved_via_commit`); no app/person-scoped
  commit surface yet (`RequestCommit` is in-process only); an agent-proposed link may cite evidence the agent cannot
  read (`T-20261107-propose-knowledge-link-proposals-do-not-check`).

- Effect-checked dispatch (2026-11-06): the per-step effect check is not on the operator proto, so the console's plan
  view shows it only as text (funnel reason, rationale, selection error) — `T-20261106-effect-check-result-is-not-on-the-operator`.
  Refined effects rest on tool NAME globs in config; a tool named unlike its action needs a glob added. Yield
  sub-goals carry no required effects (they dispatch "unverified"). The default (non-contract) planner arm emits no
  effects, so its steps are never effect-checked.

- ADR-0139 D13 (skill lifecycle, 2026-11-05): kernel side only — no console surface yet
  (`T-20261105-operator-console-has-no-skill-approval-queue`), no SDK `propose_skill` verb yet (D3 owner); hub
  skills are not signature-verified because no hub source exists (`T-20261105-hub-skills-are-not-signature-verified-no-hub`);
  the curator's pipeline/schedule exemption has no reference source and chat-turn loads never earn success_count
  (`T-20261105-skill-curator-has-no-pipeline-schedule`). Skills registered over the wire (operator RegisterSkill) are
  served but not lifecycle-managed.
- Per-agent skill access (2026-11-06): no console screen for grants or the skill_access approval kind yet (`T-20261106-operator-console-has-no-skill-grants-screen`); approvals show in the existing approvals pane as tool `skill:<name>`. Non-profile agents are not bounded by grants.

- Live step-output stream (ADR-0139 amendment 2026-11-06): no
  console screen renders the stream — `T-20261106-console-renders-no-live-step-output-although`; A2A streaming
  (outward channel and producer) waits for the A2A task protocol — `T-20261105-kernel-speaks-no-a2a-1-0-task-protocol-no`;
  Gemini streams function calls whole, so its final answer is not live (text deltas are).
- D11 `post_model` (2026-11-06): on the SDK's TEXT protocol the whole reply text is the post_model subject (served
  buffered when a hook matches); guarded control-verb arguments exist only on the native lane, and the gateway's
  Pass-1 budget cut cannot stop a buffered generation early (max_tokens bounds it). No per-agent switch forces the
  text protocol, so the E2E covers only the native lane's redaction. No built-in deny-pattern kind attaches to `post_model`
  (`deny_args_regex` is pre_tool only; a `command` hook can deny on `final_answer` / `guarded_args`).

- ADR-0139 D4 (A2A cards, 2026-11-05): the kernel serves signed cards but no A2A 1.0 task protocol, and the outbound
  A2A client still posts the pre-1.0 `/tasks` shape — `T-20261105-kernel-speaks-no-a2a-1-0-task-protocol-no`; remote
  agents (`a2a.endpoints`, trust tier third-party) register with no manifest, so card skills are not routable
  capabilities — `T-20261105-remote-a2a-agents-register-with-no-manifest-so`; cards are signed when served, not at D5
  qualification — `T-20261105-agent-cards-are-signed-at-serve-time-not-at-d5`. The third-party tier is a label until
  D8 containment lands (`T-20261105-profile-trust-tier-is-stored-but-not-mapped-to`).

- ADR-0139 D6: the posterior merit modes are unmeasured (A/B/A study not run, needs the owner's cost go-ahead —
  `T-20261105-a-b-a-routing-study-legacy-merit-vs-posterior`); the PreviewRoute wire has no outcome-evidence fields, so
  the preview reads capability SR x sample_count as evidence and the global posterior is the prior —
  `T-20261105-previewroute-wire-carries-no-outcome-evidence`. With step latency now recorded, the ADR-0001 D5 per-step
  deadline (`max(median x 2 + 15 s, step_timeout_floor_ms)`) applies to agents that have a latency profile.

- Agent stack v2: a profile's `trust_tier` is stored but kernel-spawned agents are not yet launched through `internal/sandbox`
  (every profile agent runs with the env allowlist only; boot WARNs) — `T-20261106-kernel-spawned-agents-are-not-contained-by`;
- ADR-0139 D8 sandbox gaps: no native network deny on Windows (`T-20261106-windows-appcontainer-runner-for-native-network`), declared egress hosts not filtered (`T-20261106-egress-host-allowlist-declared-by-mcp-servers`), containment only as a string on the operator plane (`T-20261106-integrationop-has-no-structured-containment`), live catalog/docker check pending (`T-20261106-live-verification-catalog-stdio-servers-under`);
  profile allowlists cannot name contributed `local:` tools, so the ceiling filters them out —
  `T-20261105-profile-tool-allowlists-cannot-name-contributed`; the planner/replan prompts' hard-pin example still
  names the deleted `terminal_agent` — `T-20261105-planner-and-replan-prompts-agent-pin-example`

- ADR-0139 D2: the router, planner, replan and clarifier (wave 2a) and the memory organs (facts, triplets) and thought-step synthesis (wave 2b) render through `prompts/` and send their response schema; the rewritten wave-2a prompt texts have no real-model benchmark yet (`T-20261105-router-planner-replan-and-clarifier-prompts`); other organs migrate in their own waves — `T-20261104-no-structured-outputs-anywhere-every-kernel`. The retrieval_agent prompts (wave 2c) are unmeasured until an A/B/A run (`T-20261105-a-b-a-retrieval-run-for-the-retrieval-agent`), and the customer tools-only chat posture is enforced only by the chat worker, not server-side (`T-20261105-kernel-does-not-enforce-the-customer-tools-only`). Per-prompt golden sets are not yet registered as benchmarks; the structured triplet format needs an A/B/A run before it can be the default (`T-20261105-a-b-a-retrieval-run-for-the-structured-llm`), and the moved fact prompts make every stored extraction pending for facts-backfill (`T-20261105-fact-extractor-moved-to-the-structured-library`).

- ADR-0139 D11: hook invocations are logged and offered to `HookObserver`, but no durable event log or journal
  subscribes yet (D7) — `T-20261105-hook-invocations-reach-only-the-slog-log-and`; tool-hook annotations never reach the
  agent (no response field) — `T-20261105-pre-tool-and-post-tool-hook-annotations-are`; the compaction and run
  points are declared but fire only when D3/D7 call them.

- ADR-0139 D10: the checkpoint gate is gone (validator, `DAGExecutor` gate, `domain.CheckpointValidator`,
  `SemanticCheckpointError`, replan CHECKPOINT FAILURE branch), but `checkpoint_after` still feeds the
  (default-off) ADR-0100 D4 cheapest-competent dispatch branch, whose "verified step" rationale was that gate; keep
  it off until re-grounded on the VerificationWorker — `T-20261104-cheapest-competent-dispatch-branch-is-keyed-on`
- ADR-0138: an in-flight KG/fact job of a retired document version can write onto the reused chunk ids —
  `T-20261104-async-kg-triplet-and-fact-jobs-of-a-retired`; operator re-uploads with a session are new documents, not
  versions — `T-20261104-operator-upload-cannot-replace-a-document-a-re`; undated facts of different uploads close each
  other in upload order — `T-20261104-undated-facts-from-different-uploads-close-each`

- Scene removal (2026-11-05) changed planner enrichment (no scene priming, no `<PrecedentLTM>`) without a benchmark run;
  the A/B/A check needs the owner's cost go-ahead — `T-20261105-a-b-a-planner-retrieval-check-for-the-removed`.
  The `e2e`-tagged memory tests do not compile (pre-existing) — `T-20261105-e2e-tagged-memory-tests-do-not-compile-stale`
- Three `cmd/` maintenance tools write through the raw vector adapter —
  `T-20261031-three-cmd-maintenance-tools-write-through-the`
- `nightly` and `release-gate` are not run by CI — `T-20261031-nightly-and-release-gate-checks-are-not-run-by`
- ADR-0061 A2 journal: a lone producer appends ~8% slower than the pre-A2 layout (owner to accept) —
  `T-20261103-adr-0061-a2-journal-single-producer-append-is-8`
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
- ADR-0136 A1: principal-initiated erasure (forget) is unbuilt — ADR-0137's RPCs, authz, tombstones and fences
  (`T-20261103-build-a-principal-initiated-memory-erasure-path`); its substrate part (`EraseItemsTx`, `EvidenceIDs`) and
  the document cascade exist since ADR-0138;
  F2a vs F2b needs two facts-on ingestions (`T-20261103-adr-0136-f2a-vs-f2b-cannot-be-told-apart-from`); out-of-order
  sessions are not assessed against newer facts (`T-20261103-fact-assessments-skip-sessions-ingested-out-of`); learned
  aliases are not recorded (`T-20261103-learned-predicate-aliases-same-predicate`); the plugin knowledge seam is still
  raw for other kinds (`T-20261103-kernelservices-knowledge-hands-plugins-the-raw`);
  QueryMemory returns the fact text as hit metadata, but no reader prompt renders it yet
- ADR-0136 D1's PageRank feature is an interim corpus-relative scale, not the global percentile the ADR specifies —
  `T-20261103-adr-0136-d1-pagerank-feature-is-an-interim-x-n`; Stage-A coherence sums over a Go map, so scores vary
  in the last bit run to run — `T-20261103-chunkcoherence-float-sum-depends-on-go-map`
- ADR-0134: relative time phrases in a question ("last month") are resolved only behind ADR-0136 D3's
  `temporal_intent_enabled` (default off, as a soft temporal-fit feature, never a window; unmeasured) —
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
- ContentStore GC is live-plan aware since 2026-11-03: `domain.PlanScratchTracker` (BeginPlan/EndPlan, optional
  capability on `domain.ContentStore`, implemented by `BBoltContentStore`) means a plan ending evicts only plan-scoped
  scratch (step_result, tool_result) no still-live plan can need, by write-sequence watermark; after the last live plan
  ends all scratch is gone. `DAGExecutor.ExecuteFrom` leases it; stores without it fall back to `GC(keep)` (ADR-0022
  observation) — `T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently`; folding it into the port —
  `T-20261103-domain-planscratchtracker-is-an-optional`
