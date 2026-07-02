# ADR-0038: Unified Deliberation Loop for Core Cognitive Components

**Status:** Proposed (2026-06-05) — design recorded; not implemented. Gated on a falsification spike (Planner A/B, see Falsification plan), mirroring the ADR-0037 acceptance discipline.
**Amended by ADR-0051 (2026-06-22) — NOT superseded.** ADR-0051 (Grounded Planner — Pre-Plan Discovery) is the first concrete Planner slice and **revises only the three Planner-specific decisions here**: **D4** (the trivial skip-gate is *reversed* — always-Scout, no heuristic skip), **D9** (the Planner grounding axis is *extended* — 0038's loop is retrieve-only over *internal* surfaces; 0051 adds *external world-state* observation via a Scout agent, and moves the loop into that privileged actor rather than the Planner), and **D8** (fan-out is named as a *sanctioned* in-execution-editing exception). The Falsification plan below is superseded by ADR-0051 §Falsification (the fast-path-skip arm is removed).

**This ADR remains in force and is the sole owner of its non-Planner decisions** — **D1** (the one bounded deliberation-loop primitive), **D2** (the loop/no-loop discriminator), **D3/D5/D6/D7** (bounds, progress guard, graceful degradation, per-round auditability), and **D10** (the ConsolidatorAgent retrieve-only variant). ADR-0051 covers none of these; they are unaffected. See `docs/adr/0051-grounded-planner-pre-plan-discovery.md`.
**Date:** 2026-06-05
**Author:** Afsin
**Depends on:** ADR-0036 (`run_think` ReAct loop — the reference design), ADR-0037 (Central-Executive Planner, esp. **D5** "one retrieval primitive for agents and the Planner", D3/D4 catalog + escalation ladder, D15 progress guard), ADR-0016 (WorkspaceStage pre-injection), ADR-0017 (spreading activation), ADR-0012/0029 (ConsolidatorAgent), ADR-0034/0035 (scope/classification — the deterministic exceptions)
**Rationale doc:** `docs/unified-deliberation-loop-review.md`
**Theory basis:** Recurrent Processing Theory (iterate-to-converge), Free Energy Principle / Planning-as-Inference (gather evidence to reduce uncertainty before acting), Global Workspace Theory (one workspace, bounded focus).

---

## Context

Cambrian's task-executing **agents** reason with a bounded reason/retrieve/act loop (`run_think`, ADR-0036): seed context → optionally `memory_query` / `tool_call` → `final_answer`, capped and degrading gracefully. The **core "agent-like" system components**, by contrast, are **one-shot**:

- The **Planner** receives LTM facts *pre-injected* by the WorkspaceStage (ADR-0016) and emits a plan in a single LLM call, with only a degenerate one-retry on an invalid DAG (`planWithValidation`).
- The **ConsolidatorAgent** synthesizes session memory in a single pass.
- The **MemoryAgent** curates via a deterministic pipeline (mask → score → dedup → ingest).

This is an asymmetry: the most consequential reasoning step in the system — **planning** — cannot gather evidence it discovers it needs mid-deliberation. It cannot consult "what can the system actually do" (the ADR-0037 `CapabilityCatalog`), re-frame an unreachable intent, or recall a prior successful plan *conditioned on a partial draft*. ADR-0037 D5 already committed to "one retrieval primitive for agents **and the Planner**"; today that primitive exists only on the agent (Python SDK) side. The Go core has **no shared deliberation-loop primitive** — looping is ad-hoc (`planWithValidation`) or lives only around *execution* (the ADR-0037 failure-escalation ladder), not around *deliberation*.

The operator's request: give the loop to the Planner, and to other core agent-like components **where it makes sense**.

## Considered Options

- **A — Keep one-shot (status quo).** Leaves planning unable to ground itself in available capability or recall mid-draft. The ungrounded-plan problem ADR-0037 D4 fights is only half-solved (the catalog exists but the Planner consults it, at best, once). Rejected.
- **B — Add a loop to every LLM-touching component ("loops everywhere").** Maximizes uniformity but loops deterministic pipelines (curation) and safety paths (classification/scope) that gain nothing, multiplying latency, cost, and non-determinism. Risks a knob-laden god-abstraction. Rejected.
- **C — One bounded deliberation-loop primitive, applied by a discriminator: reasoning components loop, deterministic transforms do not.** Chosen.

## Decision

Introduce a single **bounded deliberation loop** as a shared primitive and apply it **only** to components that make open-ended reasoning judgments, governed by an explicit discriminator. This completes ADR-0037 D5.

### D1 — One deliberation-loop primitive (Go), injected seams

A pure, bounded `(reason → {retrieve | act | finalize})*` loop lives in `internal/domain/` with injected seams — an LLM `Generator`, one or more **retrievers**, and optional **tools/actions** — mirroring the Python SDK `run_think`. It owns no component-specific logic; each component *drives* it with its own retrievers/tools. This preserves hexagonal separation (pure loop + injected dependencies), keeps it DRY (one primitive, several drivers), and makes it testable with fakes (the belief-store / `run_think` test pattern).

> **Not a god object.** The primitive is thin: control flow + budget + termination. All domain specifics live in the injected retrievers/tools. A new knob per component is a smell to reject in review.

### D2 — The discriminator (what loops, what does not)

A component loops **iff** it makes an open-ended judgment that improves with iteratively-gathered evidence. A deterministic transform or a single scored verdict does **not** loop.

| Component | Loop | Form |
|---|---|---|
| **Planner** | Yes | Full reason/retrieve/act (D9) |
| **ConsolidatorAgent** | Yes | Retrieve-only (reason + retrieve; deterministic writes) |
| **MemoryAgent curation** | No | Deterministic pipeline + single scoring call |
| **Classification / scope / write-tags** | Never | Zero-Hardcode deterministic exceptions (ADR-0034/0035) |

The exclusions are **normative**: the ADR forbids looping the deterministic safety paths, so the boundary cannot erode later.

### D3 — Every loop is bounded by iterations and a token budget

Each driver sets a max-round cap and a token budget (as `run_think` caps `max_memory_queries` / `max_tool_rounds`). There is no unbounded deliberation. The Planner — on the hot path of every request — gets tight defaults.

### D4 — Hot-path mitigation: caching + fast-path

> **REVISED by ADR-0051 D2.** The "skip the loop entirely for trivial requests" rule is **rejected**: deciding whether to ground *without having looked* is the same epistemic error the grounded planner fixes (a state-dependent request need not look trivial). ADR-0051 replaces the skip-gate with **always-Scout + staleness-targeting** — the cheap *memory* look always runs; the expensive *world* look fires only for stale/unknown referenced entities (so trivial requests still cost ~one turn, but the decision is grounded in observed staleness, not guessed triviality). Per-intent retrieval caching is retained.

The Planner loop caches retrieval per intent (ADR-0037 already flags per-intent retrieval caching) and **skips the loop entirely for trivial requests** (the `run_think` precedent: no re-query when there is nothing to gather). The loop must not regress p95 latency materially.

### D5 — Progress / livelock guard

A round that adds no new information terminates (reusing the ADR-0037 D15 narrowing/no-progress idea). A deliberation loop can never spin re-querying.

### D6 — Graceful degradation, never crash

On budget/iteration exhaustion the loop emits a best-effort result from what it has gathered — it never raises into the caller. (Cautionary precedent: the `run_think` crash on a malformed intermediate `answer` — a loop must tolerate malformed intermediate state.)

### D7 — Per-round auditability

Each iteration emits a structured `deliberation_round` event (mirroring `react_round`), so a looping Planner is **observable** (Langfuse/telemetry) rather than reintroducing invisible reasoning.

### D8 — Determinism boundary: deliberation may loop, execution may not

> **CLARIFIED by ADR-0051 D10.** "Execution may not mutate" admits the same sanctioned exceptions DAG immutability already carries: **`replan`** and (new) **fan-out / map-node expansion**. Both are *deterministic given prior-step output* and are the documented in-execution-editing class — not a loop leaking into execution. The invariant is unchanged for *arbitrary* mutation; fan-out is an allowed, deterministic expansion.

Looping is confined to the *deliberation* phase that produces an artifact (a plan, a consolidation). Once produced, the artifact is frozen: **DAG immutability** (the post-ADR-0004 safety invariant) is preserved — looping must never leak into execution-time plan mutation.

### D9 — Planner integration reuses existing surfaces

> **EXTENDED by ADR-0051 D7.** D9 grounds the Planner only in **internal capability** (catalog, prior plans — retrieve-only over internal surfaces). It does **not observe the environment**, so it would not catch a wrong-*shape* plan (the helicopter failure: the catalog knows "we have a file-writer," not "3 of 10 sections exist"). ADR-0051 adds the missing **external world-state** axis via **live read-only observation**, and — diverging from "the Planner loops full" — moves that loop into a **privileged Scout agent** (reusing `run_think`) rather than building a tool-running loop in `internal/awareness/`. The Planner stays one-shot over Scout's compact grounded report. D9's internal-capability grounding and 0051's external-observation grounding are complementary halves.

The Planner loop is: draft skeleton → check **`CapabilityCatalog`** reachability (ADR-0037 D4, built in 0037-02) → **re-frame** unreachable intents (the ladder's re-frame rung, 0037-06) → recall prior plans (**Hippocampus**) and LTM (WorkspaceStage) conditioned on the draft → emit. No new retrieval representation — it wires together surfaces that already exist. This generalizes and subsumes the ad-hoc `planWithValidation` one-retry.

### D10 — ConsolidatorAgent is the retrieve-only variant

The Consolidator gains the reason + retrieve halves (gather related episodes / prior consolidations before synthesizing) but **not** arbitrary tool-acting — its writes (edges, episodic store) stay deterministic. Offline and latency-tolerant ⇒ the low-risk second adopter.

## Consequences

### Good
- **Closes the rest of the ungrounded-plan hole** (D9): the Planner can iteratively ground a draft in available capability and prior plans, not just a single pre-injection.
- **One primitive, less surface** (D1): unifies `run_think`, `planWithValidation`, and future component loops behind one tested abstraction; completes ADR-0037 D5.
- **Boundary is explicit** (D2): deterministic safety paths are normatively excluded — no future erosion.
- **Observable** (D7): looping reasoning is traced, not hidden.

### Bad / Cost
- **Latency on the Planner critical path** (D3/D4): more LLM calls per request; mitigated by caching + fast-path + tight bounds, but a real risk to measure.
- **Cost variability**: more tokens per request; meter it (ties into ADR-0037's model-arm cost-EFE).
- **Complexity**: a shared loop is powerful and can attract knobs; D1 deliberately keeps it thin, enforced in review.
- **Non-determinism**: a looping Planner is less reproducible than one-shot; bounded + logged + frozen-after-emit (D8) contains it.

### Neutral / Honesty note
- The win is **measured, not asserted**: "iterate-to-converge" is mechanically a bounded retrieval/refine loop. The behavioral benefit (grounding, fewer impossible/under-specified steps) must clear the gate below before the loop is promoted past the Planner pilot.

## Falsification plan (gate to acceptance)

A/B on `internal/benchmarks` (`-tags e2e`): **one-shot Planner** vs. **deliberation-loop Planner**, on:
1. **Plan quality** (LLM-as-judge) ≥ one-shot.
2. **Impossible-step / under-specified-step rate** measurably lower (D9 grounding).
3. **Plan success rate** ≥ one-shot.
4. **Latency** p50/p95 not materially worse (with caching + fast-path).
5. **Cost** (tokens/request) within an accepted bound.

Accepted for the Planner only if quality/success are non-inferior with a measurable grounding win (2) and no material latency regression (4). The ConsolidatorAgent variant (D10) lands only after the Planner pilot clears. MemoryAgent curation and classification/scope are **out of scope** (D2) and must not adopt the loop.

## Related
- `docs/unified-deliberation-loop-review.md` (the design review this ADR formalizes)
- ADR-0036 (`run_think`), ADR-0037 (Central Executive — D5 retrieval primitive, D4 catalog, D15 progress guard, ladder re-frame), ADR-0016 (WorkspaceStage), ADR-0034/0035 (deterministic exceptions)
