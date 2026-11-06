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
| `internal/substrate/` | gRPC server, session, OperatorConsole plane, knowledge-graph extractor. |
| `internal/supervision/` | Gatekeeper, verifier pool, watcher (gates SignalStream; in OSS plans each accepted signal once and queues the plan for operator approval on the ApprovalHub, running it via `domain.PlanExecutor` only if approved). |
| `internal/authz/` | Access-control enforcement (ADR-0085). Fail-closed; contains no policy. |
| `internal/infrastructure/` | Adapters: `postgres/`, `llm/`, `mcp/` (outbound), `mcpserve/` (inbound MCP endpoint). |
| `internal/reactive/` | Reactive rule engine plug surface (premium provides the implementation). |
| `internal/chat/` | OSS turn path: pool-dispatched, retry-safe, planner-free. |
| `internal/storage/` | BBolt adapter + durable reactive journal (ADR-0061). Since A2 (2026-11-03) the journal is `reactive_journal_v2/<stream>/<seq>` (16-byte header + signal JSON; a legacy flat `reactive_journal` is migrated on open), pruned per stream, with idempotency pruning and offline compaction before open (`bbolt_compact.go`; `storage.compact_min_bytes` / `storage.compact_free_ratio`; `cmd/bbolt-compact`). Gate: `bbolt_reactive_gate_test.go` (`CAMBRIAN_JOURNAL_GATE=1`), benchmark `reactive-journal-a2-gate`. |
| `internal/config/` | Koanf 12-layer loader (ADR-0024). |
| `internal/memorynotes/` | ADR-0139 D14 principal memory notes: one markdown document per (`personal`, principal) and (`org`, org id), sections = `## ` headings. `Service` (the only read/write path for both planes): `Snapshot` (frozen per run key = lease RunID else lease, bounded in-process cache), `Propose` (append / replace_section / remove_section, etag CAS), operator `List/Get/Set/Delete/SetBudget`. Budget in code points (config default < scope-default row < document row), over budget refused never trimmed; lowered budget serves the snapshot cut with `truncated`. Injection scan = `internal/contentscan` (any non-clean verdict refuses; operator override audited). C8 = owner rule + `domain.Authorizer` (resource `memory:memory-notes:<scope>:<owner>`); unreadable == NOT_FOUND. D12 children (`LeaseBinding.ParentLeaseID`) are SCOPE_DENIED unless `MemoryWriteGranted`. Stores: `PgStore` (migration 0030: `memory_notes`, `memory_notes_budgets`, `memory_notes_history`; deletes keep the row so revisions never restart) and `MemStore` (no-DB fallback). RPCs: `network/memory_notes_rpc.go`, `operator/memory_notes.go`; composed in `app/memory_notes.go` (run principal = conversation owner → task beneficiary → `execution.memory_notes.default_principal`). Capability `memory-notes`, contract 0101. |
| `internal/substrate/network/subagent.go` | ADR-0139 D12 in-agent subagents (2026-11-05, T-20261105-adr-0139-d12-in-agent-subagents-via-spawn). `Server.SpawnSubagent` + `SubagentService` (per-ROOT-lease trees: concurrency slots, tree-wide tool-round and token accumulators, idempotency by (root lease, client_spawn_id); per-child-lease toolset ceiling and counters). Identity and depth come from the lease: `resolveLeaseFor`, then the `LeaseBinding.ParentLeaseID` chain (a request cannot state depth). Toolset resolved against the parent's menu + run overlay + own lease ceiling; any entry outside it is DENIED, `*` = parent's tools minus `subagent_withheld_tools`, memory-write names grant `MemoryWriteGranted` only if the parent holds it. Child lease = `Acquire` + `BindLease` (session/run/step/conversation copied, AgentID = child, ParentLeaseID = parent); usage rolls up to the parent on completion (`RollUpLease`), so up to the root. Enforcement: `ToolExecutor.LeaseCeiling` (grantFor / ConferSkillGrants / AvailableToolsNamedFor; ListTools filters by the caller's lease), ExecuteTool counts rounds (`AdmitToolCall`), gateway `SetLeaseGate` refuses calls past token_budget / tree cap (`ErrSubagentBudget`). Runs through `s.Dispatcher` (CallAgent for self/named agent, Execute for a capability) under `domain.WithDedicatedInstance`, which `agentplane.Transport` serves on a fresh instance (`InstanceManager.BootDedicatedInstance`, not indexed, evicted after) because agents are single-threaded. `subagent_start` (can deny) / `subagent_end` hooks fire. output_schema validated by `llm.ValidateJSONReply` with one repair re-dispatch. Config `execution.agents.subagent_*`. |
| `internal/stepoutput/` | The live step-output stream (ADR-0139 amendment 2026-11-06, superseding ADR-0047 D12's token tier; T-20261106-native-react-path-emits-no-live-token-events). `Stream`: ONE stream per step keyed by the lease; producers `EmitGateway` (both model lanes, via `network.gatewayForwarder`: text deltas + the final-answer argument decoded by `Extractor`) and `EmitAgent` (agent-plane `EmitStepOutput`: lease-bound, seq dedupe, `final`); kernel `seq` per lease; caps `execution.step_output.max_chunk_bytes`/`max_step_bytes`; sinks fixed at construction (the operator feed, `TokenChunkEvent`, attached late via `app/step_output_wiring.go`) and per-session subscribers (MCP `notifications/progress`). Constructor-injected policy: `streams_output` agents and any `post_model` hook whose static scope MATCHES the step (`HookChain.HasFor(post_model, agent, "agent_step")`, header and lease-bound agent) suppress gateway deltas (fail-closed; an unscoped hook matches every step, a planner-only one none). No goroutines. The native lane streams internally through `llm.Provider.GenerateOnObserved` / `SubstrateLLMGateway.GenerateWithToolsObserved` (RPC stays unary); adapters emit `domain.ToolCallDelta` (Anthropic, OpenAI-compatible; Gemini none). `Server.TokenSink` is gone. Capability `step-output-stream`, contract still 0101. |
| `internal/hooks/` | ADR-0139 D11 lifecycle hooks built from `execution.hooks`: built-ins `deny_args_regex`, `require_approval`, `redact_output`, and the `command` hook (argv, JSON event on stdin / JSON `HookResult` on stdout, ctx timeout kills it, `procenv.Allowlisted` env). `NewChain` merges them with plugin hooks (`app.Registry.AddHook`). The port and runner are `domain/hooks.go` + `domain/hook_chain.go`. |
| `internal/migrate/` | Pure-Go DB migration runner (ADR-0064). Boot goes through `migrate.Boot` with the `database.migrations` guard (2026-11-04): `mode` `auto` (default) / `refuse_pending` / `off`, `protected_dbnames` (default empty; a listed database with any pending migration or concurrent index refuses to boot, read-only, before any write, naming the pending changes), `allow_pending` (one-boot opt-in, `CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true`). `migrate up` is the unguarded deliberate path. Applied migrations and index builds log at WARN with the database name. |
| `internal/agentplane/` | Agent-plane gRPC connection pool (renamed from auctioneer by ADR-0100 P3). |
| `internal/agentpool/` | Bounded pool of interchangeable daemon workers (ADR-0084 D4). |
| `internal/tool/` | System-tool lifecycle (Python, `ToolRegistry`, confined `ProcessHandler`). |
| `internal/skill/discovery/` + `skills/` | System skills (ADR-0046, format ADR-0139 C3): `skills/<name>/SKILL.md` discovered from the cwd-relative `skills/` dir at boot, indexed `DocTypeSkill`, served by `ListSkills`. Ships `file-report` (2026-11-04) and, with agent stack v2 (2026-11-05), `summarise`, `web-research`, `exploratory-analysis` and `chart-report` (the starter profiles' skills); `skills/README.md` covers the frontmatter and how a deployment points at the dir. The Docker image does not copy it yet (T-20261104-docker-image-does-not-ship-cambrian-core-skills). |
| `internal/skill/lifecycle/` | ADR-0139 D13 skill lifecycle (2026-11-05). `Service`: `Propose` (agent plane; never active until approved; trust stamped `agent-proposed`; id `sp-<hash(name, base_version, content)>` so an identical re-proposal is the same row; refusals INVALID_FORMAT / NAME_TAKEN / LOCKED / SCAN_DANGEROUS / QUOTA / DENIED; dangerous proposals are not stored), `Decide` / `SetLock` / `Rollback` (operator; idempotent on command_id at the ledger; stale `base_version` and dangerous content are FAILED_PRECONDITION; rollback restores content+version+state, keeps the lock, and is a new entry), `RecordUse` / `RecordRunOutcome` (use_skill Tier-2 loads in `network/skills_rpc.go`; success credited once per run after `DAGExecutor.ExecuteFrom`), `Curate` + `Curator` worker (deterministic, no LLM: active →stale →archived on idleness; pinned exempt; default manages `agent-proposed` only). Approved agent skills live in Postgres and are materialised into the SAME `InMemorySkillRegistry` + `DocTypeSkill` index file discovery fills (`SyncBuiltins`, `Materialize`) — never written into `skills/`. Shipped skills get a `builtin` record (version, usage, ledger) but are served from disk and locked to agents. Store: `postgres.PgSkillLifecycleStore` (migration 0029: `skill_proposals`, `skill_versions`, `skill_records` — not `skills`, which is 0007's descriptor index — `skill_ledger` append-only by trigger, `skill_run_uses`). RPCs: `network/skills_lifecycle_rpc.go` (ProposeSkill; source run/agent from the lease, principal from the D14 resolver), `operator/skills_lifecycle.go` + ListSkills lifecycle fields; composed in `app/skill_lifecycle_wiring.go`. Config `execution.skills`. Capability `skills-lifecycle`, contract 0101. |
| `internal/skill/access/` | Per-agent skill access (owner decision 2026-11-05, `decisions/skill-access-grants.md`; amends ADR-0139 D13). A PROFILE agent's effective skill set = profile `skills` + durable grants keyed by agent id; `network/skills_rpc.go` `ListSkills` enforces it (no body outside the set, fail closed; full menu = the set; find_skills shows other visible skills as `requestable`, Tier-1). `Service.Request`: use_skill outside the set raises an `ApprovalRequest{Kind: skill_access}` on the SAME operator hub as dangerous tools (`toolApproval`, never `toolApprovalCtrl`), waits `execution.skills.access_request_timeout_seconds`; approved ⇒ grant + body in the same call, no ruling (`ApprovalDecision.Decided=false`) ⇒ pending (request row stays open), denied ⇒ denied. `access_auto_approve` grants without asking. `GrantProposer` is the lifecycle's `ProposalGranter` (approve ⇒ proposer grant). Store `postgres.PgSkillGrantStore` (migration 0031: `skill_grants`, `skill_access_requests`, append-only `skill_grant_events`). Operator: `operator/skill_grants.go` ListSkillGrants / GrantSkill / RevokeSkillGrant, capability `skill-grants`. Wired in `app/skill_access_wiring.go`. Non-profile agents are unbounded (mirrors `ProfileToolCeiling`). |
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
    partial plan or returns `DeadlineExceeded`. It replaced the fixed `plan_timeout_ms` (120 s), which is now a deprecated
    key: an explicit value loads as `plan_max_duration_ms` (per layer, `deprecatedExecutionKey` in
    `internal/config/execution_migrate.go`).
  - Repeated timeouts are never reported as "loop detected".
  - The Dispatcher stamps `DispatchTask.Deadline` with the deadline the step actually runs under. It used to be a
    hardcoded `now + 20 s` that nothing read.
  - The chat stall watchdog's probe is `chat.LatestActivity(progress, gateway.ConversationActivity)`. A model call in
    flight on the turn's lease (`BudgetLeaseState.InFlightCalls`) counts as liveness, so one long unary generation is not
    a stall. A turn with no call in flight and no progress for `DefaultStallTimeout` (90 s) is still cancelled.
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
- **Config store layer 11**: durable operator writes in bbolt (ADR-0101). Since D8/D9 (2026-11-01) a console write to a key a writable file layer already sets goes INTO that file (format-preserving, atomic; `internal/config/file_layers.go` `ResolveFileSetting`/`WriteFileSetting`, one `persist()` in `app/config_write_source.go` for every write RPC) and the store override is removed; committed `tuning.json` and read-only files are never rewritten — the value is stored and the ack/log names the file it overrides. Boot WARNs once per store key that beats a different file value (`config.StoreOverrideReport`, `app/config_store.go` `warnStoreOverrides`); `orchestrator config overrides list|unset` (`app/config_cli.go`, offline, `list` opens the store read-only) inspects and clears overrides. Generators are stored per id (`llm_provider.generator_overrides.<id>`, tombstone `removed:true`) and merged by id over the file list (`config.MergeGenerators`); a pre-D9 stored whole list is migrated at boot. The console marker / ack fields need a proto bump — T-20261101-config-write-outcome-does-not-carry-d8-written.
- **Contributed machine tools are opt-in per agent**: an agent reaches a user's machine only through an explicit `local:*` (or exact `local:<machine>/<tool>`) grant; ownership picks the machine (ADR-0127 CL-4, `domain/contribution.go` `grantCovers`).
- **`app.KernelServices` is the plugin capability seam**: premium receives functions, not internals — e.g. `VerifyMachineToken` lets the `machine_token_v1` ingress verifier check a token without reading it (ADR-0127 CL-5). `PlanExecutor` (`domain.PlanExecutor`, `app/plan_executor.go`) runs a plugin-generated plan on the real DAG executor via the Orchestrator.Execute authored-plan branch — the premium `start_plan` action uses it (PREM-0032).
- **Self-installing**: `setup` bootstraps Postgres, Python runtime, agents, config, migrations, detached start — and, since ADR-0130, mints each discovered agent's plane credential (`agent:<id>:token`) into the ADR-0101 store.
- **The agent plane authenticates and every listener shares one policy** (ADR-0130, 2026-11-01). Every `Orchestrator` RPC and every premium service declared agent-facing requires the per-agent named secret as `authorization: Bearer` with `x-agent-id` as the assertion it must match (`internal/authz/agent_auth.go`; one constant-time compare, `authz.VerifyNamedSecret`, shared with the MCP endpoint); the principal is established there, never from the header alone. The kernel mints missing credentials at boot and injects `CAMBRIAN_AGENT_TOKEN`/`CAMBRIAN_AGENT_ID` into each spawned agent's environment (`agentmgr/agent_credential_env.go`, the one credential SEC-01 admits). `server.insecure_localhost=true` waives agent auth (D2) — the benchmark harness runs under it. Every HTTP listener is built by `app/listener_security.go` (`serveHTTP`; plugins use `app.ServeHTTPListener`): loopback default, routable-bind refusal without TLS, header/idle timeouts, a body cap; `/v1/admin/*` takes the operator bearer through the same `OperatorIdentity` as gRPC (`app/admin_auth.go`), and `app/listener_policy_test.go` refuses any other `http.Server` literal. Operator `Login` locks a peer out after 5 failures in 15 min and tokens expire after 12 h (`operator/login_limiter.go`); every attempt is logged and handed to `domain.LoginObserver` plugins (`Registry.AddLoginObserver` — the premium receipts plugin chains failures). The `/healthz` shim is a handler served by `serveHTTP`; only two operator-run tools (`cmd/pagerank-recompute`, premium `cmd/cambrian-runview`) stay on the listener allowlist, each with an ADR citation. Spawned agents dial the address the listener policy bound, and on a pure-TLS bind receive `CAMBRIAN_TLS_CA` (+ `CAMBRIAN_TLS_SERVER_NAME`) beside their token (`app/agent_transport.go`). Both gRPC planes carry a per-principal token bucket after authentication (`internal/ratelimit`, `server.rate_limit`, default 200 req/s burst 400, `ResourceExhausted` on overflow).
- **Launched processes, MCP tool policy and leases fail closed** (ADR-0139 D8 Phase 0, 2026-11-04). One environment allowlist, `internal/procenv`, serves agent spawns (`procenv.Allowlisted`, strict) and MCP stdio servers (`procenv.AllowlistedNamedSecrets`: OS essentials plus the server's `env` list; a credential-shaped name passes only when named, `CAMBRIAN_*`/`LANGFUSE_*` never). A discovered MCP tool with no `tools` entry and no server `default_tool_policy` is Dangerous with effects read+write+egress (`mcp.UnlistedToolPolicy`); `ToolPolicy.Effects` declares effects explicitly. Lease ids are 128-bit crypto/rand hex (`newLeaseID`, `llm_gateway.go`). Every lease resolution goes through `Server.resolveLeaseFor` (`session_identity.go`), which fails closed for agents: an agent-plane caller resolves only a lease bound to itself, so a lease bound to another agent OR to no agent is refused (operator and other non-agent principals are exempt and resolve any live lease; T-20261104-leases-minted-with-no-agentid-are-usable-by-any); `ExecuteTool`, `GenerateViaModelStream` and `GenerateWithTools` refuse such a lease outright. Every site that hands a lease to an agent binds it: plan steps (dispatcher), the bypass arm, retrieval dispatch, verification, chat turns, and plugins via `app.KernelServices.AcquireAgentLLMToken` (mints and binds atomically; `AcquireLLMToken` stays unbound for in-process use such as the reactive start_plan budget). Plan-step leases are minted before selection and bound to the executing agent by the dispatcher immediately before each `CallAgent` (`dispatch.Dispatcher.bindLease` → gateway `BindLeaseAgent`, port `domain.LeaseAgentBinder`); the AgentID moves with a runner-up fallback or yield hand-off. Chat-turn leases are bound to the pool's worker agent. Retrieval spend is charged to a caller's parent lease only when the caller holds it (`callerLease`). The raw `x-session-id` fallback is open only to the operator surface, the MCP task lane and non-agent in-process calls; an agent-plane caller with an unknown value gets no session.
- **Tools come only from MCP servers; the shipped set is curated and pinned** (owner directive `decisions/agent-stack-v2-mcp-tools-and-profiles.md`, 2026-11-05; T-20261105-curate-verify-and-pin-the-mcp-server-set). `configs/mcp.json.example` is the keyless core set, pinned to exact versions (`npx pkg@x.y.z` / `uvx pkg==x.y.z`):
  - fetch, DuckDuckGo search, filesystem, a Docker-backed code sandbox (`llm-sandbox`), a shell (`mcp-shell-server`, which replaces the dead super-shell), git, SQLite (Google MCP Toolbox), time, a pure calculator (`mcp-server-calculator`, added 2026-11-05 for exact arithmetic; the assistant profile reaches it and its role says to use it), pdf-reader, markitdown and sequential-thinking.

  `configs/mcp.integrations.json.example` is a NOT-loaded catalog of keyed entries to copy into `mcp.json`: GitHub, Brave, Tavily, Exa, SearXNG, Google Workspace, Microsoft 365, Slack, Postgres, Notion, Atlassian, Linear, Playwright and Office authoring.

  `configs/MCP.md` holds the catalog, prerequisites (node 22+, uv, Docker), trust notes and the merge procedure. Policy rules:
  - Servers that can write, send or execute list their read tools explicitly and carry no `default_tool_policy`, so new tools fail closed.
  - Network reads declare `egress`.

  Gaps:
  - Every row is registry-verified only; the live handshake is pending (T-20261105-live-install-and-handshake-verification-of-the).
  - Auth is one static credential per server, with no per-principal OAuth (T-20261105-mcp-auth-is-one-static-credential-per-server-no).
  - There is no per-server `enabled` flag (T-20261105-mcp-servers-have-no-enabled-flag-every).
  - `env` is passthrough-only, so sandbox hardening is set kernel-wide (T-20261105-mcp-server-config-cannot-set-literal-env-values).
  - stdio servers are not process-sandboxed (T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s).
  - Server health is not on the operator plane (T-20261105-mcp-server-health-is-not-visible-on-the).
- **Every LLM call is metered by one ledger** (ADR-0131 D1-D3, 2026-11-01). `llm.SpendMeter` (`internal/infrastructure/llm/spend_meter.go`) is fed by the two doors every call passes: the outermost decorator in `Provider.Acquire` (all organs) and `SubstrateLLMGateway` (all agent streams and tool turns, including the `retrieval_agent` chain, whose lease is now bound to the caller's session and charged back to the asking step's lease). Usage is provider-reported — adapters fill a ctx capture cell (`usage_capture.go`) from their own response — and `len/4` is used only as a fallback flagged `Estimated`. `execution.session.max_session_tokens` (> 0) refuses calls with `llm.ErrSpendCeiling`; the plan ends with gRPC `ResourceExhausted` and a `System_SpendCeiling` TaskEvent. Wired in `internal/kernel` `ProvideServer`. Each Execute response carries `_llm_*` spend metadata, router classification included. Attribution is a property of the call's ctx on the same meter (owner decision 2026-11-01): a session in ctx is booked and enforced; `domain.WithBackgroundSpend` + the originating session (the verifier call via `VerificationRequest.SessionID`) is booked, never refused — for gateway calls that property rides the lease: a lease `Acquire`d under the marker is `BudgetLeaseState.Background`, admitted without the ceiling check and booked to its bound session, which is how each verifier call's lease (minted by `VerificationWorker` through the constructor-injected gateway, bound to the originating session, completed after the call) is metered; Execute routes under the caller's existing live session (`liveCallerSession`, never creates one) or on an `llm.WithSpendTab` tab settled into the session it opens; anything else lands in the reported, unenforced kernel bucket `llm.NoSessionBucket` (`kernel:no-session`, logged by the gateway's eviction sweep). Step TaskEvents bill the step lease's provider usage (`llm.StampStepUsage` → `domain.HandoffKeyStep*` → DAG executor), with `TaskEvent.UsageEstimated` flagging the `len/4` fallback. Lease completion runs one gateway hook (`SetCompleteHook`), which clears ADR-0046 run grants for every lease site. One plan gets `1 + max_replan_attempts` planner calls in total (validation retries and replans share the budget, `plan_call_budget.go`).
- **One LLM request contract, one constructor** (ADR-0139 C1/D1 Phase 1 wave 1a, 2026-11-04, T-20261104-llm-transport-has-no-system-role-or-message). `domain.RequestGenerator` (`domain/llm_request.go`) takes a structured `GenerateRequest` (system blocks with cache marks, messages, tools, tool choice, response schema, options, cache TTL, `<provider>:<field>` extensions) and returns text, tool calls, a normalised stop reason, usage (input/output/cache read/cache write) and `Ignored[]`. Each generator declares per-field `Capabilities` (enforced / best_effort / unsupported), overridable per generator in `llm_provider.generators[].request_capabilities`; an unhonoured field is reported in `Ignored`, and is `domain.ErrUnsupported` only under `Options.Strict` / `ResponseSchema.Strict`. The adapters are on the official SDKs (anthropic-sdk-go v1.84.0, openai-go/v3 v3.74.0, genai v1.77.0); Ollama goes through its `/v1` endpoint and the OpenAI adapter (the hand-written `/api/generate` client is gone; `OllamaEmbedder` is unchanged). `llm.NewClient(GeneratorConfig)` is the ONE constructor (the Provider registry and `ProbeGenerator` use it; `config.ModelConfig` was deleted; the gateway streamers and the `ProviderRegistry` stub were deleted in wave 1c). The legacy ports (`Generate`, `GenerateWithTools`, `GenerateStream`) are shims over `GenerateRequest`. Every Provider decorator forwards the C1 port; `schemaGenerator` (outermost, `schema.go`) validates a best-effort response schema with santhosh-tekuri/jsonschema/v6 and repairs once, then fails with `domain.ErrSchemaValidation`; purpose/model handles fall back to the next healthy generator on 429/5xx/transport errors and on 401/403 after one same-generator retry, never on a refusal, a schema failure or a 4xx. SDK retries are off; `sharedLLMTransport` is the only retry layer. Anthropic caching: breakpoint on the last cache-marked system block plus the last three messages (max 4), TTL 5m/1h from `CacheTTL`, opt-in. Not yet done: the premium Langfuse wrapper does not forward the C1 port (C1 calls are then untraced, logged once).
- **The agent plane is on C1; one pathway, one breaker** (ADR-0139 D1 Phase 1 wave 1c, 2026-11-05, T-20261104-llm-gateway-silently-drops-generateoptions-sdk). `cambrian.proto` (additive): `GenerateStreamRequest` gains `system` (5, `SystemBlock{text, cache}`), `messages` (6), `response_schema` (7, `ResponseSchema{name, schema_json, strict}`), `cache_ttl` (8, enum `CacheTTL`); `GenerateWithToolsRequest` gains `system` (6), `response_schema` (7), `cache_ttl` (8); `GenerateOptions.max_tokens`/`temperature` became `optional` and gained `strict` (4); `GenerateChunk` (final chunk) and `GenerateWithToolsResponse` gain `ignored`, `usage` (`GenerateUsage`, with cache read/write and `cache_write_known`), `stop_reason`, `model`. `prompt` is deprecated on both requests; a `"system"` role message is refused (system text has one way in). `network/gateway_request.go` builds ONE `domain.GenerateRequest` for both RPCs and maps errors (`FailedPrecondition` = no native tools only; `InvalidArgument` = strict field unsupported; `Aborted` = schema failure after repair). `SubstrateLLMGateway` serves every call through `network.StepModels`, implemented by `llm.Provider.GenerateOn`/`StreamOn`: the step chain (winner → fallbacks → default, generator ids) on the Provider's own decorator stack (breaker, tracing, concurrency cap, schema layer) minus the spend meter, which stays lease-scoped in the gateway (admission, `RecordSessionTally`), with the D1 fallback policy (transport/429/5xx only). The gateway's second health cache (hardcoded 30 s cooldown), its client table (`RegisterModelClient`/`SetClientFactory`), `llm.NewStreamersFromGenerators`, `llm.ProviderRegistry` and `Server.ModelRouter` are deleted; a live generator reload needs no gateway rewiring. A Pass-1 budget cut now cancels and drains the stream (the Provider's concurrency slot is held until the stream ends). The ADR-0079 exchange lane now also captures `GenerateWithTools` turns (rendered system/messages/tool calls). Test double: `internal/testing/fakemodels`.
- **Lifecycle hooks narrow, never widen** (ADR-0139 D11 / C5, 2026-11-05, T-20261105-adr-0139-d11-lifecycle-hooks-pre-post-tool-pre). `domain.Hook` (`domain/hooks.go`) gets a deep-copied `HookEvent` (principal chain, agent, tool + args + effects or a model-call summary, lease, run id, current verdict) and returns a `HookResult`: narrow (`ask`/`deny` + reason), rewrite input (`pre_tool` args / `pre_model` messages) or output (`post_tool` result / `post_model` text and final-answer deliverable) only when the spec is `may_rewrite`, and annotations. `domain.HookChain` runs hooks in (priority, name) order on the caller goroutine with a ctx timeout (a late answer is discarded), folds them so one hook's narrowing feeds the next, and is the only place the rule lives: a verdict moves only allow → ask → deny, a looser answer is ignored and logged `widen_ignored`; an error/timeout/panic/malformed result follows `on_failure` (default `block` at pre_*/*_start, `allow` at post_*/*_end). `ToolExecutor.Execute` runs `pre_tool` AFTER grant/resource/scope/effects (now one `authorize` function, `domain/hook_tool.go`) and re-runs `authorize` on a rewritten call, so a rewrite cannot reach an ungranted path; a hook `ask` goes to the approval controller (nil ⇒ deny; under `tools_auto_approve` it is approved); `post_tool` may redact or withhold the output (withheld outputs are not recorded to memory). `pre_model`/`post_model` run at the `llm.Provider` entry points (purpose/model handles incl. legacy `Generate`/`GenerateWithTools`, and `GenerateOn`/`StreamOn`), outside the fallback walk; a stream a `post_model` hook's scope matches (`HookChain.HasFor` with the purpose label and the context's agent, decided before the call) is served BUFFERED (`Provider.bufferedStream`/`bufferedPromptStream`: consumed internally, post_model on the complete reply, the rewritten text delivered as one chunk + the C1 final chunk, nothing on a deny or a broken stream; this covers the SDK text protocol via `GenerateViaModelStream`, T-20261106-text-protocol-agent-replies-never-reach-post), any other stream fires `pre_model` only; `post_model` sees the whole reply — text, tool calls (`HookEvent.ToolCalls`, read-only) and the native lane's deliverable (`HookEvent.FinalAnswer`: the `execution.step_output.final_answer_tool`/`_arg` call's string argument, `llm.WithFinalAnswer`), which a `may_rewrite` hook rewrites (`HookResult.FinalAnswer`) and `HookChain` writes back into the call's JSON args (`domain/hook_reply.go`); `redact_output` redacts both; an unreadable deliverable (non-object args, missing/non-string arg, two final-answer calls) makes every `may_rewrite` post_model hook deny the reply whatever its `on_failure` (T-20261106-post-model-hooks-never-see-native-tool-call); generalised to GUARDED ARGUMENTS (2026-11-06, T-20261106-post-model-hooks-do-not-guard-control-verb): `execution.hooks.guarded_arguments` (default `yield_subgoal.intent`, `spawn_subagent.objective`, `propose_memory_note.content`, `propose_skill.skill_md`; `llm.WithGuardedArgs`) plus the final answer appear as `HookEvent.GuardedArgs` and are rewritten via `HookResult.GuardedArgs` through the same write-back (`FinalAnswer` kept for back-compat), `redact_output` covers them, an unreadable one (non-object args, present non-string value) fails closed; other calls' args are left to `pre_tool`; hooks carry a static SCOPE (2026-11-06, T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or): `HookSpec.Agents` (every point; never matches an event with no agent) and `HookSpec.Purposes` (model points; labels router/planner/verifier/memory/facts/thought/agent_step/model; `thought` since 2026-11-06 = in-kernel thought-step synthesis, `domain.PurposeThought`, previously labelled planner — a deliverables scope is `[agent_step, thought]`; routing unchanged: with no `llm_provider.roles.thought` it resolves through the planner's role, `llm.RoleFallbacks`; the role is assignable from the console; T-20261106-thought-steps-call-the-model-under-the-planner), config `entries[].agents`/`purposes`, checked by `HookSpec.Matches` before a hook runs (non-matching = skipped, not recorded); on the model path ask = deny (`domain.ErrHookDenied`, never a fallback trigger); pre_model annotations join the request as a trailing system block. The chain is built once in `bootstrapKernel` (`hooks.NewChain`: `execution.hooks` + `Options.Hooks`) and constructor-injected into the Provider (`llm.WithHooks`) and the ToolExecutor (`Hooks`). Fire today: pre/post_tool, pre/post_model, and `subagent_start`/`subagent_end` (D12, `network/subagent.go`; a start deny refuses the spawn); `pre/post_compact`, `run_start/end` are declared for D3/D7. Every invocation is slog-logged and handed to `Options.HookObserver` / `Registry.AddHookObserver`.
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

## Known gaps

- **Upstream results of a dependent plan step (fixed 2026-11-06, T-20261105-a-planner-thought-step-that-depends-on-two).** One manifest, `domain.StepDependency` (`domain/step_dependency.go`: step, objective, status `completed|empty|unavailable`, cid, bytes, content, shortened), built per dispatch by `executer.buildStepDependencies` (`internal/metabolism/executer/dependencies.go`) from `DependsOn` and carried in the handoff metadata under `_step_dependencies` for both workspace modes and both executors (SDK agent, in-kernel thought step). Content is bounded fair-share by `execution.plan.dependency_results_max_chars` (default 24000; replaces `thought_results_max_chars`) and a shortened result keeps its cid. The thought step renders it (`thought_synthesis.go`, prompt `thought_synthesis` v2; the old "payload is the prompt" branch that hid every result since 2026-07 is gone); the step cache keys dependent steps on the full upstream results; an empty step is recorded as empty (no stale `step_N_result` after a replan); an empty fan-out reports its node as an empty dependency. Since 2026-11-06 (`T-20261106-an-injected-injectcorrection-step-cannot-see`) a replan (InjectCorrection, error replan, hot-swap) captures the swapped-out plan's completed and failed steps (`priorPlanResults` in `dependencies.go`, `StepDependency.PriorPlan`/`Error`, status `failed`) and hands them, bounded, to every replacement-plan step with no in-plan dependency, labelled `prior_step_N`; the step cache keys on them. Open: operator narrowing via an explicit `depends_on` on InjectCorrection (`T-20261106-injectcorrection-has-no-way-to-name-which`, needs a proto change).

- ADR-0139 D12 subagents (2026-11-05): (1) a `target_capability` child's winner is not checked against the decision point (`KindAgent`), only a named `target_agent_id` is; (2) every child boots a dedicated agent process (cold start per spawn, no pool); (3) the child's output schema reaches the SDK child as a prompt constraint, not as `response_schema` on its final call (the kernel validates + repairs once); (4) a kernel "tool round" is one admitted ExecuteTool call; (5) artifacts are the child's offloaded tool-result CIDs only; (6) not exercised on a live kernel + model yet.

Each has a ticket; close the ticket and update this list in the same change.

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
  text protocol, so the E2E covers only the native lane's redaction. `subagent_start`'s Detail truncates the objective at 500 runes and omits
  `context_summary` (not a model argument, so not guardable at post_model) —
  `T-20261106-subagent-start-hook-detail-truncates-the`. No built-in deny-pattern kind attaches to `post_model`
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

- Agent stack v2: a profile's `trust_tier` is stored but not mapped to ADR-0139 D8 process containment (every profile
  agent runs in the SEC-01 sandbox; boot WARNs) — `T-20261105-profile-trust-tier-is-stored-but-not-mapped-to`;
  profile allowlists cannot name contributed `local:` tools, so the ceiling filters them out —
  `T-20261105-profile-tool-allowlists-cannot-name-contributed`; the planner/replan prompts' hard-pin example still
  names the deleted `terminal_agent` — `T-20261105-planner-and-replan-prompts-agent-pin-example`

- ADR-0139 D2: the router, planner, replan and clarifier (wave 2a) and the memory organs (facts, triplets) and thought-step synthesis (wave 2b) render through `prompts/` and send their response schema; the rewritten wave-2a prompt texts have no real-model benchmark yet (`T-20261105-router-planner-replan-and-clarifier-prompts`); other organs migrate in their own waves — `T-20261104-no-structured-outputs-anywhere-every-kernel`. The retrieval_agent prompts (wave 2c) are unmeasured until an A/B/A run (`T-20261105-a-b-a-retrieval-run-for-the-retrieval-agent`), and the customer tools-only chat posture is enforced only by the chat worker, not server-side (`T-20261105-kernel-does-not-enforce-the-customer-tools-only`). Per-prompt golden sets are not yet registered as benchmarks; the structured triplet format needs an A/B/A run before it can be the default (`T-20261105-a-b-a-retrieval-run-for-the-structured-llm`), and the moved fact prompts make every stored extraction pending for facts-backfill (`T-20261105-fact-extractor-moved-to-the-structured-library`).

- ADR-0139 D11: hook invocations are logged and offered to `HookObserver`, but no durable event log or journal
  subscribes yet (D7) — `T-20261105-hook-invocations-reach-only-the-slog-log-and`; tool-hook annotations never reach the
  agent (no response field) — `T-20261105-pre-tool-and-post-tool-hook-annotations-are`; the compaction, run and subagent
  points are declared but fire only when D3/D7/D12 call them.

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
