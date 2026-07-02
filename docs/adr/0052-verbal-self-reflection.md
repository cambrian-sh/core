# ADR-0052: Verbal Self-Reflection on the Recurrence Gate

**Status:** Implemented (2026-06-23) — SDK-only, behind `reflect_enabled` (default on); 267 Python SDK tests green (8 new in `test_reflection.py`).
**Date:** 2026-06-23
**Author:** Afsin
**Amends:** **ADR-0041** (Local Recurrent Workspace) — extends **D4** (the recurrence gate) with a *verbal* layer; the structural gate is unchanged.
**Depends on:** ADR-0041 (recurrence gate + typed working memory — the hard-veto this hooks, the episodic buffer it reads the failure from), ADR-0048 (working-memory context hygiene — the pinned-entry assembly path), the Zero-Hardcode Rule (`CONTEXT.md` — this adds no routing logic).
**Research:** `docs/research/agent-loops/SUMMARY.md` (gap #1), `docs/research/agent-loops/reflexion-2023.md` (the design source), `docs/research/agent-loops/react-2022.md`. Canonical citation: Shinn et al., "Reflexion: Language Agents with Verbal Reinforcement Learning," arXiv:2303.11381 (2023).

---

## Context

The agent-loops research (`docs/research/agent-loops/`) compared Cambrian's `run_think` ReAct loop against the literature and found Cambrian **at or ahead of consensus** on every axis but a short list of gaps. Of those, exactly one is (a) inside the per-agent loop, (b) fully specified, and (c) flagged as the "natural next step" in *both* foundational paper notes: **verbal self-reflection.**

The recurrence gate (ADR-0041 D4) is a **structural** reflection. It detects a re-issued failing action — exact arg match *or* semantic cosine over cached action embeddings — and runs a local escalation ladder: soft-nudge → hard-veto → escalate. But it never asks the model *why* the action failed or *what* to change; the hard-veto re-prompts with a fixed terse note (`VETOED: X already failed N times…`). Reflexion's empirical finding is that a one-sentence **verbal** reflection, carried into the next attempt, lets an agent learn from failure *within a single run* — the gap the structural counter cannot close on its own.

The other research gaps are deliberately **not** addressed here: per-step **tree search** is marked *intentional / gated on a benchmark* by the research itself; **skills-as-executable-functions** (HASP) is an ADR-0046 + kernel skill-schema change, not a loop fix; **population learning** (FORGE) needs ADR-0049 entity-store write-back (cross-run, not per-agent); **formal semantics** (λ_A) is a proof artifact; **event-sourced loop** (ActiveGraph) is a kernel substrate change. This ADR is scoped to the one in-loop, ready gap.

## Decision

On the recurrence gate's **first hard-veto** for an action, extract a **verbal reflection** and pin it into working memory so every later attempt reads it. Structural gate for the *fast* veto; verbal reflection for *slow* learning — both bounded by the same veto depth. SDK-only, no kernel or schema change.

### D1 — Trigger: the first hard-veto, not every veto

The reflection fires once per failing action signature, when `veto_counts[sig] == 1` (the first hard-veto). This gives the model a chance to change approach *before* escalation, and avoids a second reflection on the same action right before it escalates. Bounded twice over: by `recurrence_veto_depth` (the gate already caps vetoes per signature) and by `max_reflections` (default 3) across the whole run — a thrashing run cannot reflect endlessly.

### D2 — One small, best-effort LLM call (`reflection.py` + `react._request_reflection`)

`reflection.build_reflection_prompt` is **pure** (mirrors `recurrence.py`'s pure detection): it names the role, task, the *specific* failing action with **condensed args** (a multi-KB payload does not bloat the reflection call), the most recent failure message (pulled from the latest matching failed `ToolCard`), and any reflections already recorded this run (so the model does not repeat a lesson). It explicitly forbids emitting JSON / an action.

`react._request_reflection` owns the side-effecting call (mirrors how `react.py` orchestrates around `recurrence.py`): one `substrate.generate` at `max_tokens=200`, `timeout_ms=0`. **Best-effort by construction** — a missing substrate, an RPC failure, an empty response, or a model that ignored the instruction and emitted an action (detected via `parse_action`: a non-`final_answer` result is discarded) all degrade to `""`, and the loop falls back to the terse structural note alone. Never raises into the loop.

### D3 — Storage: a PINNED working-memory entry (`working_memory.add_reflection`)

The lesson is stored as a `<reflection>…</reflection>` `TextEntry` with a new `pinned=True` flag. `WorkingMemory.assemble` always keeps pinned entries (alongside open-failure cards and the most-recent entry) regardless of relevance/recency bounding — an ordinary `<note>` would be bounded out within a few turns; a reflection must persist across every subsequent attempt. Reflections are embedded for relevance like any block but never offloaded (they are short by construction).

### D4 — Default on, one opt-out flag

`reflect_enabled` (default `True`) and `max_reflections` (default 3) are `run_think` parameters, matching how `recurrence_enabled` is on-by-default and unexposed at the `think()` surface. No `base.py` change: reflection is on for every cognitive agent exactly like the recurrence gate. Set `reflect_enabled=False` to get the pre-0052 structural-only behaviour.

## Consequences

**Positive.** Closes the one ready in-loop research gap. The structural gate and verbal reflection are complementary — *what the kernel observes the LLM doing* (the gate) plus *what the LLM says it learned* (the reflection). Cost is bounded: ≤ `max_reflections` extra small LLM calls per run, only on hard-vetoes (a run with no repeated failures pays nothing). Behaviour-preserving on the escalation outcome — the tool still runs exactly twice (novel + one soft-nudge retry) and a stubborn failure still escalates to an honest `type="error"`; the reflection does not relax the gate.

**Negative / residual.** The reflection is **per-run, per-session** — it is not written back to LTM, so there is no *cross-run* learning yet (the FORGE/`LessonsLearned`-lane gap, ADR-0049, remains open and is the natural follow-up: a reflection event is exactly what would seed an experiential `LessonsLearned` lane). The lesson lives only in working memory; it is not stored on the `ToolCard` (the pinned-entry channel was simpler and achieves the behavioural goal — the next attempt reads it). Reflection quality is the model's; a weak model may produce a vacuous lesson, in which case it is harmless filler the bounding eventually deprioritises (it stays pinned, but `max_reflections` caps the count).

## Falsification

A reflection that does not change the model's next action is dead weight. The honest test (future, on the GAIA runner, ADR-0050): an A/B of `reflect_enabled` on/off measuring whether vetoed runs that received a reflection recover (find a different successful action) more often than structural-veto-only runs. Until then this is shipped on the strength of the Reflexion result and the SDK unit tests, behind a default-on flag that is trivially reversible.

## Tests

`python-sdk/tests/test_reflection.py` (8): the pure prompt builder (names the failure, forbids actions, includes prior reflections, condenses huge args); the pinned-entry channel (survives bounding, empty ignored); loop integration (first hard-veto extracts + pins a reflection that reaches a later prompt; `reflect_enabled=False` makes no call; escalation outcome unchanged — tool runs twice, still errors). Full SDK suite green.
