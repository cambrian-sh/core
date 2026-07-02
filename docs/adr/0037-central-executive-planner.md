# ADR-0037: The Central-Executive Planner — Unified Resource Selection over Complementary-Learning Memory

**Status:** Accepted (2026-06-04) — *grilled to a 16-decision design (D1–D16) via `/grill-with-docs`.
The architectural decisions are locked; empirical estimators are explicitly deferred to a benchmark
spike (see Falsification plan). Nothing is implemented yet — the spike is the next artifact.*
**Date:** 2026-06-04
**Author:** Afsin
**Depends on:** ADR-0002 (Hybrid Gatekeeper), ADR-0006 (Capability-Based Orchestration), ADR-0009
(Dispatcher Neural Signal), ADR-0011 (Neuromodulator cost-aware routing), ADR-0013 (TrustScore), ADR-0014
(Thalamic Gating / Capability Clusterer), ADR-0015/0017 (Engram Engine spreading activation), ADR-0018
(Managed LLM Gateway), ADR-0022 (Global Workspace), ADR-0030 (Plan Template Generalizer), ADR-0036
(Trait-Aligned SDK — the `think()` retrieval loop)
**Amends/Supersedes (if accepted):** the **Auctioneer / bid-market** half of the orchestration model
(ADR-0006/0009); and **model routing** (ADR-0011 cost-aware routing + ADR-0018 model selection) — both
folded into one capability-belief + EFE mechanism (D16). The Gatekeeper and Verifier Pool are **retained
and re-pointed**, not removed.
**Scope note:** this ADR unifies **agent** selection *and* **model (TraitModel)** selection under one
resource-selection mechanism (D16). It is a *large* amendment by design — the unification is the point.
**Theory basis:** Complementary Learning Systems (CLS), Baddeley & Hitch Working Memory, Free Energy
Principle (FEP) / Planning-as-Inference, Global Workspace Theory (GWT) / Blackboard Systems, Recurrent
Processing Theory (RPT), Options / Hierarchical RL. See `docs/theory/`.

---

## Context

Cambrian selects agents through an **auction**: agents bid with confidence scores (`RequestProposal`),
the Gatekeeper filters candidates, the Auctioneer picks a winner, and the Verifier Pool scores quality
post-hoc. This is a reward-maximizing *market* mechanism grafted onto an otherwise inference-based
architecture. Two structural problems follow:

1. **The market is an extrinsic layer the system must minimize *around*, not *through*.** Agent
   selection is treated as a competition to be refereed rather than as an inference the orchestrator
   already has the information to perform. Selection logic is split across Auctioneer + Gatekeeper +
   Verifier with weak recurrent coupling to the Planner.
2. **Plans are generated in agent-blind capability-space and only *later* matched to agents.** A
   Planner can emit a step no agent can serve; the auction then finds no qualified bidder and the
   system falls back. Planning is not *grounded* in what capabilities actually exist.

A re-grilling of the orchestration model (2026-06-04) surfaced that **the components needed for an
inference-based alternative already exist** — they are merely wired as a market:

| Existing component | Latent cognitive role |
|---|---|
| Verifier Pool + Provisional lifecycle (ADR-0013/14) | Fast/episodic ("hippocampal") agent store |
| ProfileAggregator EWMA + 2-D TrustScore (ADR-0013) | Slow/statistical precision ("neocortical") |
| CapabilityClusterer (ADR-0014) | Distributed capability **schema** |
| CircadianRhythm consolidator | Offline replay / systems consolidation |
| Engram Engine spreading activation (ADR-0015/17) | The recall mechanism |
| Global Workspace (ADR-0022) | Broadcast / attentional spotlight |
| SDK `seed_recall` think-loop (ADR-0036) | The unified retrieval primitive |

This ADR proposes re-pointing those parts into a **Central-Executive Planner**.

## Considered Options

- **A — Keep the auction (status quo).** Leaves the extrinsic-market and ungrounded-planning problems
  open. Rejected.
- **B — Dissolve everything into one monolithic Planner ("god object").** Maximizes integration (a naive
  IIT reading) but destroys modularity and the future-gatekeeping requirement, and risks fragmenting the
  local recurrent loops the design depends on. Rejected.
- **C — Central-Executive Planner over a Complementary-Learning agent memory; Gatekeeper and Verifier
  retained as modular precision/consolidation operators; the bid demoted to an on-demand epistemic
  action.** Chosen.

## Decision

The Planner becomes a **Central Executive** (Baddeley) that performs **active inference**
(FEP / Planning-as-Inference) over a **Complementary-Learning** agent memory (CLS), retrieving and
binding agents with the **same recall loop** task-agents use. The Auctioneer and the mandatory bid
round are dissolved; the bid survives only as a *solicited epistemic action*. The Gatekeeper and
Verifier Pool are retained as separate, pluggable operators.

### D1 — The Planner is the Central Executive (no storage of its own)

Per Baddeley & Hitch, the Central Executive is a **limited-capacity attentional controller** that holds
no memory itself; it orchestrates buffers and interfaces to LTM. The Planner therefore:
- maintains a **capacity-bounded working set** (the *episodic buffer*) of the current goal + prior step
  results + a bounded candidate-agent set (do **not** load the whole registry — the bound is the point);
- holds **no** authoritative agent state — trust, capability, and history live in the agent memory
  (D2), which it queries.

The bound is **per-active-binding (a single spotlight)**, not across the whole delegation frontier: only
the sub-goal the CE is *currently binding* holds a candidate set. Suspended parents and sibling sub-goals
live on the plan graph (the call-stack analog) holding **no** candidates; on resume their candidates are
**re-retrieved fresh** (cheap — D5's unified recall, and the world may have moved) rather than kept
resident. Total *active* working memory therefore stays bounded regardless of delegation breadth or
depth — preserving Baddeley's single-focus property and avoiding upper-level starvation (see D10–D15).

This directly answers the "monolithic Planner" risk: integration is of the **decision**, not of
**storage** — the subsystems stay separate.

### D2 — Resource memory is a Complementary Learning System; **capability is region-resolved belief**

The registry is reframed as a CLS, resolving the **stability-plasticity dilemma** (cold-start vs.
hard-won trust) that a single store cannot. *Resources* here are **agents and models (TraitModel)** —
both are bound to intents by the same mechanism (D16).

**Capability is not a static label or an embedding neighborhood — it is a precision-weighted belief the
CE holds and updates:** `belief(resource, intent) → (expected_success, confidence)`. This is today's
`ProfileAggregator` TrustScore made **region-resolved** (per capability-region) instead of global — so
"good at comparison, bad at summarization" is representable, and *learned from outcomes*, not inferred
from description similarity. Four layers:

- **Prior** — seeded from **declared affordances verified by the interview** (Provisional→Active);
  low confidence (verified but unproven). For models, the prior is **declared specs + a calibration
  pass** (no interview). This makes a brand-new resource immediately routable, with high uncertainty.
- **Posterior** — `expected_success`/`confidence` **updated from execution outcomes** on intents near
  each region (the Verifier prediction-error of D8/D12). Description-similarity only set the *starting
  point*; the resource **earns** its region by succeeding in it.
- **Index** — region centroids live in the **shared description-embedding space** (one embedder for both
  intent text and resource profile — `DocTypeAgentProfile`); retrieval = nearest belief-regions.
- **Generalization** — **CapabilityClusterer** clusters give a new resource a cluster-level prior (the
  CLS-2016 *schema fast-path*).

The two CLS tiers carry this belief: a **fast store** (provisional resources + recent pattern-separated
episodes, high plasticity — Verifier Pool / Provisional lifecycle) and a **slow store** (consolidated
region-resolved precision + cluster schemas — ProfileAggregator + CapabilityClusterer). The
CircadianRhythm consolidator **interleaves** replayed episodes into the slow store offline, so a few bad
runs cannot catastrophically overwrite established belief.

**Cold-start is solved without a market:** the verified declared-affordance prior makes a new resource
routable immediately at low confidence; FEP epistemic value drives the CE to sample it — **inside the
Provisional sandbox, not on production traffic** (D8 fast-path inhibition retains one-run eviction /
Surveillance, ADR-0013/14). *Why this beats a description-similarity lookup:* belief updates from
**outcomes**, so two topically-similar resources **diverge** as one succeeds and the other fails — the
system learns the functional difference embeddings cannot see.

### D3 — Skeleton-stable, binding-plastic planning (the bounded dynamic DAG)

The stability-plasticity dilemma applied to **plans**:
- **Stable schema:** the Planner first drafts a skeleton of **intents / sub-goals**, *not bound to
  specific agents* ("retrieve → analyze → summarize"). This is durable global structure (an *option*
  sequence; see Options/HRL).
- **Plastic binding:** at each step's turn the Central Executive resolves that intent to a concrete
  agent **via the retrieval loop, conditioned on the prior step's actual output** (the episodic
  buffer). If nothing capable is retrieved, *only that step* re-plans.

A fully static DAG is all-stability (cannot adapt mid-run); per-step full replanning is all-plasticity
(no coherence, costly). Skeleton-stable + binding-plastic is the resolution — the "dynamic DAG", bounded.

**The failure-escalation ladder (what makes "stable" precise).** "Skeleton-stable" is undefined until we
say *when the skeleton itself may change*. A failed step climbs a **deterministic, tiered ladder**, and
**the skeleton is revised only at the top rung**:

1. **Re-bind** (plastic, cheap): try the next-best resource for the *same* intent.
2. **Re-frame the single intent** (local patch, not skeleton revision): if no resource satisfies the
   intent as stated, the CE re-expresses *that one intent* toward a capability-region that has belief
   mass (D2/D4 catalog) and re-binds. This is **bounded** — a local recovery budget + the D15 progress
   guard — so it escalates rather than spins.
3. **Re-plan the skeleton** (structural, expensive) — **only when a *hard* failure invalidates a
   downstream dependency.** A hard failure = no usable output after rungs 1–2 are exhausted; a
   *soft/degraded* output is **not** a structural failure — it propagates as degraded and is gated by the
   Verifier quality threshold (D8). Re-plan is **minimal and forward-only**: it revises only the
   *unexecuted* downstream intents whose `DependsOn` precondition was invalidated (reusing the existing
   `HotSwapPlan` DependsOn-remap), **preserving still-valid completed results** — never a restart.
4. **Fail the plan** (honest): if no skeleton grounded in *available* capabilities (D4) can be produced,
   the system reports it cannot do the task.

The crisp definition this yields: **the skeleton is revised iff a failure breaks a downstream
`DependsOn` precondition** — a *local* failure never touches it. The ladder terminates on **capability
exhaustion or budget exhaustion** (escalation spends tokens; budget is the external knowledge source of
the blackboard model). Per the Zero-Hardcode Rule, the **ladder is deterministic control** (the safe-path
exception — deterministic for safety/latency), while **each rung's content** (which resource, how to
re-frame, the new skeleton) is **inference**: control-flow deterministic, routing zero-hardcode. This is
the top-level analog of D15's option-termination, with an added single-intent re-frame tier.

### D4 — Capability-grounded planning (plans cannot hallucinate impossible steps)

Before drafting, the CE retrieves the **catalog** = the union of capability-regions that have *credible
belief mass across Active resources* (D2) — i.e. "what the system can actually do well **right now**,"
posterior-grounded, **not** an emergent cluster map. The Planner drafts intents in that vocabulary; an
intent landing in a region with **no belief mass** is the impossible step, **structurally unreachable at
generation time**. The capability vocabulary is the *existing* description-embedding space with the
CapabilityClusterer names as labels — so this is a *wiring* change (consult the catalog **pre-draft**),
not a new representation, and it is more Zero-Hardcode-aligned than a hand-maintained taxonomy. This is a
concrete correctness win independent of the theory framing (today such steps are emitted, fail the
auction, and fall back). *Intent granularity* — how specific an intent must be to land cleanly in one
region — is the residual tuning knob (deferred to the spike).

### D5 — One retrieval primitive for agents and the Planner

Agent selection uses the **same** loop a `CognitiveAgent` uses for LTM (`seed_recall` → recall → rank →
inject → refine), via spreading activation over a capability graph (Engram Engine). "One protocol, two
memory stores": a cognitive agent recalls **LTM facts**; the Planner recalls **agent manifests +
performance history**.

### D6 — The Auctioneer dissolves; the bid becomes an epistemic action

The mandatory bid round is removed. A **live `RequestProposal` is solicited only when the Planner's
posterior over candidate agents is flat** (a near-tie / novel step / high expected information gain).
This preserves the one thing a manifest embedding cannot reconstruct — an agent's **input-conditioned
self-assessment** ("do I hold credentials for *this* API?", "is *this* within my context window?") —
while making it *pull, not push*. Soliciting a proposal exactly when uncertain is itself the
FEP-optimal epistemic action.

### D7 — The Gatekeeper is retained as a precision / policy oracle (a blackboard knowledge source)

The Gatekeeper does not compete; it **shapes the Planner's posterior**. Its interface becomes:

```go
// Precision oracle, not a bid collector.
type Gatekeeper interface {
    Evaluate(ctx context.Context, intent domain.Intent,
             candidates []domain.AgentDefinition) ([]PrecisionWeight, error)
}
```

Policy non-compliance ⇒ precision 0 (block); rate-limit ⇒ temporal precision modulation; scope ⇒
candidate-set filtering. Every *future* gatekeeping mechanism is another independent **knowledge source**
on the workspace blackboard (GWT/Blackboard); the Planner never needs to know they exist. This honors
the operator requirement to keep gatekeeping a separate, extensible module.

### D8 — The Verifier Pool becomes the consolidation (precision-learning) signal

Post-execution, the Verifier Pool returns a **prediction-error magnitude** rather than a separate
leaderboard score. Low error ⇒ raise precision; high error ⇒ lower it. The error writes to the **fast
store immediately** (episodic) and is **consolidated into the slow store offline, interleaved** (D2).
The existing **one-run eviction / Surveillance mode** is retained as *fast-path inhibition* on the fast
store — the safety valve for the offline-consolidation lag window.

### D9 — Selection minimizes Expected Free Energy

Given precision-weighted candidates, the Planner selects by minimizing EFE = **pragmatic value**
(expected goal-matching / prediction error) **+ epistemic value** (expected information gain;
uncertainty-driven exploration of under-sampled agents). Operationally this is a precision-weighted,
exploration-bonused choice (a contextual bandit over the CLS store) — see the honesty note below. This
also tightens **Zero-Hardcode** alignment: routing becomes *inference*, not Go `switch`/merit-scoring.

---

## Recursion & Delegation — the Yield Model

D1–D9 read as if the Central Executive (CE) sequences *one* plan. With delegation
(`substrate.execute()`), an agent can spawn further work — turning the bounded dynamic DAG into a
**recursive execution tree**. This section resolves that. The governing choice (D10) is **yield, not
recursion**: there is exactly **one** Central Executive owning an open sub-goal frontier; agents *return*
sub-goals to it rather than spawning their own planners. This is faithful to GWT/Baddeley (one
workspace, one executive), to the untrusted-cells security posture, and to the SDK's `max_workers=1`
process model (ADR-0036 D2 — a blocking sub-call would starve the single worker for the whole sub-plan).

> **Internal reasoning is not delegation.** An agent's own `think()` loop — tool calls, memory recall,
> multiple LLM turns in one process (ADR-0036) — is *not* delegation and is unaffected. Delegation is
> *only* the cross-agent-capability case.

### D10 — Delegation is yield-by-default (result-carried sub-goal + stateless resume)

An agent that needs another capability does **not** block. It returns an `AgentResult` marked as a
**yield**, carrying `{intent, capability_hint?, payload, continuation_state}`:
- **`intent`** — a sub-task description in *capability-space*, **never an agent ID** (agents are blind to
  the agent population; the CE is the sole holder of the registry and the sole binder).
- **`capability_hint`** — optional, advisory; a soft prior into the CE's posterior, subject to Gatekeeper
  precision / scope / cycle rejection. Never authoritative.
- **`continuation_state`** — an **opaque, agent-owned** blob the CE stores and returns verbatim; the CE
  never interprets it (keeps the agent's internals private and the CE thin, honoring D1).

The CE splices the sub-goal into the live plan as a new `Step` (reusing `Step.DependsOn` + `HotSwapPlan`,
which already remap dependencies in a running plan), binds an agent to it via the **standard
capability-grounded selection** (D4–D9), and on completion **re-dispatches the original agent** with
`{sub_result, continuation_state}` injected. No blocking; fits ADR-0036 D2.

*Agent-initiated recursion via a blocking call is a bounded exception only (see D15), not the norm.*

**SDK consequence:** `AgentResult` gains a yield variant + `continuation_state`; the blocking
`SubstrateClient.execute(target=agent_id)` is replaced by `yield_subgoal(intent, capability_hint=None)`.
**No agent IDs cross the SDK boundary.**

### D11 — Episodic buffer inheritance is least-privilege and scope-gated

Because the parent never names a child, the only context that crosses is what the parent **explicitly
puts in the sub-goal**. A bound child therefore sees **only `{intent, payload}`** — *not* the parent's
other step-results or working memory. If the parent wants the child to know *why*, it must **distill
that into the intent** (which is also D12's lever: good decomposition is the parent's delegable act).

The security half is an **invariant, not a filter**: the `{intent, payload}` is **scope-gated to the
child's effective scope** (ADR-0034). The parent cannot smuggle content the child is not authorized to
read; if the only capable agent lacks scope for the required payload, the sub-goal is **unsatisfiable for
that binding** — the CE selects another qualified agent or escalates/fails. "Scope-leak across
delegation" is impossible because **nothing is inherited** — only an explicitly-passed, scope-checked
payload crosses.

### D12 — Capability-specific precision (execution vs. counterfactual decomposition)

Agent-blindness (D10) already eliminates the classic laundering vector — a parent **cannot** route to a
known-good agent because it cannot name one. The residual case is a **lazy pass-through** (yield the
whole task, take credit). Two precision channels close it:
- **Execution precision** → the agent that *did the work*. A child that runs a sub-goal earns it; the
  parent earns execution precision only for steps it executed itself. **The parent never receives
  execution credit for a child's work.**
- **Decomposition precision** → the parent, for **framing quality**, scored **counterfactually as
  information added**: did the sub-goal narrow/clarify the task beyond what the CE already had? A
  pass-through that echoes the parent's own task adds nothing ⇒ ~0 credit; a useful carve-out ⇒ positive.

This makes a *feature*: an agent high in decomposition but low in execution precision is a good
**manager/worker** split (Options/HRL) the CE can route to deliberately. The Verifier Pool (D8) emits
*both* channels as prediction-error signals.

### D13 — Intent lineage is a free byproduct of yield

Because the **one** CE performs every binding and every sub-goal expansion, the intent tree **is** the
CE's own plan graph: each yield records a `Step` with `DependsOn` to its sub-goal plus a `yielded_by`
provenance edge. No distributed, cross-process lineage tracking is needed (it would be required, and
hard, under recursion). D12 credit attribution simply walks this graph — error attributes to the agent
that made the *reframing* (decomposition channel) distinctly from the agent that *executed* (execution
channel).

### D14 — Sub-goal selection prior is the CLS slow/fast split

When the CE binds an agent to a yielded sub-goal, its **starting** precision (distinct from D12's
post-hoc credit) is: **inherit the slow store** (the agent's global trustworthiness carries across) but
**query the fast store fresh** for *this* sub-task's capability (do **not** inherit the parent task's
recent per-task performance). This stops a parent task's bias from leaking into an unrelated sub-task,
and is simply CLS (D2) applied at the binding boundary.

### D15 — Liveness: EFE-monotonic progress + option-termination failure propagation

Recursion-control is **not** a depth counter. Three mechanisms, theory-grounded:
1. **Monotonic progress (liveness).** *Motivation:* each sub-goal should reduce the goal's expected free
   energy. *Mechanism (cheap, hot-path-affordable):* a yielded sub-goal must be a **strict refinement of
   its parent** — its intent embedding must be **narrower than / sufficiently distinct from** the
   parent's by a margin (cosine on the intent texts the CE already embeds for capability retrieval),
   **and/or** it must shrink the open-goal frontier. A sub-goal whose intent is ~identical to its parent
   (the livelock signature: capability-similar agents circulating near-identical intents) **fails the
   guard and is terminated**. True EFE is intractable per sub-goal; this proxy is what actually catches
   the failure mode. The hard depth ceiling (below) remains only as a backstop.
2. **Cycle detection is O(1) under yield.** The CE checks whether a candidate agent already appears in
   the current sub-goal's **ancestry** (a visited-set on its own plan graph) — no distributed stack
   trace across processes (which recursion would require).
3. **Failure propagation = option termination.** Each sub-goal (an *option*, ⟨I, π, β⟩) carries a
   termination condition + a **bounded local recovery budget**; failure propagates to the parent intent
   exactly when *alternatives for that sub-goal are exhausted* (its initiation set for alternatives is
   empty). The parent then re-decides (re-frame / re-bind / fail upward) — not a fixed retry constant.
   A hard depth ceiling remains only as a **safety backstop**, not the primary control.

---

## Universal Resource Selection — Agents *and* Models

### D16 — Capability belief is the universal resource-selection mechanism (agents + models)

Once capability is region-resolved precision (D2), **"which agent" and "which model" are the same
question at different granularities**: bind a *resource* to an *intent* by minimizing EFE over a
capability belief. An **agent** is bound to a plan **step** (CE, D1–D15); a **model (TraitModel)** is
bound to a **generation call**. Both draw from the one CLS store; both update from the same Verifier
prediction-error (D8/D12). TraitModel is therefore **another population in the resource memory**, and
model routing becomes *inference*, not a deterministic cost table — *more* Zero-Hardcode-aligned, not
less. This **subsumes ADR-0011** (neuromodulator cost-aware routing) and **ADR-0018**'s model selection
into the active-inference frame.

Model selection is the **simpler instance** of D2 — do **not** copy the full agent machinery:

- **Prior = declared specs + a one-off calibration pass** over a fixed probe set — *not* an interview
  (models have no Provisional→Active lifecycle).
- **Posterior = generation-quality outcomes** on intent-regions (did this model produce good *code*? good
  *summaries*?), via the same Verifier/judge signal (D8). Region-resolved: "qwen3:8b is high-precision for
  summarization, low for complex code" is **learned**, not declared.
- **Cost is a first-class pragmatic term.** Model EFE =
  `expected_quality × confidence − cost_penalty + epistemic_value`, where `cost_penalty` is exactly
  ADR-0011's neuromodulator (tokens / $ / latency). For agents cost is a tiebreaker; for models it is
  often *the* deciding term.
- **Small, stable population.** A handful of models, not a churning agent set — so pattern-separation
  cold-start drama and the cluster schema fast-path are overkill; a model's belief is a simple
  **per-model / per-region table**, not full episodic machinery. The *mechanism* is shared; the *scale*
  is much smaller.
- **Agents stay blind to models.** Selection is CE/gateway-side (agents already call
  `substrate.generate()` without naming a model — subsuming ADR-0018's gateway routing). No SDK change on
  this axis; agents are blind to *both* the agent and the model population.

**Deferred to the spike (region representation + compositionality).** Whether a capability region is
discretized by cluster (cheap, coarse) or kept continuous per successful-intent with kernel weighting
(fine, costlier), and the prior↔posterior weighting (CLS consolidation / EWMA-α), are tuning choices.
**Compositionality** — composing capabilities into pipelines as *learned options* (Options/HRL,
ADR-0030) — is an explicit deferred research thread, not a blocker.

## Consequences

### Good
- **Closes the ungrounded-plan hole** (D4): impossible steps are unreachable at generation time.
- **Cold-start without a market** (D2): pattern separation + epistemic drive, sandboxed in Provisional.
- **Modularity preserved** (D1/D7/D8): the Central Executive integrates the *decision*; Gatekeeper,
  Verifier, consolidation, and future knowledge sources stay separate and pluggable.
- **Unified retrieval** (D5): one recall protocol; less conceptual surface.
- **Unified resource selection** (D16): one mechanism selects **agents and models** — capability and
  trust collapse into a single *region-resolved* quantity, and cost-aware model routing (ADR-0011/0018)
  folds into the same EFE inference instead of being a separate deterministic table.
- **Capability is learned, not assumed** (D2/Option D): grounded in verified declarations + observed
  successes, so topically-similar resources diverge by outcome — fixing "similarity ≠ capability."
- **Mostly a re-wiring, not a rebuild:** CapabilityClusterer, ProfileAggregator, Verifier Pool, Engram
  spreading activation, Circadian consolidator, Global Workspace, and the SDK `seed_recall` loop already
  exist. The Auctioneer is the principal deletion.

### Bad / Cost
- **Latency.** One upfront *parallel* auction becomes a per-step retrieval (+ occasional proposal
  solicitation). Deep plans compound it. Mitigation: cache retrieval per intent; re-bind only when the
  prior step's result changes the intent.
- **Capacity tuning.** The working-set bound (D1) is empirical — too small misses the right agent, too
  large kills the spotlight. A tuning knob, not a derived constant.
- **Fast/slow disagreement window.** Consolidation is offline; a flaky agent can be epistemically
  sampled faster than its trust corrects. Requires retaining fast-path inhibition (D8).
- **Intent granularity.** Planning in capability-space (D3/D4) is a real representation change; intent
  granularity is the part most likely to need iteration (too abstract ⇒ binding fails; too concrete ⇒
  just renaming an agent).
- **Yield is an SDK breaking change (D10).** `execute(target=...)` → `yield_subgoal(intent, hint)`;
  agents must **externalize `continuation_state`** (no Python locals across a yield) and tolerate
  stateless re-dispatch. A parent that under-distills the intent (D11) gets a worse sub-result — a
  *quality* cost, not a safety one, and diagnosable from the intent audit record.
- **Counterfactual decomposition credit (D12) is harder to compute** than a simple outcome score (it
  needs an "information added vs. the CE's default" estimate). Accepted as the price of blocking
  pass-through laundering.
- **Large amendment surface (D16).** This now touches the auction (0002/0006/0009), trust/clustering
  (0013/0014), gateway + cost routing (0011/0018), *and* the SDK (0036). Higher review/coordination cost
  and a bigger blast radius if wrong. Mitigation: the spike validates the **agent** path first; the model
  re-plumb (D16) lands only after the agent arm clears the falsification gate.

### Neutral / Honesty note
- **Theories motivate the decomposition; they do not validate the win.** "EFE over a CLS store" is,
  mechanically, a contextual bandit with an exploration bonus over a two-timescale memory. The
  decomposition (which modules to keep separate, and why) is the real value; the *behavioral* win must
  be **measured**, not asserted.

## Falsification plan (gate to acceptance)

A/B on the existing `internal/benchmarks` e2e harness — **auction** vs. **central-executive** — on:
(1) plan **success rate**, (2) **quality-judge** score, (3) **latency** p50/p95, (4) **impossible-step**
rate (D4), (5) **cold-start time-to-first-successful-use** of a freshly registered agent (D2). The ADR
is accepted only if the central-executive arm is non-inferior on quality/success and not materially
worse on latency, with a measurable win on (4) and/or (5).

**Delegation-specific metrics (the Yield Model, D10–D15):**
6. **Delegation depth** — max sub-goal nesting reached without livelock/backstop firing; confirms the CE
   handles realistic multi-level delegation (D15).
7. **Semantic drift** — on forced-delegation tasks, LLM-as-judge compares the *original* user intent
   against the *final* result; measures whether reframing across yields drifts the answer (D11/D13).
8. **Precision-laundering** — register a deliberately lazy agent that yields its whole task; assert its
   **execution** precision does **not** rise parasitically and its **decomposition** precision stays
   ~flat (only informative carve-outs earn credit) (D12).
9. **Scope-leak across delegation** — assert a child **never** receives any context outside its effective
   scope; an un-readable payload makes the binding fail rather than down-scope or leak (D11).

**Model-routing metric (D16) — gated *after* the agent arm clears (1)–(5):**
10. **Model-routing quality/cost** — central-executive model selection (capability belief + cost-EFE) vs.
    the ADR-0011 cost-table baseline: equal-or-better output quality at **equal-or-lower** cost, and
    region-resolved learning demonstrably beating a static per-model default (e.g. it learns to stop
    sending complex-code intents to a weak-but-cheap model).

## Related
- `docs/central-executive-planner-synthesis.md` (the design rationale this ADR formalizes)
- `docs/theory/` — CLS, Baddeley-Hitch, FEP, GWT, RPT, and the three added here:
  `Planning_As_Inference.md`, `Options_Hierarchical_RL.md`, `Blackboard_Systems.md`
- ADR-0002/0006/0009 (auction model amended), ADR-0011/0018 (model routing folded into D16),
  ADR-0013/0014 (trust/clustering — re-pointed), ADR-0030 (plan templates ≈ learned options),
  ADR-0036 (the retrieval loop reused)
