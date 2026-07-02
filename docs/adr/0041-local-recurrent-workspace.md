# ADR-0041: Local Recurrent Workspace (LRW) for the Agent Loop

**Status:** Proposed (2026-06-05) — design recorded; not implemented. Big-bang replacement of the Python SDK `run_think` loop, gated on the LRW test suite (see Testing) holding the existing `run_think` contract green.
**Date:** 2026-06-05
**Author:** Afsin
**Depends on:** ADR-0036 (`run_think` ReAct loop — the design this supersedes), ADR-0022 (Global Workspace: `ContentStore` / `ContextRef` / `assemble_context` — reused for offload), ADR-0037 (Central-Executive Planner — **D3** FailureEscalationLadder, **D10–D15** YieldCoordinator/yield-by-default, EFE), ADR-0039 (kernel-owned tool registry — system-tool result `result_cid` offload is already done kernel-side).
**Relates to:** ADR-0038 (Unified Deliberation Loop). 0038 takes `run_think` as its *reference design* for a shared Go+Python deliberation primitive; LRW **upgrades that reference design**. If 0038 lands, the shared primitive should adopt LRW's structure (typed working memory + recurrent reconciliation). They already agree on the FailureEscalationLadder and the RPT/Free-Energy basis — there is no conflict.
**Theory basis:** Baddeley & Hitch Working Memory (typed slave buffers + episodic binding + a *storage-less* Central Executive), Recurrent Processing Theory (a feedforward sweep is an unconscious reflex; reconciliation requires reentrant loops; **local** RP vs **global** RP), Blackboard Systems (opportunistic, data-directed contribution — delegated to the kernel), Free Energy Principle / Planning-as-Inference (act to reduce expected surprise before committing).

---

## Context

Cambrian's **kernel** is a theory-rich cognitive architecture: Global Workspace (ADR-0022), Complementary Learning Systems (the belief store, ADR-0037), Blackboard knowledge-sources (the Gatekeeper precision oracle), and active-inference selection (EFE). Its **agents**, by contrast, run a theory-poor loop: `run_think` (ADR-0036) keeps a flat, append-only `scratch: List[str]` and **replays the entire history into the prompt every turn**. In Baddeley's terms the agent is the thing his model was invented to replace — the Atkinson-Shiffrin *single passive buffer*.

Three concrete flaws follow (observed in `docs/requirements/AGENTS_RUNNING_CONTEXT.md`):

1. **O(N²) token bloat.** Every round rebuilds the prompt from the whole `scratch`, so each turn replays all prior turns.
2. **No batching of independent sub-steps.** The loop is strictly one-LLM-call-per-action.
3. **No tool-call provenance.** The loop appends `<tool name=…>{result}</tool>` with **no args, no intent, no status** — so the model sees an error but not *what it ran* or *why*, and re-issues near-identical calls (the canonical failure: `find … -size +1048576` then `find … -size +1M`, both timing out).

These are not three bugs; they are three *architectural deficits*: a single undifferentiated buffer (1+3), a linear pipeline where a blackboard belongs (2), and a pure **feedforward sweep** with no reentrant reconciliation (the dumb retries).

### Architectural framing: the agent is *Local* Recurrent Processing

RPT distinguishes **local** recurrent processing (phenomenal, fast, within a region) from **global** recurrent processing extended to executive hubs (access, reportable). This maps onto Cambrian's untrusted-cell / Substrate-as-kernel split exactly:

- **Local RP = one agent** reconciling its own task. Bounded, private, in-process, not reportable to peers.
- **Global RP / Global Workspace = the Substrate** orchestrating many agents (auction, shared workspace, LTM, verification).

The kernel already implements the global half. LRW gives the agent the **local recurrent** half it is missing.

---

## Considered Options

- **A — Keep the flat `run_think` (status quo).** Leaves all three deficits. Rejected.
- **B — Add a bounded working-memory list of `ContextRef`s (the minimal patch).** Fixes 1/3 partially but leaves the loop a feedforward sweep — it does **not** stop the dumb retries, which is the most visible failure. Rejected as insufficient.
- **C — Build an in-agent scheduler/planner for batching.** Duplicates the kernel Planner/DAG, bypasses per-step EFE selection + verification + budget, and violates the single-planner / hexagonal principle (and the Local-RP/Global-RP boundary). Rejected.
- **D — The Local Recurrent Workspace: typed Baddeley working memory + a recurrent reconciliation gate, with batching *delegated to the kernel*.** Chosen.

---

## Decision

Replace `run_think`'s flat scratchpad with a **Local Recurrent Workspace**: a structured, bounded working memory plus a recurrent reflex→reconcile→act gate. The LLM remains a **storage-less Central Executive** — it holds no state between calls; LRW builds the buffers it directs and primes only the slots attention needs. **Pure SDK** (Python): no proto change, no new RPC, and the ADR-0039 firm authorization boundary is untouched. Rolled out as a **big-bang replacement** of the single `run_think` (no parallel legacy loop), gated on the test suite below.

### D1 — Typed working memory replaces the flat scratchpad (Baddeley)

`scratch: List[str]` is replaced by typed buffers, mirroring Baddeley's components:

| Baddeley component | LRW buffer | Holds |
|---|---|---|
| Central Executive | the LLM (no storage) | nothing — directs attention only |
| Phonological loop | reasoning buffer | the agent's own prior thoughts/decisions |
| Visuospatial sketchpad | observation buffer | tool outputs / file contents (offloaded; see D3) |
| Episodic buffer | **invocation cards** | the bound, chronological task narrative |

The **invocation card** is the unit of episodic memory and the provenance fix:

```
ToolCard = { intent, tool, args, status, summary, cid?, bytes, ts }
```

The card carries *what the agent intended*, *what it called*, *with what args*, *the outcome status*, a heuristic `summary`, and (for offloaded results) a `cid`. This is what the Executive reads to know what it has already tried. **Closes flaw #3.**

### D2 — Bounded, relevance-ranked prompt assembly with mandatory pins (attention allocation)

Each turn, LRW assembles the prompt from a **bounded** slice of the buffers (cap configurable, default 10 slots), not the full history. Selection:

- **Mandatory pins (always in-prompt, regardless of relevance):** the originating task/intent, any **open/unresolved failure** card, and the **most recent action's** result.
- **Remaining slots:** ranked by **embedding cosine** similarity of each card to the current sub-intent (the free local `nomic-embed-text`; the intent is embedded once per turn, card vectors are cached at card creation). This requires the agent to embed text, for which the SDK gains **one minimal, read-only `substrate.embed(text) → vector` RPC** (the kernel already owns the embedder) — the single, deliberate exception to D6's "no new RPC" (see D6).
- Evicted cards leave the *prompt* but remain retrievable by `cid` (D3).

This is the Central Executive's defining job — *strategic attention allocation* — done as actual relevance, with pins so a low-cosine-but-critical card (classically, the original error) cannot scroll out and starve the recurrence gate. Relevance ranking here is **Local** scope (this task's own observations) and does **not** duplicate the kernel's `PrimeForStep` (LTM / Global scope). **Closes flaw #1** (O(N²) → ~O(k)).

### D3 — Offload heavy results, keep `{summary + cid}` (reuse ADR-0022)

Heavy results are not inlined:

- **System-tool results are already offloaded kernel-side** (ADR-0039 `ToolExecutor` returns `result_cid` above `InlineThreshold`); the card keeps `{summary + cid}` for free. Full payload is one `substrate.get_context_node(cid)` away — and that on-demand retrieval *is* the recurrent "drill-down" when the agent actually needs detail.
- **Local `@tool` results** are in-process Python values, usually small — summarized inline.
- **Summary policy:** **heuristic by default** (status line + head(N) + byte size + result kind), **no LLM call**. An **opt-in** path (per-tool flag or an oversized-result trigger) may LLM-summarize a specific tool's results. We do not pay an LLM-summary tax on every observation — that would reintroduce the cost we are cutting.

### D4 — The recurrent reconciliation gate (RPT + local active inference)

The loop becomes **reflex → reconcile → act** instead of act:

1. **Reflex (feedforward):** propose an action from the current buffers.
2. **Reconcile (reentrant):** before committing a `tool_call`, check the episodic buffer for a **near-duplicate prior failure**:
   - **Detection = exact arg-hash** (identical retries) **+ semantic cosine** over cached action embeddings (cosmetic-variation retries — the `+1048576` vs `+1M` case that exact-hashing alone misses).
3. **Act** only after reconciliation.

On a detected near-duplicate, a **local escalation ladder** (mirroring ADR-0037 D3 `ReBind → ReFrame → RePlan → Fail` at Local-RP scale):

- **1st near-duplicate → soft nudge:** inject a salient card ("you already tried X → failed with Y; a repeat is unlikely to help — change approach or escalate") and let the Executive decide. Preserves agent autonomy and allows **one legitimate retry** of a transiently-failing tool (e.g., a slow command).
- **Repeat anyway → hard veto:** the action is blocked; the loop forces a re-frame, or escalates by **yielding a `SubGoal`** or emitting `final_answer` with an honest failure.

The gate is **guarded/tunable** (cosine threshold + ladder depth as config) so a false-positive veto can be relaxed without a redeploy. This is the layer that actually stops the dumb retries; it is *local active inference* — act to reduce expected surprise. **Addresses the observed retry failure.**

### D5 — Batching is delegated to the kernel, never executed in the agent (Blackboard → Global RP)

The agent does **not** schedule or parallel-execute sub-steps. Two cases:

- **Trivial reasoning that was over-decomposed into tool calls** (e.g., `R*R*π`) is just the Executive *thinking* — it stays in one reasoning pass, not five tool calls. Much of flaw #2 dissolves here, via prompt/buffer design.
- **Genuinely independent, expensive sub-tasks** are **decomposed and delegated to the kernel** through the existing `yield_subgoal()` / `substrate.execute()` path. The kernel's Planner/DAGExecutor already runs independent steps in parallel with per-step EFE selection, verification, and budget — that is Global RP and the single planner of record.

**Yield is now wired (0041-07, 2026-06-05):** the post-LRW audit found the `YieldCoordinator` (ADR-0037 D10–D15) had never been composed — agents could *emit* a yield but nothing kernel-side consumed it. A **synchronous `YieldDriver`** (`internal/centralexec/yield_driver.go`, injected binder/caller seams, narrowing + cycle guards, `MaxDepth`) now binds + dispatches a yielded sub-goal via the live selector and **resumes** the parent (delegate-and-continue; `run_think` seeds a `<delegated>` card). The agent worker is freed on each yield, so this is faithful to D10 without an async refactor. Wired on the EFE dispatch path (`selectViaEFE`).

**Out of scope (deferred to a follow-up ADR):** async **parallel fan-out** (multiple sub-goals in flight, true frontier scheduling / DAGExecutor suspend-resume), and driving yields on the auction-fallback path.

### D6 — Pure SDK, big-bang replacement

- All of D1–D4 lives in the Python SDK (`react.py` + new `working_memory` / `recurrence` modules). **The one new kernel surface is a minimal, read-only `Embed(text) → vector` RPC** required by D2's relevance ranking (the kernel already owns the embedder; the agent has none). No other proto change, no write RPC, and the firm authorization boundary (ADR-0039 A1.4) is untouched. Consequence accepted: the kernel cannot observe/audit the agent's internal buffers (they never cross gRPC) — acceptable, since the scratchpad was never kernel-visible either.
- `run_think` is **replaced in place** (one loop, no parallel legacy path), so all `CognitiveAgent`s adopt LRW at once. The blast radius is mitigated by the Testing section, and the behavior-changing recurrence gate ships guarded.

---

## Consequences

**Positive**

- Token cost per step drops from O(N²) to ~O(k); long agent tasks become affordable on local models.
- The agent stops re-issuing failed actions; failures escalate or re-frame instead of spinning.
- Tool provenance gives the Executive causal continuity (what/why/outcome), improving correction quality.
- One mental model at two scales: Local RP (agent) inside Global RP (kernel); local escalation ladder mirroring the kernel's; the agent loop finally *reuses* kernel primitives instead of being an outlier.

**Negative / risks**

- Big-bang replacement: a regression hits every agent at once (mitigated by tests + the guarded gate).
- Heuristic summaries can be lossy (mitigated by the `cid` drill-down and the opt-in LLM summary).
- The recurrence gate can false-positive and veto a legitimate retry (mitigated by the soft-nudge-first ladder + tunable threshold).
- No kernel-side observability of LRW state (accepted; revisit if debugging demands a telemetry surface).

---

## Testing decisions

The big-bang raises the bar: LRW must hold the **existing `run_think` behavioral contract** green and add LRW-specific coverage. Tests assert *external behavior*, not buffer internals.

- **Contract preservation:** the full existing `test_think.py` suite (final-answer parse, memory-query budget/degradation, tool-call routing local vs system, tool-round cap → typed error, system-tool degrade) passes unchanged.
- **Provenance (D1):** after a tool call, the assembled prompt contains an invocation card with tool + args + status (not just the result blob).
- **Bounded assembly (D2):** with > cap cards, the prompt holds ≤ cap, always includes the pinned set (origin/open-failure/last), and the prompt does **not** grow unboundedly across N rounds (token-bound regression — the O(N²) guard).
- **Offload (D3):** a heavy system-tool result is represented in-prompt by `{summary + cid}`, not the full payload; `get_context_node(cid)` retrieves the full content.
- **Recurrence gate, both directions (D4):** (a) a near-duplicate of a *failed* action triggers the ladder (soft nudge then veto); (b) a near-duplicate of a *succeeded* action, or a non-duplicate, is **not** vetoed (no false positive); (c) `+1048576` vs `+1M` is detected as a near-duplicate (semantic, not just hash).
- **Delegation (D5):** an over-decomposed trivial calculation resolves without N tool calls; an independent expensive sub-task yields a `SubGoal` rather than being scheduled in-agent.

Prior art: the `run_think` fakes (`_FakeLLM`, `_FakeMemory`, `ToolBot`) and the belief-store/`run_think` injected-seam test pattern.

---

## Falsification

LRW is accepted when, on the local benchmark suite: (1) the `run_think` contract suite is green; (2) median prompt tokens/step on a ≥5-round task drop materially versus the flat loop; (3) the observed retry-storm cases (duplicate `execute_command` failures) no longer recur. Until then the status remains **Proposed**, mirroring the ADR-0037/0038 acceptance discipline.

**Verification status (2026-06-05 — all six slices 0041-01…06 implemented):** the test-level proxies for the three criteria pass — (1) the full pre-LRW `test_think.py` behavioral contract is green against the LRW loop (211 SDK tests; Go `Embed` handler + tool-process e2e green); (2) `test_token_bound_regression_caps_prompt_below_round_count` shows the assembled prompt is bounded to ≤ cap cards, strictly below the round count the flat loop replayed; (3) `test_recurrence_gate_soft_nudge_then_veto_then_escalate` shows a re-issued failing action is stopped and escalated rather than spun. The remaining acceptance gate is the **live benchmark** (Ollama + kernel) measuring median tokens/step and confirming retry-storms are gone in production — not runnable from unit tests, so the status stays **Proposed**.

## Out of scope

- YieldCoordinator parallel **fan-out** (multiple sub-goals/turn) — follow-up ADR (kernel change).
- LLM-based summarization as the default (kept opt-in).
- Kernel-side observability/telemetry of LRW buffers.
- Relevance ranking over LTM/cross-session content — that remains the kernel's `PrimeForStep` (Global RP).
- Folding LRW into the ADR-0038 shared Go+Python deliberation primitive — coordinated there if/when 0038 is implemented.
