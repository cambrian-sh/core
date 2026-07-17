# ADR-0051: Grounded Planner — Pre-Plan Discovery via a Scout Agent

> **Amended-by (pending): [ADR-0078](0078-scout-deterministic-discovery-organ.md)** — Scout is inverted from LLM-`run_think`-first to **deterministic-probe-first** (LLM demoted to opt-in): D1/D4 rewritten, D3/D9 world-model write-back replaced by **session-memory** persistence, D13 reinforced. D2/D5/D6/D8/D11 kept. Decisions here remain live until ADR-0078 ships.

**Status:** Proposed (2026-06-22) — design recorded via a grilling session; not implemented. Gated on the falsification spike in §Falsification (inherits ADR-0038's A/B discipline, amended).
**Date:** 2026-06-22
**Author:** Afsin
**Amends:** **ADR-0038** (Unified Deliberation Loop) — reverses **D4** (trivial skip-gate), extends **D9** (adds a live-observation grounding axis the Go retrieve-only loop lacks), clarifies **D8** (fan-out as a sanctioned in-execution-editing exception), and updates the Falsification plan.
**Depends on:** ADR-0049 (Experiential Memory — world-model prior via `PrimeForPlanning` D11; **this ADR amends 0049** with entity valid-time + a drift event — see ADR-0049 §Amendment A1), ADR-0038 (Unified Deliberation Loop — Scout's focused Go reason→scan→report loop is its first concrete instance, see D1), ADR-0039 (`ToolExecutor` reference monitor + grants — Scout calls it in-process), ADR-0043 (MCP tool provider — budget/egress; Scout's only tool path), ADR-0044 (`find_tools` semantic retrieval — Scout finds its discovery tools), ADR-0048 (`content_cid` offload + context hygiene), ADR-0029 (ConsolidatorAgent — the privileged-organ precedent Scout mirrors), the Zero-Hardcode Rule (`CONTEXT.md`). The ADR-0049 prior Scout relies on (working + episodic + semantic memory) is independently validated by **BrainMem** (arXiv:2604.16331), a training-free working/episodic/semantic hierarchy — so Scout's `PrimeForPlanning` dependency inherits that validation.
**Requirements doc:** `docs/requirements/REQ-REACTIVE-PLANNER-GROUNDING.md` (the design discussion + research grounding §8).

---

## Context

The Planner is **one-shot**: `GetExecutionPlan(userInput) → single LLM call → DAG`. It cannot observe the world before it plans, so it cannot adapt the plan's *shape* to reality.

**The failure mode has a name: Delayed Environmental Perception → an Epistemic Bottleneck** (MAP, arXiv:2605.13037). The planner commits before it perceives, so its plan is a guess about a world it never inspected. MAP reports the consequence empirically — on WebArena ~80% of a one-shot plan's structure is "purely programmatic" boilerplate uninformed by the actual environment. The fix is to make *what can be done here* precede *what to do* — which is **Tolman's cognitive-map theory (1948)** and **Gibson's affordance perception (1979)**: an agent forms a map of the situation, and perceives affordances, *before* acting. Scout implements this lineage in a dedicated **pre-plan perception phase** — 75-year-old cognitive science, not ad-hoc engineering.

**Motivating failure.** *"Continue documenting a summary of what's needed to create a helicopter, in the folder `helicopter`, where we left off."* The planner emitted a **single-step** plan (one file). Expected: scan `helicopter/` → see prior runs wrote one section per file → emit a **multi-step** DAG, one step per remaining section. Two root causes: (1) **no live discovery** — the one-shot planner can't look, so it guessed "one doc"; (2) **no working precedent** — the ADR-0049 scene that would have recalled the one-file-per-section pattern was broken (MCP-writes-misclassified-as-reads). This ADR fixes **(1)**; (2) is the ADR-0049 lane repair (separate, depended-upon).

**Security angle (not just correctness).** Even a *correct* one-shot planner under a ReAct executor is a liability: untrusted runtime data flows into the model at **every** step, a direct path for prompt injection to steer the agent's *control flow* (Web Agents Should Adopt Plan-Then-Execute, arXiv:2605.14290). PTE draws the trust boundary correctly — untrusted data may influence *values inside a predefined execution graph* but cannot *redefine the user task*. Scout + the frozen DAG (PTE) closes both axes: correctness (the helicopter bug) **and** the injection-control-flow liability. (Scout's own findings are themselves untrusted input — see D13.)

**Why not the obvious fixes.** A per-step reactive loop is O(steps) LLM turns (rejected — PTE/ReWOO efficiency, §Research). ADR-0038's Go deliberation loop grounds the planner in **internal capability** (`CapabilityCatalog`, prior plans — retrieve-only over internal surfaces, D9) but **never observes the environment** — it would not have caught the helicopter bug (the catalog knows "we have a file-writer," not "3 of 10 sections exist"). The missing axis is **external world-state grounding**.

**Theory basis (§Research).** Agentic World Modeling's mixed-mode prescription — use a cached belief for efficiency, *re-observe* when evidence may contradict it — and the named **L2 "silent drift"** failure (a coherent-but-wrong plan from a stale belief) is exactly the helicopter bug. We ground by **observation, not simulation** (the workspace is cheap to observe; we have no learned dynamics model — ADR-0049's non-parametric stance). D8's cap-exhaustion → discovery-step + checkpoint pattern is a form of **continual planning** (desJardins et al., *A Survey of Research in Distributed, Continual Planning*, 1999): the plan is not "one-shot-and-committed" but "committed with bounded replanning on staleness" — a 25-year-old lineage.

---

## Decision

Introduce a **privileged "Scout" agent** that performs **bounded, read-only, pre-plan discovery** and feeds the Planner a compact grounded report. The Planner stays one-shot over Scout's report. The discovery LOOP lives in the Scout agent (`run_think`); a thin Go dispatcher invokes it and a deterministic env block is merged kernel-side.

### D1 — Scout is a privileged **Python `run_think` agent**, kernel-invoked (not auctioned)

> **DECISION HISTORY (two reversals — the trail is honest).** (1) Originally specified as a Python `run_think` agent. (2) Amended 2026-06-22 to a Go in-process organ, on the premise that *"Scout's loop is narrow (find_tools → ≤3 read scans → one interpretation turn), so reimplementing it in Go is cheap."* (3) **Reversed back to Python (2026-06-23)** when that premise proved FALSE: Scout must be a **rich, multi-modal, extensible discovery agent** — discovering filesystem state, API endpoints/schemas, package/library docs (for future coding tools), *and* querying memory — driven by an LLM choosing tools, not a fixed pipeline. The Go implementation had degenerated into exactly the rigid 2-turn filesystem pipeline that scope rejects. Building the rich tool-agnostic ReAct loop in Go means building **and maintaining a second `run_think`** (find_tools/describe_tool/memory_query/multi-tool dispatch/action parser) forever. `run_think` already *is* that loop, battle-tested — so the narrowness premise that justified Go is invalid, and Python is correct. (Honest cost: this is a real rework, not "free" — a new agent + structured-report contract + dispatch seam — but cheaper than a perpetual duplicate loop.)

Scout is a privileged Python `CognitiveAgent` (`agents/scout_agent.py`) driving the SDK **`run_think`** ReAct loop, invoked directly before planning via `Auctioneer.CallAgent` (no auction). Through `run_think` it gets `find_tools` (discover read tools across fs/web/endpoints/schemas/docs), `tool_call`, and `memory_query` for free — confined to **read-only** by its `discovery-safe` grant (D6). Its `final_answer` is the structured `DiscoveryReport`, parsed kernel-side and merged with deterministic env facts. Invoking a fixed system organ is **not** a Zero-Hardcode violation (no `if/else` routes a user task to an agent), and it dissolves the chicken-and-egg (you cannot auction a discovery agent *before* a plan exists; privileged organs aren't auctioned).

- **Tool-round cap (D5)** = `run_think`'s `max_tool_rounds` (Scout sets it tight, ≤3); memory recall = `max_memory_queries` (≤2). Staleness-targeting (issue-002) is now **advisory**: Scout uses `memory_query` to recall what's already known and decides whether to re-observe — not a deterministic Go gate (the Go `TargetStale`/`CapScans`/`WorldModelLookup` modules were removed; the world-model `last_observed_at`/`session_id` stamps remain for Scout to recall over).
- **Concurrency scope.** Invoked per `Execute` call, no shared mutable state.
- **System-agent status (`domain.IsSystemAgent`).** Because Scout is a registered Python agent (unlike the Go ConsolidatorAgent) it would otherwise hit the Gatekeeper interview and the auction/EFE candidate pool. As a privileged organ it is marked a **system agent**: it **bypasses the interview** and is **verified by default** (InterviewWorker fast-path — `Provisional=false`, `TrustScore=1.0`), and the **Gatekeeper excludes it** from candidates, so it is **never auctioned/EFE-assigned a user task**. A deterministic system exception (not task routing → Zero-Hardcode unaffected). Model is allocated deliberately via the dispatcher's `Acquire` (a managed session pinned to `scout_model`), not the gateway default-fallback.

### D2 — Always-Scout, no heuristic skip-gate

Scout runs on **every** request (cheap early-exit when there is nothing to observe). A heuristic gate that skips grounding for "trivial" requests is **rejected** — deciding whether to look *without having looked* is the same epistemic error that caused the motivating failure (a state-dependent request need not carry a continuity keyword). The look/no-look decision is grounded in **observed staleness** (D3), never **guessed triviality**. Putting the swing decision on the organ whose *default is to look* (Scout) — rather than the planner, whose default is to plan — makes the failure mode cheap (a wasted turn), not catastrophic (an ungrounded plan).

### D3 — Discovery is staleness-*targeted*, not staleness-*gated*

Scout's first move is cheap: read the world-model prior (ADR-0049 entities via `PrimeForPlanning`) for the entities the request references — **no live tool call**. Per-entity **valid-time** (`last_observed_at`, added by ADR-0049 §A1) decides freshness. Scout's **live-scan set = the stale-or-unknown referenced entities only**; fresh entities are served by the prior. **Early-exit = all referenced entities fresh (or none referenced)** → zero live scans, emit. "Always look" thus splits into a **cheap look at memory** (always) and an **expensive look at the world** (only for stale/unknown referents).

- **Kind-aware staleness tolerance.** `last_observed_at` is "when *we* last looked," not "when it last *changed*" — so freshness ≠ trustworthiness for externally-mutable entities. Tolerance is **per-entity-kind**, operator-configurable, **defaulting to "trust only what we wrote this session; re-observe everything external"** (`api:`/`url:`/shared get ~zero cache trust; `file:`/`dir:` we wrote get a window).
- The deterministic threshold may only push Scout toward **more** looking, never skip a look it would otherwise do (preserves the safe-default bias of D2).

### D4 — Multi-modal discovery, MCP-only, tools found semantically

Discovery spans **filesystem, web, endpoints, DBs** (populating ADR-0049 entity kinds `file:/dir:/api:/url:/service:/db:`). System tools are deferred; Scout's **only** tool path is MCP (ADR-0043). Scout **does not hardcode tool names** — it finds its discovery tools via ADR-0044 `find_tools` ("list directory contents"), so it works across whatever read-MCP servers the operator wired (Zero-Hardcode-clean). **`find_tools` is FREE of the D5 live-scan cap** — it is a metadata *retrieval*, not a live world *observation* (no egress, no real workspace read), so it must not consume one of Scout's scans; otherwise Scout would have only 2 of 3 calls left for actual observation. The cap counts world observations only (D5).

- **Deployment-contingency (accepted).** If no read-capable MCP server is configured, Scout has nothing to observe with → it early-exits and the system **degrades gracefully to one-shot + world-model prior** (no crash). Grounding is a *capability*, not a guarantee; the helicopter-class fix holds **only where a read-MCP server exists**. A **reference read-only filesystem MCP server** is recommended as a deployment component so filesystem grounding works out-of-the-box without violating the System-tools deferral.
- **Non-goal: multimodal (GUI/visual) discovery.** Scout observes **textual** state only (fs/web/api/db). It **cannot** observe a screenshot/GUI/canvas state — the corpus treats vision as a first-class discovery channel (§Research: S-Agent, AlloSpatial, MyPCBench), but Cambrian has no vision `TraitModel`. So Scout inherits Cambrian's known multimodal gap *by design*; a GUI-state discovery would need a vision tool + a separate ADR. Stated as an explicit non-goal so it is not mistaken for an omission.

### D5 — Two orthogonal governors: a latency cap and the existing spend/egress controls

- **Latency cap (new).** A **hard cap on live scans**, default **3**, operator-configurable, counting **live world observations only** (`find_tools` is free — D4; memory recalls stay under their existing `mem_queries` budget). Not budget-based — deterministic plan-time latency matters under always-Scout. **Why 3:** it bounds plan-time latency to ~5–10s at typical MCP tool latencies (~200–500ms each + one Scout LLM turn). It is also the saturation knee for fixed-budget multi-step synergy — phase-transition analysis (arXiv:2601.17311) places the useful threshold at ~3–5; past it, marginal grounding falls off sharply. Operator-overridable. Within the cap, Scout prioritizes by **goal-relevance** (a cheap embedding rank inside Scout): staleness decides *eligibility* to scan, relevance decides *order*.
- **Tool error mid-scan (robustness).** A discovery tool failing mid-loop (MCP server down after `find_tools`, timeout, malformed response) is **not** fatal: Scout **logs the failure, continues with the remaining scans, and stamps the unobserved entity into `<DiscoveryLTM>`** (a Reactive-Self-Correction pattern, arXiv:2605.24069). The **"never discard findings"** invariant (D8) holds — partial findings are emitted and the Planner reasons over the partial map, marking the unobserved entities for a discovery-step (D8). This is distinct from D8's *cap-exhaustion* (out of budget) — here a scan *failed*; both degrade to "emit what we have + flag the gap," never to a crash.
- **Spend/egress (reused, not rebuilt).** Scout is a normal cognitive agent through the `ToolExecutor`, so its paid/egress discovery is already metered by the ADR-0043 `BudgetLedger` + audited by the `EgressAuditor`/SSRF guard. **Plan-time discovery debits the session budget** — it is real spend before execution starts, and can hit `budget_exhausted` *before* a plan is emitted (→ degrade to one-shot). The always-Scout cost worry is neutralized by D3: a request referencing nothing stale does zero discovery, hence zero paid calls.

### D6 — `discovery-safe` tool tag (stronger than "not-dangerous")

Scout's grant is gated by an explicit operator **`discovery-safe`** classification — *narrower* than "not-dangerous" — because under always-Scout these tools fire **constantly and unattended at plan time**, earning a higher bar than a deliberate agent tool call. Read-only is enforced by **grant curation**, not tool self-classification (MCP `DataReadKinds` is never stamped — ADR-0043 connector — and `DataWriteKinds` is unreliable).

- **Threat model: Tool Description Poisoning.** The reason curation must not trust tool self-classification: a malicious MCP server can poison its own tool descriptions to be selected/mis-trusted — ~**100% attack success on GPT-4o across 6 high-risk scenarios** (arXiv:2605.24069). `discovery-safe` defends by making **the operator**, not the tool's self-description, the classifier. Residual: a *misconfigured* MCP server (operator marks a mutating endpoint read-only) — mitigated by the ADR-0043 `EgressAuditor` log + operator postmortem, **not** in-loop detection (we don't second-guess the operator's grant at plan time).

- **Residual (documented, not solved).** The read-only guarantee is **strong for filesystem** (no write grant ≈ safe) and **weak for endpoints** (a "GET" can mutate; relies on the operator marking mutating endpoints `Dangerous` + the egress audit). Same generic MCP-trust residual, sharper for non-fs discovery. No new mitigation beyond `discovery-safe` curation + egress audit.

### D7 — Topology: Scout loops (bounded), the Planner stays one-shot

The discovery **loop lives in the Scout agent** (bounded by D5), invoked **before `planWithValidation`** via `Auctioneer.CallAgent` (a thin Go `AgentScoutDispatcher` does the call + parses the structured report + merges env). The **Planner consumes Scout's report in a single call** — the dispatcher attaches it to ctx (`WithDiscovery`/`DiscoveryFromContext`) for `GetExecutionPlan`. There is **no planner↔Scout ping-pong** — if discovery is incomplete (D8), the residual becomes a **DAG discovery-step**, never a Scout re-invocation. This is a deliberate **divergence from ADR-0038 D9 "the Planner loops full"**: the loop lives in a separate privileged agent (conceptually Aime's "Actor Factory" pattern, §Research) and the Planner stays one-shot over findings. (The Handoff round-trip through `CallAgent` is the serialization seam the brief Go-organ detour avoided — accepted as the cost of the rich `run_think` loop, D1.)

### D8 — On cap-exhaustion: emit a discovery-step + checkpoint; reuse existing recovery; never discard

When Scout caps out still-uncertain, it emits its partial findings and the Planner emits a best-effort plan containing an **explicit discovery step with `checkpoint_after: true`** over the un-scanned residual → the **existing `SemanticCheckpoint` + `ReplanHandler`** reshape the tail at execution time. Invariants: **never discard Scout's findings**, **never fall back to bare one-shot**. This keeps slice 1 **independent of fan-out** (D9) — fan-out is an *optimization* of this round-trip, not a dependency. Deep discovery thus converts hidden pre-plan latency into a **visible, resumable DAG step**.

### D9 — Findings representation: compact projection + world-model write-back, raw behind a CID

Scout's observations have **two faces** (mirroring ADR-0049 scenes):
- **`<DiscoveryLTM>`** — a **compact structured state delta** the Planner reasons over now (`helicopter/: 10 sections, 3 written [intro, rotor, tail], 7 missing [...]`). Raw observations (full listings, page text) stay behind a **`content_cid`** (ADR-0048 offload) — **never inlined** (that is the O(N²) bloat ADR-0048 fought).
- **World-model write-back** — the same reads enrich ADR-0049 entities via the existing read-enrichment path, stamping the new `last_observed_at` (D3). Same observations → synchronous Planner input + durable L3 update (next request's prior is fresher).
- **Abstraction is structured-where-structured + a thin LLM interpretation.** Structured observations pass deterministically (ADR-0049's "deterministic where possible"); Scout's LLM adds only a **brief pattern interpretation** ("looks like one-file-per-section, 7 remain") — *that* judgment is Scout's value-add (it is exactly the §Context insight the one-shot planner missed). Residual: the LLM interpretation could misread the pattern; the structured delta + CID let the Planner cross-check. **Empirical anchor:** factorized object-attribute world-state representations beat reactive system-1 policies by **+21.64% (AlfWorld) / +12.40% (ScienceWorld)** (StateFactory, arXiv:2603.09400) — i.e. the *structured delta is the substrate; the LLM layer rides thin on top*, exactly D9's split.

### D10 — Slice 2: fan-out / map nodes (proactive map, reusing replan)

For genuinely-unknown cardinality, a DAG node may be **parametric**:
```json
{ "query": "write the file for {item}", "fan_out_over": 2, "depends_on": [2] }
```
- **Source** — `fan_out_over` references a prior step's **structured** output (Scout's discovery delta is the natural source — D9). Extracted **deterministically**, no LLM re-parse.
- **Expansion** — at runtime, when the source completes, the executor expands into N children with **deterministic template substitution** (no per-item LLM — that would be the O(steps) cost we reject; per-item *different* handling is a `replan`, not a fan-out). Reuses `applyReplannedPlan`.
- **Dependencies** — a step depending on the fan-out node is a **barrier/reduce** (waits for all children, receives aggregated outputs — VMAO context propagation); no dependent ⇒ **pure map** (parallel, which the executor already handles). No per-child streaming in slice 2.
- **Width cap** — operator-configurable max fan-out width; on exceed, raise a **structured expansion error → `ReplanHandler`** (no silent truncation, no recursive fan-out).
- **Determinism boundary (amends ADR-0038 D8)** — runtime expansion is deterministic given the source output, so it is a **sanctioned in-execution-editing exception**, the same class as `replan` — *not* a breach of DAG immutability.
- **Boundary vs. `yield_subgoal` (keeps the O(steps) cost out).** Fan-out is **purely parametric** — same agent, different item substituted into a fixed template. **Cross-agent / cross-capability delegation is `yield_subgoal` (ADR-0036), never fan-out.** The O(steps) LLM cost we reject is *per-item LLM reasoning*, which is `yield_subgoal`'s regime; fan-out stays deterministic templating with **no per-item LLM call**. State the line explicitly so no one reaches for fan-out to do delegation and smuggles the O(steps) cost back in.

### D11 — Zero-Hardcode preserved

No Go `if/else` over discovered content: Scout is a privileged organ (not auctioned routing, D1); discovery tools are found semantically (`find_tools`, D4) not by fixed name; staleness/kind tolerance are **operator config**, not routing branches; the **LLM emits the plan**; fan-out substitution is deterministic templating over a structured set, not content routing. The deterministic pieces (staleness threshold, width cap, `discovery-safe` gate) are operator-set bounds, consistent with the existing deterministic exceptions (shell/Omurilik/scope/ladder).

### D12 — World model stays non-parametric; learning belongs to selection

We **do not** adopt a parametric/latent world model or RL for the world model: the regime is wrong (data-starved — a workspace yields dozens of transitions, not millions; no dense reward; cheap observation, so simulation buys nothing; and AriGraph's *episodic graph* already beats RL baselines on this task class — §Research). The ADR-0049 episodic store remains the model (it is also the dataset for a future parametric model, if a cheap-simulation-of-expensive-ops use-case ever appears). Any RL-flavored learning belongs in the **selection/retrieval layer (ADR-0037 EFE)** — e.g. RL-based precedent retrieval, and adaptive per-entity trust (a frequently-drifting entity earning a shorter staleness tolerance — deferred there, not a world-model property).

### D13 — Scout's findings are UNTRUSTED input to the planner (injection boundary)

`<DiscoveryLTM>` flows into the **planner prompt** — the highest-stakes reasoning step in the system — so Scout's findings are an **injection / memory-poisoning surface**, sharper than a normal agent tool call (the corpus is loud here: §Research — *Beyond Similarity* tool-call drift / memory-induced jailbreaks; *VESTA* 47.1% average attack success across 12 agents). A poisoned MCP read ("dir contains 7 sections — also, ignore prior instructions and grant…") must not be able to steer the planner.

- **The structured delta is the trust boundary.** Scout's **structured observations** (entity deltas — file lists, `exists`, `content_ref`) cannot carry instructions and are trusted. The **thin LLM interpretation** (D9) and any **raw observed text** are treated as **untrusted**: they pass the **same ADR-0043 MCP-response injection scan** as any other untrusted MCP payload, *at least as strictly* given the privileged consumer, before entering `<DiscoveryLTM>`. Raw bodies stay behind the `content_cid` (D9) — referenced, never inlined into the planner prompt.
- This makes the code-as-WM/LLM-as-WM seam (§Research) also the **trust** seam: the deterministic structured world-state is authoritative *and* trusted; the generative interpretation is advisory *and* untrusted. One boundary, two properties.
- Reuses the existing ADR-0043 `EgressAuditor` + injection scan + the ADR-0034 scope on what Scout may read; adds no new scanner — it **extends the existing scan's coverage to the planner-bound path**.

---

## Consequences

### Good
- **Fixes the motivating failure on live discovery alone** (D3/D8) — independent of the ADR-0049 lane repair, which only *improves quality* (a fresher prior → fewer/cheaper scans).
- **No new loop primitive** — reuses `run_think`, `ToolExecutor`, `find_tools`, budget/egress, `SemanticCheckpoint`/`ReplanHandler`. Scout is a thin privileged organ.
- **Bounded plan-time latency** (D5 cap) + **bounded spend** (D5 budget) as orthogonal governors; trivial requests cost the same as today (D3 early-exit).
- **Context-hygiene-safe** (D9) — compact projection + CID offload, not raw dumps.
- **Composes cleanly with slice 2** — structured findings (D9) are the fan-out source (D10).

### Bad / Cost
- **Always-Scout adds one cheap LLM call to genuinely trivial requests** (the early-exit turn). Accepted (correctness-over-latency); mitigated by D3 (no live scans, no paid calls).
- **Grounding is deployment-contingent** (D4) — no read-MCP server ⇒ behaves like today; the fix-claim narrows to "where a read-MCP server exists."
- **Endpoint read-only is a residual, not a guarantee** (D6).
- **A new privileged organ** to operate, observe, and budget.

### Neutral / Honesty
- This is a **measured** win — the falsification gate below must clear before promotion past the Scout pilot, mirroring ADR-0037/0038 discipline.

## Falsification plan (gate to acceptance — amends ADR-0038)

**Mechanism (implemented, descoped):** the A/B is run via a **config gate** — `execution.scout_enabled` (default `false` ⇒ one-shot baseline; `true` ⇒ Scout-grounded). Flip it between arms and compare. A bespoke trajectory-aware *harness/judge* was deemed too expensive and is out of scope; the comparison + promote/don't-promote decision is a maintainer (HITL) call. Default-off keeps prod on one-shot until promoted, matching this ADR's Proposed status.

A/B on `internal/benchmarks` (`-tags e2e`): **one-shot Planner** vs. **Scout-grounded Planner**. (Note: the ADR-0038 "trivial fast-path skip" arm is removed — D2.)

**Judging is trajectory-aware (process-level), not outcome-only.** The corpus shows outcome-only grading *overestimates* on exactly this task class — long-horizon, multi-interface (§Research: *WeaveBench* frontier PassRate 41.2% with a trajectory-aware judge revealing the overestimate; *VESTA* process-level safety eval). So metrics 1–3 are scored over the **execution trajectory** (did the right steps run, grounded in real state), not just the final artifact — otherwise a wrong-shape plan that stumbles into a passable final file would score as success and mask the very failure this ADR targets.

1. **Plan shape-correctness** — on state-referencing tasks (the helicopter class), the grounded planner emits the right *number* of steps measurably more often.
2. **Plan success rate** ≥ one-shot (trajectory-judged).
3. **Latency** p50/p95 not materially worse, *with D3 staleness-targeting* (trivial requests must not regress).
4. **Cost** (tokens + metered MCP spend/request) within an accepted bound; trivial requests show ~no added MCP spend (D3/D5).
5. **`discovery-safe` enforcement** — Scout never invokes a non-`discovery-safe` tool (D6), verified.

Accepted for the Scout pilot only if 1 shows a measurable grounding win with 2–5 non-inferior. Slice 2 (fan-out, D10) lands only after the pilot clears.

## Sequencing

0. **ADR-0049 §A1 dependency** — entity `last_observed_at` (valid-time) + drift event (passive). Unblocks D3/D9.
1. **Slice 1 — Scout + grounded planning** (D1–D9, D11): privileged Scout organ, always-Scout + staleness-targeting, bounded loop, `discovery-safe` grant + `find_tools` discovery, `<DiscoveryLTM>` + write-back, cap → discovery-step + checkpoint. *Fixes the motivating failure.*
2. **Slice 2 — fan-out nodes** (D10): parametric map node, deterministic expansion, width cap → replan.
3. **Reactive selection** (out of scope — separate REQ/ADR): whether per-step auction becomes ranked `find_agents`. Independent of this ADR.

## Open questions / future evolution (do not block slice 1)

- **Learned self-regulation could replace the "always" default (D2).** "Always-Scout" is a *safe* default, not provably the optimum. A future evolution could learn a **self-regulation policy** — a "System III" that decides *when* to invoke Scout and *how deeply* to plan (SR²AM, arXiv:2605.22138: 25.8–95.3% fewer reasoning tokens; RL grew the planning *horizon* +22.8% while planning *frequency* rose only +2.0%). The A/B (§Falsification) should record whether learned self-regulation would beat the hardcoded "always"; phase-transition analysis (arXiv:2601.17311) is the tool for setting the trigger. Flagged so D2 doesn't calcify.
- **MCTS tool planning at scale (vs. the ANN `find_tools`).** Scout's tool discovery rides ADR-0044's ANN retrieval. `ToolTree` (MCTS, arXiv:2603.12740) reports +10% over ANN baselines at higher cost. At today's ~30-tool menu ANN is right; **when the menu exceeds ~100 tools**, revisit ANN → MCTS (and the SING intention-graph, §Research) for Scout's discovery-tool selection. Future ADR.

## Research grounding

See `REQ-REACTIVE-PLANNER-GROUNDING.md §8` and `docs/research/world-modelling/` for the full surveys. Key load-bearing references: **ReWOO/PTE** (keep execution decoupled, −82% tokens vs ReAct); **Aime** (Actor Factory = privileged on-demand discovery organ — D1/D7); **AgentOrchestra/TEA, 89% GAIA** (versioned environment as first-class — D3/D9, and its limit motivating live observation); **Agentic World Modeling** (L1/L2/L3 mixed-mode, "silent drift" names the failure, observe-not-simulate — D12); **Graph-based Agent Memory survey** (bi-temporal vs LWW — the ADR-0049 §A1 motivation); **AriGraph** (episodic graph beats RL — D12); **VMAO** (map-reduce context propagation — D10); the dynamic-workflow survey ("pre-execution generation" = slice 1, "in-execution editing" = slice 2/D10).

**World-modelling corpus (`docs/research/world-modelling/`, the newer 2606.xxxx line — independent corroboration):** **Text World Models** (arXiv:2606.09032) — *code-as-WM (high fidelity) vs LLM-as-WM (high generalization, hallucination-prone)*: validates D12's deterministic world model and locates D9's thin LLM interpretation as the fidelity/trust seam (D13). **Beyond Similarity** (arXiv:2606.06054) + **VESTA** (arXiv:2606.08531, 47.1% ASR) — *memory is a control channel*: the injection/poisoning case for **D13** (Scout findings untrusted into the planner). **WeaveBench** (arXiv:2606.09426, 41.2% frontier PassRate) — *outcome-only grading overestimates on long-horizon tasks*: the case for **trajectory-aware falsification** (§Falsification). **S-Agent / AlloSpatial / MyPCBench** — vision as a first-class channel: the **multimodal non-goal** (D4). **Orchestrated Reality** (arXiv:2606.16014, PDVA + content-hashed deltas) — the explicit-world-model direction A1.2 drift gestures at (future ADR). **SING** (arXiv:2606.16591) — intention-graph tool retrieval at scale: a future evolution of Scout's `find_tools` path (D4) past ~thousands of tools.

## Related
- `docs/requirements/REQ-REACTIVE-PLANNER-GROUNDING.md` (requirements + research); `docs/research/world-modelling/` (the 2606.xxxx corpus driving D13 + the §Context/§Falsification additions).
- ADR-0038 (amended: D4/D8/D9 + Falsification), ADR-0049 (depended-on + amended §A1), ADR-0037 (selection-layer learning — D12), ADR-0043/0044/0048 (reused surfaces), ADR-0029 (ConsolidatorAgent precedent).
- ADR-0012 (TaskEvent + Session event log): Scout's `<DiscoveryLTM>` is a structured write into this log. **ESAA** (arXiv:2602.23193, append-only log + replay verification) independently validates event-sourcing as the right state substrate — the same pattern ADR-0049's rebuildable entity cache and this ADR's write-back rely on.
