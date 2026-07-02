# ADR-0050: GAIA Benchmark Instrumentation — `benchmark_mode`, `bypass_auction`, `memory_stats`

**Status:** Accepted (2026-06-22) — grilled; PRD at `docs/prd/0050-prd-gaia-benchmark-runner.md`; as-built detail lands in `CURRENT_CODEBASE_STATE.md` once implementation lands.
**Date:** 2026-06-22
**Author:** Afsin
**Depends on:** ADR-0018 (LLMGateway — needed for the React baseline to acquire a generator), ADR-0022 (Global Workspace / ContentStore — `memory_stats` rides the existing `PrimeForStep` seam), ADR-0036 (ReAct loop — the `run_think` ReAct loop the React baseline uses), ADR-0039/0040 (kernel-owned tool registry — the React baseline's tool surface), ADR-0041 (Local Recurrent Workspace — `run_think` lives here), ADR-0042 (centralized LLM provider — the React baseline uses `Acquire`), ADR-0048 (Working-Memory Context Hygiene — the prompt hygiene + `<ActionProtocol>` we extend with `<FinalAnswerRules>`).
**Relates to:** `docs/prd/0050-prd-gaia-benchmark-runner.md` (the runner that consumes these three knobs); `docs/adr/0037-central-executive-planner.md` (the `"react"` selector is a sibling of `"auction"`/`"efe"`); future benchmark consumers (these knobs are benchmark-mode-shaped but not benchmark-specific).

---

## Context

Cambrian's orchestration thesis (Auction + DAGExecutor + Skills + EpisodicMemory) has never been measured against a public, external agent benchmark. The GAIA runner PRD (`docs/prd/0050-prd-gaia-benchmark-runner.md`) is the measurement instrument; this ADR is the Substrate-side seam the instrument needs.

Three independent changes land together because they share the same review unit ("make Cambrian benchmarkable") and the same gated flag (`execution.benchmark_mode: bool`):

- **A within-Substrate no-orchestration baseline** is needed for the A/B arm matrix (auction vs react). The runner cannot meaningfully compare the auction to "a ReAct agent" by going outside the Substrate — that would change the tool surface, the scope/security layer, the telemetry, and the approval model, contaminating the A/B. We need a `bypass_auction` mode that stays inside the kernel.
- **A prompt-level answer-format directive** is needed because Cambrian's agents are chat-optimized (verbose, explanatory) while GAIA's grader is exact-match after normalization. The canonicalizer (a runner-side post-processing step) is the *safety net*; the prompt directive is the *primary lever* that reduces how often the safety net fires.
- **Per-question memory retrieval stats** are needed to explain the `auc-eon` vs `auc-eoff` delta. A black-box "episodic helped/hurt" number without the underlying `LTM hits / episodic uses` count is uninterpretable — the architectural thesis is only falsifiable if we can see *why* it moved.

The three are small (~100 lines of Go total), isolated (no other organ touched), and gated on a single `benchmark_mode` flag (the React baseline uses `bypass_auction` independently). They are the minimum substrate surface a benchmark runner needs to ask honest questions.

---

## Decisions

### D1 — `bypass_auction: bool` + `single_agent_id: string` config (the React baseline)

When `bypass_auction=true`, `Server.Execute` short-circuits the planning/auction/DAG path and dispatches the user's question verbatim to a single configured agent. The agent runs its existing `run_think` ReAct loop (ADR-0041) and returns `final_answer`. Everything else is identical to the auction path: same WorkspaceStage priming, same tool grants, same scope/security, same LLMProvider acquisition, same Langfuse tracing, same approval gate. Only the plan shape changes.

- **`bypass_auction`** — config field in `execution.bypass_auction`, default `false`. When `true`, `Server.Execute` skips:
  - `Planner.planWithValidation` (no LLM call for plan generation)
  - `Auctioneer.ConductAuction` (no bidding round)
  - `DAGExecutor.Execute` (no DAG traversal, no SelfHealer, no ReplanHandler — the React baseline is single-step by construction)
- **`single_agent_id`** — required when `bypass_auction=true`. The agent ID to dispatch to. The runner is responsible for ensuring this agent is registered; the Substrate errors with a typed `agent_not_found` envelope if not.
- **WorkspaceStage stays on** — the React baseline still gets the same memory priming as a real plan's first step would, otherwise the React arm gets a worse starting position than the auction arm. `PrimeForStep` runs once with the user's question as the seed; no `depends_on` chain to expand.
- **Tool surface identical** — the agent's tool grants are the same as in the auction arm. The runner's arm config (`benchmarks/gaia/configs/<arm>.json`) sets the grants; the Substrate honors them whether or not `bypass_auction` is on.
- **Above the line** — this is a kernel change. Versioned with the substrate. Any change to the bypass path is a real kernel release.

Rejected alternatives: (a) a new `"react"` `resource_selector` value — the Planner would still run (1 LLM call wasted) and the Auctioneer would still happen with one candidate, contaminating the A/B. (b) a pure-Python ReAct baseline outside the Substrate — loses the kernel's tool grants, scope, audit, approval, and Langfuse; the apples-to-apples comparison breaks.

### D2 — `benchmark_mode: bool` config flag

A single boolean gates all benchmark-specific Substrate behavior. When `true`:

- `<FinalAnswerRules>` is composed into the ActionProtocol (D3).
- `memory_stats` are attached to every Execute response (D4).
- Both `bypass_auction` and the auction path honor the flag (the prompt directive is a kernel feature, not an orchestration feature — both arms get it).

Default `false`. The Substrate's behavior in non-benchmark mode is unchanged. The flag is set by the runner at arm start (per `benchmarks/gaia/configs/<arm>.json`); production callers leave it `false`.

### D3 — `WithBenchmarkRules() PromptBuilder` method + `<FinalAnswerRules>` ActionProtocol block

A new `PromptBuilder` method that, when `benchmark_mode=true`, composes a `<FinalAnswerRules>` block into the ActionProtocol (which itself was relocated to its own XML tag in ADR-0048 D8). The rules are strict + one escape hatch:

```xml
<FinalAnswerRules benchmark="true">
When emitting final_answer:
- Match the answer to the question type:
    - "How many/much…" → a number.
    - "Who/What/Which/Where…" → a name.
    - "When…" → a date or year.
    - "Is/Does/Are/Has/Can/Should…" → yes or no.
- Include ONLY the answer value. No preamble, no explanation, no labels.
- Do not include units unless the question explicitly asks for them.
- Do not include the question text.
- Do not apologize, hedge, or qualify.
- Exception: if the question explicitly asks for an explanation
  (e.g. "explain why…", "describe how…"), follow the question.
</FinalAnswerRules>
```

- **Loop-invariant** — the ActionProtocol is composed once per task (ADR-0048 D8), so the rules appear in the initial system message and stay cached for the loop. Same stable bytes, same cacheable surface.
- **Both arms** — `auc-*` and `react-*` arms get the directive. The directive is a kernel feature, not an orchestration feature; the matrix measures "Cambrian with directive" vs "Cambrian without orchestration with directive."
- **Above the line** — the directive's text is a kernel string, versioned with the substrate. Tuning the directive is a real kernel release, not a benchmark-time config change. A future A/B on directive variants is a research project, not a benchmark concern.
- **Telemetry** — every Execute response carries `prompt_directive_present: bool` (a server-side flag, not a prompt-emitted string) so the runner can verify the agent was running with the directive.

Rejected alternative: a below-the-line (tuning surface) version of the directive. Would let the canonicalizer iterate the directive alongside the rules, but invalidates all prior scores under (b) methodology. Above the line keeps the score history coherent.

### D4 — `memory_stats` per-question column

A new field on the `Execute` response envelope: `MemoryStats{ LTMSearchHits int, EpisodicMemoryUses int, ProceduralTemplates int, ContentStoreReads int }`. Populated by the Substrate as part of the existing `PrimeForStep` + `WorkspaceStage` flow (ADR-0022). The runner writes it into every JSONL row as `memory_stats` so the eon/eoff delta is explainable, not a black-box number.

- **Counted operations**:
  - `LTMSearchHits` — number of documents returned by `WorkspaceStage.PrimeForStep` (mnemonic_fact + mnemonic_scene, deduped after the ADR-0048 D1 filter).
  - `EpisodicMemoryUses` — number of `EpisodicMemory` documents returned (the ADR-0029 lane).
  - `ProceduralTemplates` — number of `DocTypeProceduralTemplate` matches (Hippocampus).
  - `ContentStoreReads` — number of `GetContextNode` calls the agent made during the question.
- **Zero behavior change** — the stats are observation, not routing. The agent's behavior is identical with or without the stat collection.
- **Bypass path** — D1's React baseline still emits `memory_stats` (the React baseline still goes through `PrimeForStep`; the stats are the same shape as a real plan's first step).
- **Benchmark mode only** — when `benchmark_mode=false`, the field is omitted (zero-value struct). Production callers don't pay the (negligible) cost of counting.
- **Above the line** — `memory_stats` is a kernel-emitted field on the response envelope. Changes to the field's shape are real kernel changes.

Rejected alternative: separate span-level counters in Langfuse. Redundant with the response envelope; harder to correlate per-question; harder to write to JSONL. The response envelope is the cleanest.

---

## Consequences

- **Total Go surface:** ~150 lines. D1 ~80 lines in `Server.Execute` (one new branch + a config field); D2 ~5 lines (config field + one gate); D3 ~40 lines in `awareness/prompt.go` (one new builder method + a static rules string); D4 ~25 lines across `WorkspaceStage` + the response mapper. All isolated; no organ touched.
- **Test plan:**
  - D1 unit: `Server.Execute` with `bypass_auction=true` and a registered agent returns the agent's `final_answer` directly; without `bypass_auction`, the path is unchanged. PRD §Testing Decisions: a 2-question golden set exercises the bypass path end-to-end.
  - D3 unit: `WithBenchmarkRules()` produces the expected XML; without the flag, no `<FinalAnswerRules>` block.
  - D4 unit: `memory_stats` is populated for a synthetic Execute with a known LTM state; zero-value when `benchmark_mode=false`.
- **Sequence:** D2 first (config field, no behavior change, unblocks D3/D4). D1, D3, D4 in parallel after D2.
- **New term in `CONTEXT.md`:** "BenchmarkMode" (the umbrella term) + "BypassAuction" (the React-baseline mechanism). Both are first-class Substrate capabilities, not benchmark-specific despite the naming.
- **Risk to measure:** D3's prompt directive might over-correct (agent emits bare `"5"` even when a unit is requested because the agent interpreted the rules too aggressively). Mitigation: the escape hatch + the canonicalizer as a safety net. The first benchmark run will tell us.
- **Out of scope:** test-set runs (HF-gated, separate decision); image/audio multimodal agents (separate ADR scope); A/B on directive variants (research, not benchmark); per-question retries (under (b) methodology, the first attempt is the score).
