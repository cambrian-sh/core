---
id: 0094
title: The Procedural Tier — Induced Routines, Capability-Tagged and Advisory
status: Proposed
date: 2026-07-27
supersedes: []
superseded_by: []
amends:
  - 0030-active-learning-plan-template-generalizer
depends_on:
  - 0049-experiential-memory-world-model
  - 0095-experiences-as-first-class-entities
  - 0067-capability-vocabulary-canonicalization
  - 0054-multi-signal-ranking
  - 0093-document-entity-and-table-split
  - 0035-kernel-derived-write-classification
  - 0002-hybrid-gatekeeper
  - 0046-agent-skills
---

# ADR-0094: The Procedural Tier

## Status

**Proposed — implemented behind a default-off arm; first exercised on real data 2026-07-28.**

Amended twice since: **A1** re-keyed D3's clustering (0 → 8 clusters on the first real
corpus), **A2** wired D8's outcome loop, which had been declared and read but never
written. Read both before treating any D-number as describing live behaviour.

Built (2026-07-28): D1 record shape, D2 capability-tagging with `NamesAnyAgent` as an
assertable invariant, D3 deterministic clustering + one bounded naturalisation call that
FAILS OPEN to mechanical intents, D4 lifecycle (slow-consolidation confidence,
deprecate-not-delete, supersede), D5 the fourth recall lane, D7 population-wide with
attribution, D8 the erosion guard, and the offline scheduler ADR-0049 §A2.5 permits.

D8's guard was in place from the start but received no signal until A2 closed the loop;
until then no routine's confidence ever moved and none was ever deprecated.

NOT built: D6's per-fire authority clipping beyond the advisory contract, and the
`experience_derivations` compatibility CHECK at induction time is enforced by refusing
mixed-classification clusters rather than by re-reading the link table.

**No procedure has ever been induced.** The tier cannot produce one until plans
accumulate under the `capability_contract` arm, so every claim here is unit-verified and
none is field-verified. `procedure_induction_interval_hours` defaults to 0 (disabled).

## Context

The memory literature has converged on a three-way split — **episodic** (what happened), **semantic** (what is true), **procedural** (how to do it) — and on one finding that is close to unanimous across 2026: **procedural abstractions transfer across tasks where episodic trajectories do not.** arXiv:2604.27003 finds abstract procedural memories transfer more reliably than detailed trajectory recordings and that instance-level storage produces negative transfer that disproportionately damages hard cases; arXiv:2606.04703 finds principle-level experience more durable than instance-level. Agent Workflow Memory (arXiv:2409.07429), Memp (arXiv:2508.06433) and ACE (arXiv:2510.04618) all store induced routines rather than the runs that produced them.

Cambrian's coverage of that triple is lopsided. Semantic memory (the ingested corpus) is the most measured and tuned part of the system. Episodic memory is fully built and dark since 2026-07-18 (ADR-0049 §A2.0). **Procedural memory half-exists, and is the least examined.**

### What already exists, and where it stops

ADR-0030 (`Implemented`) gave the `Hippocampus` an active-learning generalizer: successful `ExecutionPlan`s are stored as `DocTypeProceduralTemplate`, grouped by `plan_hash = SHA-256(normalise(canonicalKey))` with a **cosine ≥ 0.93** fallback for LLM text drift, promoted to `is_template: true` after repeated success, and suppressed via a `blacklisted: true` flag when they reliably fail. `Hippocampus.Store` was unwired on 2026-07-18 along with the rest of the experiential write path.

That machinery is sound and this ADR keeps it. Two properties of its **representation** are what stop it being a procedural tier:

1. **A template is a concrete plan, keyed by plan text.** Grouping is on the canonical key — the plan's own natural-language subject and steps. The 0.93 threshold is deliberately tight, and correctly so: with text keys, anything looser falsely groups "semantically similar but structurally different" plans (ADR-0030 §1). The consequence is that a template can only ever capture a **near-verbatim repeat**. It cannot recognise that "ingest a PDF, extract its tables, write a summary file" and "ingest a spec, extract its endpoints, write a client stub" are the *same routine*. Generalisation across surface form is exactly what the tier is for, and text keys structurally cannot deliver it.
2. **Steps carry no capability tags.** When ADR-0030 was written, `Step` carried only free-text `Query` — routing defect D1. That is now closed: `Step.RequiredCapabilities` exists (ROUTE-03 / ADR-0067), emitted from the live capability vocabulary under the `capability_contract` arm. A procedural record can now be keyed on *what capability each step needs* rather than on what the planner happened to write.

So the tier is not missing. It is **instance-level, and needs to become abstract.**

### The constraint that shapes everything below

A procedural memory is a description of *how work gets done*. In a system whose central rule is that agent-to-task routing must live in the Awareness layer and never in authored tables, a stored routine is one careless decision away from becoming **a learned hardcoded routing table** — the Zero-Hardcode Rule defeated not by a Go `switch` but by a database row. D2 is the decision that prevents this, and it is the load-bearing decision of this ADR.

## Decision

### D1 — A procedure is a typed record keyed by situation, not by plan text

New document type `mnemonic_procedure`, with the following shape:

| Field | Content |
|---|---|
| `trigger` | The **ADR-0049 D7 situation projection** — goal shape + abstracted entity roles/types + environment kind. **This is the embedding subject.** |
| `steps` | Ordered list of `{required_capabilities: []string, intent: string, depends_on: []int}` — see D2 |
| `preconditions` | Deterministically-derived entity-kind and environment requirements |
| `known_failure_modes` | Links to ADR-0049 failure precedents observed while following this routine |
| `provenance` | Source plan IDs, contributing agent IDs (**attribution only**), induction time, sample count |
| `confidence` | Derived from sample count and outcome rate under CLS-style slow consolidation (D8) |
| `status` | `active` \| `deprecated` \| `superseded_by:<id>` |

**You retrieve by situation and act on steps** — the trigger is embedded, the steps are not. This mirrors ADR-0049 D7's reasoning exactly, including the part that decision got right and that ADR-0030's text keys could not: *specific entity IDs are excluded from the similarity key*, because embedding them makes every record unique and breaks similarity. Keying on the abstracted trigger is what lets the PDF routine and the spec routine group as one procedure where a 0.93 text-cosine never could.

### D2 — A procedure names **capabilities**, never agents

**The Zero-Hardcode decision.** Each step carries `required_capabilities` drawn from the canonical vocabulary (ADR-0067 `NormalizeCapability`) and a natural-language `intent`. A procedure **must never** record which agent executed a step, except in `provenance` for attribution, which is never read at routing time.

Consequences, all of them the point:

- A retrieved procedure is **planner input** — a suggested plan *shape*. The Gatekeeper still filters and the Auctioneer still selects who executes each step. Selection remains merit-based and live.
- A procedure keeps working when the fleet changes. Agents can be added, removed or renamed and the routine still resolves, because it names what is needed, not who did it.
- The failure mode is named and excluded: a procedure that stored agent IDs would, on retrieval, hand the planner a pre-decided assignment — an authored routing table that learned itself. That is a Zero-Hardcode violation regardless of the fact that no Go conditional is involved, and it is not one of the three sanctioned exceptions.

This decision is why a procedural tier is safe in Cambrian specifically, and it is not negotiable in the way the tunables below are.

### D3 — Induction is deterministic detection plus one bounded LLM call

Runs in the batch pass that ADR-0049 §A2.5 now permits. Three stages:

1. **Candidate detection — deterministic, no LLM.** Cluster ADR-0049 outcome records on (a) trigger-projection similarity and (b) **capability-sequence shape** (edit distance over each plan's ordered `required_capabilities`). Two axes, because either alone over-groups: similar situations solved differently are not one routine, and identical capability sequences in unrelated situations are not either.
2. **Promotion threshold.** A cluster becomes a candidate at ≥ *k* successful occurrences with no active procedure already covering it. ADR-0030's promotion counting and `is_template` guard carry over unchanged; only the grouping key changes.
3. **Naturalisation — exactly one LLM call per candidate procedure.** Writes the human-readable routine and step intents from the clustered evidence. **Per-procedure, not per-plan and not per-chunk** — this is what keeps the tier inside the kernel's "no LLM at chunk granularity" rule (LLM calls are per-query or per-memory only). Cost scales with the number of *distinct routines*, which is small and saturating, not with corpus or traffic.

The LLM never decides *whether* something is a procedure, only how to phrase one the deterministic stage already identified.

### D4 — Lifecycle is build / retrieve / update / **deprecate**

Memory that is only ever appended rots (arXiv:2508.06433).

- **Update by delta, never by rewrite.** New evidence adjusts confidence, appends a failure mode, or refines a step intent. Wholesale regeneration is forbidden: iterative rewriting is the documented mechanism of **context collapse** and **brevity bias** (arXiv:2510.04618), where each pass quietly erodes the specifics that made the routine useful.
- **Deprecate, don't delete.** A procedure that repeatedly fails when followed moves to `deprecated` and stops being retrieved. This generalises ADR-0030's `blacklisted: true` flag, and keeps the record for the same reason a rejected arm is kept: it is evidence.
- **Supersede explicitly.** A better routine for the same trigger links `superseded_by`; the old one is retained and unretrieved.

### D5 — Procedures are a fourth recall lane, subject to the A2.4 lane rules

Push to the planner via `PrimeForPlanning` as a `<ProcedureLTM>` block, and pull for agents via a `recall_procedures` lane — the same push/pull split ADR-0049 D11 established for precedents, and for the same reason (`PrimeForStep` stays dead).

Retrieval is **similarity-gated** (no trigger match above the floor ⇒ no procedure, never a fabricated one) and **non-displacing** per ADR-0049 §A2.4: a procedure may never evict a corpus primary from a knowledge query. Storage stays in `chunks` with every other recall type, per ADR-0093 D2 — this ADR adds a lane, not a table.

### D6 — Procedures are advisory and can never widen authority

- **Advisory.** A procedure is a suggestion to the planner. It never auto-executes, and it never bypasses the auction.
- **Never sole grounding.** A procedure alone cannot justify an action, for the same reason ADR-0049 §A2.8 constrains precedents: it is self-generated content in a closed write→retrieve→act loop.
- **Authority is inherited and may only be narrowed.** A procedure carries no grants of its own. Following one confers exactly the scope the executing agent already had — mirroring ADR-0035, where an agent may only narrow write tags, never broaden them. A stored routine must not become a privilege-escalation path ("the procedure says to run this").
- Provenance is kernel-stamped and unforgeable. An agent cannot author a procedure directly; procedures exist only as output of D3's induction over kernel-recorded outcomes. This closes the write channel that arXiv:2512.16962 (MemoryGraft) exploits — implanting a malicious *successful experience* so the agent replicates it.
- **Induction never crosses a classification boundary — this is ADR-0095 D9 applied, not a rule of its own.** A procedure is distilled from many experiences (D3), so induction over a mixed set would produce an artifact carrying none of its sources' restrictions. D9 governs every derivation in the system (`chunk_triplets`, theme clustering, scene generation, session narratives) and states it once: a derived artifact inherits its sources' restrictions, and derivation across a closed-tag boundary is refused rather than unioned. What is specific to procedures is only that ADR-0095 D5's `experience_derivations` link table enumerates the sources, so here the rule is **checkable rather than implied**.

### D7 — Procedures are population-wide, with attribution

Every induced procedure is visible to the whole fleet, tagged with the agents whose runs produced it. No per-agent private procedure libraries — the reasoning is recorded in ADR-0049 §A2.6, and the gain is the one arXiv:2606.19911 measures: without a shared repository, each newly spawned agent rediscovers what the fleet already knows.

Visibility remains governed by the existing access-policy machinery (ADR-0034/0085). This ADR introduces no new access mechanism.

### D8 — Confidence resists single runs (erosion guard)

Procedure confidence updates through a **slow-consolidation rate**, deliberately borrowing the stability half of the CLS design already implemented in `internal/centralexec/belief/store.go`, where a small consolidation rate exists precisely so "a few bad runs fail to catastrophically overwrite established belief."

This is the direct answer to arXiv:2605.09315, which finds **capability erosion under self-evolution** across all four evolution channels — including memory — and shows that explicit preservation, not just acquisition, is what makes long-horizon self-improvement stable. A procedural tier without this guard is a mechanism for forgetting things that worked.

### D9 — Procedures are distinct from Skills, and the boundary is authorship

ADR-0046 Skills and ADR-0094 Procedures both describe "how to do something" and must not blur:

| | **Skill** (ADR-0046) | **Procedure** (this ADR) |
|---|---|---|
| Origin | **Authored** by a human | **Induced** from observed outcomes |
| Authority | Normative — this is how it *should* be done | Descriptive — this is how it *has* gone |
| Trust | Testimony; trusted on authorship | Observation; trusted on sample count and outcome |
| Retrieval | Semantic tool/skill retrieval (ADR-0044/0046) | The D5 procedure lane |

A procedure never overrides a skill. Where both match, the skill is normative and the procedure is evidence about it — including, usefully, evidence that the documented way keeps failing.

## Rejected alternatives

| Alternative | Why rejected |
|---|---|
| **Per-agent private procedure libraries** | Breaks merit portability and creates incumbency lock-in; unaudited write channel; forces rediscovery by new agents (ADR-0049 §A2.6). |
| **Procedures that name agents** | A learned hardcoded routing table. Defeats the Auction model without a single Go conditional. See D2. |
| **Keep ADR-0030's plan-text keys, just loosen the threshold** | ADR-0030 §1 is right that loosening text-cosine falsely groups structurally different plans. The fix is a better key (D1), not a weaker threshold on a bad one. |
| **One LLM call per trajectory to induce routines** | Violates "no LLM at chunk granularity"; cost scales with traffic rather than with the number of distinct routines. D3 inverts this. |
| **Auto-execute a high-confidence procedure, skipping the planner** | Removes the auction and the plan freeze from the path that most needs them. Procedures are advisory (D6). |
| **Distil procedures into model weights** (arXiv:2607.21051) | Genuinely strong results (≥64.8% of in-context gains retained, ≥9.6× RL sample efficiency), but Cambrian's models are API-accessed and swappable. Out of scope; noted for the record. |
| **Regenerate a procedure wholesale on new evidence** | Context collapse and brevity bias (arXiv:2510.04618). Delta updates only (D4). |

## Benchmark gate

Per the DDD mandate and `docs/backlog/INDEX.md` cross-cutting rules:

- **Blocked on ADR-0049 §A2.7 Phase 0.** No procedural work starts before a memory benchmark exists and a corpus-recall baseline is recorded with the experiential lane off.
- **One arm** — `procedural_memory` — against `current-kernel`, with `bypass_auction` as the second control. The auction control matters unusually much here: the whole claim of D2 is that procedures improve plan *shape* without touching selection, and the bypass arm is what separates those two effects.
- **Offline-before-online.** Induction must demonstrably produce sensible routines over logged outcome records before any procedure reaches `PrimeForPlanning`.
- **Primary metric:** task success on the orchestration/e2e suites. **Guard metric:** corpus recall must not regress (defect 4, ADR-0049 §A2.1). **Tier metric:** repeat-task efficiency — a routine's second execution should cost fewer steps than its first, which is the only thing that distinguishes a procedural tier from a slower cache.
- Gate decision → DECISIONS.md with run-manifest IDs for both arms. A rejected arm is a result.

## Consequences

**What becomes possible.** The fleet accumulates transferable know-how rather than per-agent habit: a newly spawned agent inherits the population's routines on first dispatch. Repeat work gets cheaper in steps, not just in cache hits. And the system gains a representation for "how this is done here" that survives fleet changes, because it is written in capabilities rather than names.

**What becomes harder.** There is now a fourth recall lane to rank and protect, and the lane-aware non-displacing truncation (ADR-0049 §A2.4) must land first or the tier will quietly eat corpus recall. The batch pass is new operational surface, constrained by A2.5 so nothing load-bearing depends on it. And the tier depends on the `capability_contract` arm being on: with `Step.RequiredCapabilities` empty, D3's capability-sequence clustering degrades to trigger-similarity alone, which over-groups. That dependency is real and is stated rather than designed around.

**Residual / deferred.**
- Cross-domain transfer (does a routine induced in one repo help in another?) is unmeasured and probably the most interesting open question the tier raises.
- Procedure decay: procedures currently age only through confidence, with no λ of their own.
- Weight-level distillation (rejected above) if the model story ever changes.

## Open questions

1. **Own-experience ranking — soft or hard?** Carried forward from ADR-0049 §A2.6 and still undecided: whether an agent's own prior experience and procedures are preferred by a soft provenance term in the ADR-0054 blend or a hard per-agent filter. Both satisfy A2.6. The soft option preserves the population-learning benefit that motivates D7; the hard option is simpler and eliminates cross-session bleed outright. This is a ranking change and therefore measurement-gated — it should be resolved by the Phase 0 rig, not by argument.
2. **Promotion threshold *k*.** Benchmark-derived, not editorial.
3. **Does a deprecated procedure suppress its cluster from re-induction**, or may a genuinely better routine re-form over the same trigger? Currently unspecified; re-formation seems right but wants evidence.

## Related

- ADR-0049 §A2 — the write granularity, prediction-error gate, lane rules and batch-consolidation permission this ADR builds on
- ADR-0030 — the generalizer machinery this ADR keeps and re-keys
- ADR-0067 / ROUTE-03 — the capability vocabulary D2 depends on
- ADR-0046 — Skills, and the authorship boundary in D9
- `docs/research/experiential-memory/SUMMARY.md` — the literature review behind these decisions

---

## Amendment A1 — the D3 clustering key (2026-07-28)

**Status:** Implemented. Amends D3's clustering key only; the three-stage
deterministic-cluster → threshold → one-LLM-naturalisation structure is unchanged.

### Why

D3 keyed clusters on the ordered capability sequence plus the situation projection,
compared after lowercasing and whitespace collapse. Measured against the first real
corpus the kernel produced — 19 stored `mnemonic_scene` records, 15 successful — that key
yielded **19 distinct clusters and zero promotions at `min_samples: 2`**. Every episode
was its own routine. No amount of additional benchmark volume would have changed that:
the key was incapable of matching two runs of the same task.

Two independent causes, both only visible on real data:

1. **The trigger embedded volatile specifics.** The projection is
   `goal: <planner's free-text subject> | engages: N directory, M file`, and the subject
   names the concrete file, the per-run sandbox path and any quoted literal — *"Create and
   verify notes/alpha.md in runs/rt_diag6/workspace"*. Nine runs of one benchmark family
   produced nine different triggers. The counts in `engages:` were volatile too (the same
   task engaged 1 directory one run and 5 the next, depending on how the plan decomposed).

2. **The capability sequence was not stable.** The same family produced
   `["file_read","file_read"]`, `["file_read","file_read","file_read"]`,
   `["file_read+general_purpose","file_read"]` and
   `["general_purpose","general_purpose+file_read","file_read"]`. The planner does not
   decompose the same request into the same steps twice, so *order and arity were noise*.

### Decision

**A1.1 — cluster on the capability SET.** `capabilitySignature` splits `a+b` combos,
deduplicates, sorts and joins. The ordered sequence survives on
`ProcedureCandidate.Sequence` and is what `ToProcedure` builds steps from: the signature
answers *"is this the same routine?"*, the sequence answers *"what does it do?"*.
Collapsing the key must not also collapse the body, or every routine degenerates to one
step per distinct capability.

**A1.2 — normalize the trigger properly.** Drop path-like tokens, filenames, quoted
literals and bare digits; drop stopwords; stem the handful of suffixes that make
`verify`/`verification` look like different situations; compare the remainder as a sorted
SET, because word order in a prose goal is phrasing, not situation.

The original comment justified the crude version on the grounds that *"anything cleverer
(stemming, embedding) would make clustering non-deterministic or LLM-dependent"*. That
conflated **clever** with **non-deterministic**. Stemming is a pure function; embedding
similarity is not (a threshold makes the result depend on what else is in the batch).
A1.2 takes the first and still refuses the second — this remains exact-match clustering
on a deterministic key.

**A1.3 — canonicalise the projection at generation.** `sceneProjection` now strips paths
and quoted literals from the goal before storing. The projection is the *abstracted
retrieval face* and is what gets embedded; leaving absolute paths in it indexed situations
by a run id that will never recur. Deliberately conservative — filenames and prose stay,
because for a real request ("summarise q3.pdf") the filename is part of the situation, and
the concrete refs are already recorded by reference in `engaged`.

### Measured effect

On the real 15-success corpus, at `min_samples: 2`, each half attributed:

| key | clusters | episodes clustered |
|---|---|---|
| ordered sequence + crude trigger (original D3) | 0 | 0 |
| ordered sequence + A1.2 trigger | 1 | 3 |
| **capability set + A1.2 trigger (A1)** | **2** | **5** |

Both halves are load-bearing and both numbers are asserted in
`internal/memory/procedure_realdata_test.go`, which hardcodes the real stored scenes
verbatim. Mutation-checked: reverting A1.2 returns the corpus to zero clusters.

### The risk this took, and the guard

Loosening a key trades false negatives for false positives. The specific danger here is
that both benchmark families engage files under the same coarse capability tags
(`file_read`, `general_purpose`), so a key built from structure alone would fuse
*verify-what-you-wrote* with *summarise-into-a-new-file* — precisely the over-grouping D3
warns about. It does not: the goal tokens still separate them, and the test asserts no
cluster contains both. That assertion is the guard on future loosening.

**Still true:** with `capability_contract` off, `Capabilities` is empty, the signature is
empty and the episode is skipped entirely. The tier's dependency on that arm is unchanged.

## Amendment A2 — the D8 outcome loop was never wired (2026-07-28)

**D8's text is unchanged and was never wrong. It was unreachable.**

`PlanRecord.FollowedProcedures` — the field that tells the memory agent which routines
were in the planner's context — was declared in `domain/generator.go` and READ in
`internal/memory/agent.go`, where it drives `FeedProcedureOutcome`: confidence moves under
the slow-consolidation rate, and a routine falling below `procedure_deprecate_below` is
retired. Nothing anywhere WROTE it. `len(rec.FollowedProcedures) > 0` was never true, so
in the tier's whole lifetime no routine's confidence moved and none was ever deprecated.

The consequence is worse than a missing feature. A tier that can influence a plan but
cannot learn whether it helped does not merely fail to improve — it accumulates advice
whose quality is unfalsifiable, which is exactly the erosion D8 exists to prevent. The
guard was in place and the signal never reached it.

### The three hops

| Hop | File | What it does |
|---|---|---|
| Record | `internal/awareness/planner.go` | Copies the IDs of the routines `ltmEnrichment.Procedures` supplied into `plan.FollowedProcedures` — what the planner was SHOWN, not what it can be proven to have used. Attribution here is deliberately generous; D8's slow rate is what makes a wrong attribution survivable. |
| Preserve | `domain/plan.go` (`ExecutionPlan.Clone`) | Deep-copies the slice through the plan freeze, so a replanned plan does not silently lose its provenance. |
| Deliver | `internal/metabolism/executer/dag_executor.go` | Passes it to the `PlanRecord` at `WritePlanScene`, for both success and failure. |

### How the third hop was caught

Each hop was mutation-checked separately, and that is the only reason the executor hop is
correct. `TestProcedureFeedbackLoop_ClosesEndToEnd` builds its `PlanRecord` directly, so it
**passes with the executor hop deleted** — the same class of blind spot that let the field
exist unwritten in the first place. `TestWritePlanScene_CarriesFollowedProcedures` now
asserts that hop specifically, and fails when it is removed.

**Verified end-to-end:** a failed plan naming a routine moves it from confidence 0.600 at
3 samples to 0.510 at 4.

**Still unmeasured:** whether the loop improves anything. Confidence moves one sample per
plan, so this needs volume before it says anything, and it cannot run at all with
`capability_contract` off (no capabilities ⇒ no signature ⇒ no routines to follow).

