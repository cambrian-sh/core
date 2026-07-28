# ADR-0049: Experiential Memory — Typed Records, Online Graph, and Scenes as a World Model

**Status:** Proposed (2026-06-21) — design recorded via a grilling session; not implemented. Sequenced in §Sequencing. Supersedes the per-step `mnemonic_scene` model from ADR-0015.
**Amended 2026-06-22 (§Amendment A1):** ADR-0051 (Grounded Planner) depends on the world model as a **staleness-aware prior**. A1 adds entity **valid-time** (`last_observed_at`), a **passive drift event** on read-enrichment, names the **Scout agent** as a first-class read-enrichment population source, and conditionally accepts **LLM-extracted typed relations** (the D10 semantic-thinness gap). Audited against the graph-memory literature — see `REQ-REACTIVE-PLANNER-GROUNDING.md §8`.
**Amended 2026-07-27 (§Amendment A2):** the passive experiential write path was **unwired on 2026-07-18** for storing raw tool payloads as single embeddings. A2 records the removal, diagnoses it as a **write-granularity and write-gate** defect (not a design defect — D1–D11 stand), and specifies the re-entry: abstraction-only writes, a **prediction-error write gate**, **lane-aware non-displacing truncation**, and a **corpus/experience trust split**. A2 also **reverses this ADR's rejection of batch consolidation** (§A2.5) and defers the procedural tier to ADR-0094.
**Date:** 2026-06-21
**Author:** Afsin
**Depends on:** ADR-0015 (Engram engine — Tier-1/Tier-2 pipeline, `mnemonic_fact`/`mnemonic_scene`, `activation_strength`), ADR-0017 (spreading activation over `document_edges` — Hebbian builds on this), ADR-0022 (Global Workspace — `ContentStore`/CID, `PrimeForPlanning`/`PrimeForStep`), ADR-0025 (Tier-2 judge, negative edges), ADR-0029 (episodic `session_id`), ADR-0034 (tool `DataReadKinds`/`DataWriteKinds`; tag-based metadata filtering), ADR-0036/0041 (agent-pull ReAct loop — why `PrimeForStep` is unwired), ADR-0048 (recall lanes, summary column, `content_cid` offload + session read-gate).

---

## Context

Three observed problems, which turned out to be one:

1. **Duplicate facts.** One event ("appended a line to a file") produced ~4 near-identical memory rows: an eager `WriteScene` scene, a Tier-2 `createSceneDoc` scene, a `RecordExecution` step FACT (`step_N:`), a D6 `RecordToolOutput` FACT (`tool[…]:` raw JSON), plus a `Step N result:` masterContext entry. Four writers across two layers (DAG plan-step vs. agent tool-call), none aware of the others, no cross-path dedup. Lexical dedup can't catch them (terse JSON vs. prose share no tokens); semantic dedup means the LLM we want to avoid.

2. **The graph is unpopulated.** Rich `document_edges` (`closes`/`specifies`/`contradicts`) depended on session-level consolidation that doesn't fire reliably. Nightly/batch consolidation is explicitly rejected.

3. **Scenes are degenerate.** `mnemonic_scene` is documented as "the situation a fact happened in," but in practice it dumps the step's own output text — a near-copy of the FACT (a *cause* of problem 1), wasting its purpose. Yann LeCun's framing — *"you can't build an agentic system without the ability to predict the consequences of its actions"* — names what scenes *should* provide: a **world model**.

The root causes: (a) a **category error** — events (tool mutations) stored as knowledge (`mnemonic_fact`); (b) a **missing primitive** — no key linking the layers that record the same step; (c) a **granularity error** — scenes captured per-step (self-duplicating) instead of per-situation.

**Wiring finding (corrects a common assumption):** `PrimeForPlanning` **is** live (`planner.go:235`, `WorkspaceStage` wired at `awareness_stack.go:33`). `PrimeForStep` (the agent/step-side push) has **no non-test caller** — it is the dead path. So the world model reaches the planner by *push* (enrich the live `PrimeForPlanning`) and reaches agents by *pull* (a recall lane), never via reviving `PrimeForStep`.

This is **non-parametric / episodic** world modeling: memory stores observed `(conditions → action → outcome)` transitions; the LLM predicts by reasoning over retrieved similar ones. No learned simulator (the accumulated store is the dataset for a future one). Where determinism suffices, **no LLM is used** (a standing constraint from the grilling).

---

## Decisions

### D1 — Type memory by *what it is*, not where it came from
Four document types, each answering a distinct question:
- **`mnemonic_action`** (new) — "what did I *do*?" Minted by **mutation/side-effecting** tool calls (`DataWriteKinds` set). Deterministic structured form (`write_file → ok | path=…, +19B`), never a raw-JSON dump.
- **`mnemonic_fact`** — "what do I *know*?" Read-tool **payloads** (`DataReadKinds`), synthesized insights, `remember()`.
- **`mnemonic_scene`** — "under what *conditions*?" (redefined by D5).
- **`mnemonic_entity`** (new) — "what do we know about *this thing*?" (D8).

Routing is the deterministic `DataReadKinds`/`DataWriteKinds` switch (ADR-0034 metadata), not an LLM judgment. The old defect was mutations stored as `mnemonic_fact` (knowledge), which is the category error behind the duplication.

### D2 — Actions are durable, structured, and the transition log
An action record is the durable record of *how the world reached its current state* ("we wrote that file and it's still there"). **Durable**, not session-scoped. Its **validity** (still in force?) is tracked by supersession edges (D6) — a later `delete Y` action `closes` the `wrote Y` action; the record stays as history, but its current validity flips.

### D3 — Structural correlation kills duplicates by construction (no LLM, no lexical guess)
Thread a **step-id** (and **plan-id**) correlation key into `ExecuteTool` so D6 stamps each action with the step that issued it. Then dedup is structural — *same step*, not *similar text*:
- **Single-action step** → keep the structured Action; **drop the prose step synthesis** (decided by *action count = 1* — it is a lossy restatement).
- **Multi-action step** → actions **+** a synthesis Fact, grouped by the **step-id tag** (the hub is a tag, not a node — deliberately not the scene, to avoid pre-deciding D5).
- **One scene per *plan*, not per step** (D5). Eager `WriteScene` owns the scene; `RecordExecution`'s `createSceneDoc` no-ops when a `sceneID` exists. Remove the redundant `Step N result:` writer.

### D4 — Separate recall lanes
`memory_query` stays **facts-only** ("what do I know"). **Actions** are a distinct retrieval intent ("what did I do"). **Precedents** (D11) are a third lane. Grounding a claim and reconstructing history are different intents; mixing them re-bloats context.

### D5 — Scenes are plan-wide, immutable, written once at plan completion
A scene is the **stable setting**, not a per-step snapshot (per-step snapshots self-duplicate — a cousin of D1). Granularity: **plan-wide** primary, with a thin **session-ambient** layer for true invariants (OS, machine, repo root). Mechanism (**b-ii**): `state = scene + replayed non-superseded actions` — the scene is the **initial conditions**, actions carry every delta.
- **Scope is discovered, not guessed**: accreted from the entities the plan actually engages (first-touch). A plan that touches a directory has that directory in its scene; one that uses an API has that API.
- **Immutable as a record**: the **scene-id is pre-allocated at plan start** (from plan-id, so actions can reference it mid-run), but the **doc is materialized once at plan completion**, when scope is fully known. Never rewritten, never per-step. (Plan-end writing costs nothing for prediction — prediction queries *past* completed scenes.)

### D6 — Scene bounding: full baseline **by reference**, not by value
Capture rich baselines without bloat by storing pointers:
- **Filesystem/repo** → a **git commit hash** (reuse the CoW snapshots `agent_connector.go` already takes per task: `agentID+taskID → commit hash`). 40 bytes ↔ the entire tree, byte-exact, resolvable. Zero new capture cost.
- **Non-fs entities** (API, DB, document) → an inline **descriptor** (identity, role, shape — endpoints, auth, schema) + the full baseline **offloaded to CAS** with a `content_cid` (ADR-0048 pattern).
- **Lazy first-touch**: capture an entity's baseline only when engaged → scope bounded by what the plan does.
Consequence: a scene has **two faces** — a *reconstruction face* (descriptors + reference pointers, for state replay) and a *retrieval face* (D7), because hashes/cids are useless for similarity.

### D7 — The retrieval projection (what makes two situations "similar")
A compact, embeddable projection enables situational retrieval. Content = **goal** (the plan's intent) + **abstracted entity roles/types** (`1 write-target markdown file, 1 read-source web API` — **not** specific IDs) + **environment kind** (`git repo, Windows, Python project`). Assembled **deterministically** (goal from plan; roles/types from engaged entities; env from cwd/repo/OS) and embedded.
- **Specific entity IDs are excluded** from the similarity key — they belong to the entity index (D9). Embedding the IDs would make every scene unique and break similarity (the likely reason naive scene-embedding never retrieved anything).
- **Outcome (success/failure) is a field, not part of the similarity key** — you match on pre-conditions, *then* read what followed.

### D8 — First-class entity records (`mnemonic_entity`)
One record per real thing, materialized and kept current.
- **Identity**: `kind:canonical-id` (`dir:`/`file:`/`api:`/`service:`/`repo:`/`url:`/`db:`), derived deterministically from tool args, **aggressively canonicalized** (abs paths, Windows case/separator normalization, no trailing slash) — fragmentation is the #1 failure mode.
- **Granularity**: files **and** directories as entities; an API/service is **one** entity with endpoints as descriptor *attributes*. **Mutated-only minting** — a created/mutated resource mints a record; a pure read enriches an existing one but never mints.
- **Update**: **field-level last-write-wins**, timestamped → a **materialized current view** (`exists`, `content_ref`/commit-hash, `endpoints`, `auth`, `size`…), each field updated by the latest engagement that observed it.
- **Supersession is action-driven**: `delete`→`exists=false`, `overwrite`→new ref; the record reflects what actions did, never guesses.
- **History is derived from provenance** (links to engaging scenes/actions), not stored inline — "what was the endpoint *before*" walks the chain (cold path).
- **Rebuildable cache**: scenes + actions remain the source of truth; the entity record is a deterministic projection that can be **replayed to reconstruct** if lost or suspect. (This is what makes B safe and is *why* the merge must be LLM-free — replay must be reproducible.)

### D9 — Three access paths over experiential memory
- **Situational** (fuzzy) → embedding over the D7 projection ("scenes like my situation").
- **Entity** (exact) → canonical-ID / metadata-tag lookup ("that directory's last scene", "that API's endpoints").
- **Reconstruction** → scene baseline + replayed non-superseded actions ("what's true now").

### D10 — Graph population without nightly jobs
Edges as a byproduct of write and read:
- **Structural, deterministic, at write**: `follows` (step→predecessor), `specifies`/`closes` (retry→failure; `delete`→`wrote`), `discussed_in` (FACT→SCENE, exists).
- **Hebbian co-activation, deterministic, at read**: when recall/spreading **co-retrieves and strongly co-activates** two memories, reinforce (or create at low weight) the edge between them — small learning rate + Ebbinghaus decay (reuse the `activation_strength` decay) + normalization against the Matthew effect. The graph self-organizes from *usage*, capturing practical relatedness embeddings miss. Edge-weight writes are async/batched off the read path.
- **LLM-judged contradiction edges**: **deferred** (opt-in later) — only where determinism genuinely can't reach.

### D11 — World-model use: both planner and agent, via their *live* paths
- **Planner (push)**: extend the live `PrimeForPlanning` `LTMEnrichment` with a **precedent lane** — similar past scenes + their outcomes/actions. Highest leverage (avoids committing to a doomed *approach*, not just a bad call).
- **Agent (pull)**: a **precedent recall-lane** alongside facts/actions — the agent retrieves "situations like this → what followed" the same way it pulls memory. **Not** via the dead `PrimeForStep` push. (Per-action agent prediction is phase 2; situational lane is phase 1.)
- **Retrieve transitions** (scene + outcome + action path + success/failure), **failure-weighted** (negative precedents under similar conditions rank first), **similarity-gated** (no analogy below the floor → "no precedent", never fabricated). The **LLM reasons over the precedents** — memory is the model, the LLM is the inference engine.

---

## Consequences

- The original 4 duplicate rows collapse to: **one plan-scene** + **one action record** (clean structured) per action; the prose restatement and `Step N result:` are gone; the raw-JSON `tool[…]:` fact is gone. **Zero duplicate facts.**
- Memory becomes **experiential**: typed records (action/fact/scene/entity), an environment model queryable by entity, transitions queryable by situation, and a graph that grows from use — i.e., a queryable world model.
- Everything deterministic-where-possible: typing, action formatting, correlation/dedup, scene scoping, reference capture, entity merge, structural + Hebbian edges. The LLM is reserved for genuine semantic judgments (contradiction edges, deferred) and for *consuming* precedents at prediction time.

**Residual / deferred:** LLM contradiction edges; agent per-action prediction (phase 2); session-ambient scene layer detail; entity semantic search (embed entity descriptors) — currently entities are looked up by exact ID, not similarity; supersession edge semantics for partial overwrites; `PrimeForStep` remains dead (intentionally — agents use the pull lane).

## Sequencing

1. **Typing + dedup** (D1/D2/D3/D4): `mnemonic_action` type, deterministic action formatting, step/plan-id correlation, drop the prose restatement, one-scene-per-plan + kill double-scene + `Step N result:`, separate recall lanes. *Highest immediate value; resolves the reported duplication.*
2. **Graph backbone** (D10): structural edges at write, then Hebbian co-activation at read.
3. **Scenes as world model** (D5/D6/D7): plan-wide immutable scenes, reference-based baselines, retrieval projection.
4. **Entities** (D8/D9): `mnemonic_entity`, canonical IDs, field-LWW materialized cache, three access paths.
5. **Prediction use** (D11): precedent lane in `PrimeForPlanning` (planner) + precedent recall-lane (agent).

---

## Amendment A1 — Staleness-aware prior, drift events, Scout population (2026-06-22)

**Status: A1.1 + A1.2 IMPLEMENTED (2026-06-22); A1.3 automatic; A1.4 deferred.** `go build ./...` / `go vet` clean; `internal/memory` + `internal/domain` tests green (`TestDetectFieldDrift`, `TestMaterializedObservedAt`, `TestRecordToolOutput_ReadDriftEmitsWorldDelta`, `TestUpsertEntity_StampsLastObservedAt`). This is the ADR-0051 §Sequencing item-0 dependency slice, landed ahead of Scout.

**Why.** ADR-0051 turns the world model into the *prior* a pre-plan Scout consults to decide "trust the cache vs. re-observe." A graph-memory-literature audit (the survey at `REQ §8`) confirmed the structure is sound (hybrid semantic-entity + episodic-scene + associative-edge; deterministic extraction + aggressive canonicalization are right for a static schema) but flagged the entity cache as uni-temporal LWW. **Implementation finding (corrects the audit):** the substrate was already partly present — `fieldValue.At` (`entity_state.go`) timestamps every field's observation, and `upsertEntity` already wrote an entity-level `last_seen`. So valid-time *storage* existed; what was missing was a **named, exposed staleness contract** and **drift detection**. A1.1 formalizes the former; A1.2 adds the latter.

### A1.1 — Entity valid-time (`last_observed_at`) — IMPLEMENTED
`upsertEntity` now stamps an entity-level **`last_observed_at`** (`materializedObservedAt` = the most recent field-observation time; `entity_state.go`) into the entity doc metadata — the named, queryable staleness contract ADR-0051 D3 reads to decide, per referenced entity, *trust the prior vs. live re-observe*. (Minimal bi-temporality — *not* full Graphiti bitemporal versioning; transaction-time stays the D8 ordinal; per-field `At` was already stored.) Tolerance is **kind-aware** and operator-configurable (ADR-0051 D3): `last_observed_at` is "when *we* last looked," not "when it last *changed*", so externally-mutable kinds (`api:`/`url:`/shared) get ~zero cache trust while `file:`/`dir:` we wrote get a window. *(That kind-aware tolerance map is the Scout's, ADR-0051 — not in this slice.)*

### A1.2 — Passive drift event on read-enrichment — IMPLEMENTED
When a **read** (no mutating verb) enriches an entity and a pre-existing field's value **differs** from cache, `upsertEntity` emits a **passive `domain.WorldDeltaEvent`** (`EventTypeWorldDelta`) after the durable update (write-then-emit) — **no** propagation, **no** in-loop scan widening, **no** dir→child cascade invalidation (those break ADR-0051's bounded-scan cap and invite cache-invalidation rabbit holes). A **write's** change is intentional supersession, *not* drift, so emission is **read-gated** (`actionVerb=="" `). Drift detection is the pure, deterministic `detectFieldDrift` (first-touch = discovery not drift; a losing/unchanged observation = no event). Wired via `Agent.EventBus` (`main.go`; nil-safe — detection still runs, emission skipped). The event is durable raw material for **adaptive per-entity trust** (a frequently-drifting entity earning a shorter staleness tolerance), **deferred to the selection/learning layer (ADR-0037)** — *not* a world-model property; it currently has **no consumer** (the Scout/adaptive-trust consumers are future ADR-0051/0037 work).

### A1.3 — Scout is a first-class read-enrichment population source
ADR-0051's Scout observes the world *before* planning. Its reads populate the world model via this ADR's **existing D8 read-enrichment path** ("a pure read enriches an existing entity") — so Scout's discovery *automatically* refreshes entities + stamps `last_observed_at` for free (the "scan less next time" / L3 loop). D8 was written for execution-time engagement; A1.3 names a dedicated **pre-plan reader** as an equally valid population source (no new mechanism).

### A1.4 — LLM-extracted typed relations (the D10 gap), conditionally accepted
D10's graph is structurally rich (`follows`/`closes`/Hebbian) but **semantically thin** — no typed relations *between* domain entities ("this config configures that service"), so the model can't do multi-hop relational reasoning over the workspace, only co-activation proximity. **Accepted in principle: add LLM-extracted typed entity relations — *conditional on optimizing cost/latency*** (the standing determinism-where-possible constraint means the LLM extraction must be bounded/batched/cached, not on the hot path). Until that optimization is designed, this stays **deferred** (alongside the already-deferred LLM contradiction edges).

### A1 sequencing note
A1.1 (valid-time) + A1.2 (drift event) are the **dependency slice** ADR-0051 §Sequencing item 0 — they unblock Scout's staleness-targeting (0051 D3) and write-back (0051 D9). A1.3 is automatic given D8. A1.4 is deferred behind a cost/latency design.

---

## Amendment A2 — The removal, and the terms of re-entry (2026-07-27)

**Status: Proposed.** Nothing in A2 is built. A2 does not revise D1–D11; it records why the write path was removed, and constrains how it comes back. Research backing: `docs/research/experiential-memory/SUMMARY.md` (2026-07-27).

### A2.0 — What happened
On **2026-07-18** all agent-EXECUTION write-back was unwired: `RecordExecution`, `WritePlanScene`, `Hippocampus.Store`, `IngestNegativeEdge`, the `EpisodicExtractor` + `MemoryLifecycleManager` (both **removed outright**), and ADR-0034 scope promotion. Component implementations are retained. Document **ingestion is unchanged** and agents still **read** memory. The gate is `execution.experiential_memory_enabled`, default false. Recorded at `CONTEXT.md` §Known Gaps and in the flag's own doc comment:

> "the design stores whole, unfocused tool outputs (a directory listing, a file's contents) as single embeddings, which both overflows the embedder's context window and pollutes recall with low-signal auto-captured junk. Knowledge that is genuinely a memory source belongs in the explicit chunked ingest pipeline, not this auto-capture."

**The removal was correct and is not reversed by A2.** What is reversed is the assumption that the only alternative to raw auto-capture is nothing.

### A2.1 — Diagnosis: four defects, and which decisions they touch

| # | Defect | Verdict on the original decision |
|---|---|---|
| 1 | **Wrong write granularity.** Raw tool payloads embedded whole. | D1/D2 are correct in *typing* but were silent on *payload shape*. A2.2 closes this. |
| 2 | **No write gate.** Every step result and tool output written; one event produced ~4 near-identical rows. | D3 fixed *structural* duplication, but nothing decided whether an event was worth storing at all. A2.3 closes this. |
| 3 | **Undifferentiated recall.** Experience and corpus competed in one result set on one score. | **D4 already specified the fix and it was never enforced at the ranking layer.** A2.4 closes this. |
| 4 | **No displacement protection.** Nothing stopped low-signal experience evicting corpus gold. | Not covered by any original decision. A2.4 closes this. |

The design was not the problem. **D7 remains the most valuable decision in this ADR** — abstracting the retrieval projection to goal + entity *roles* + environment kind, and deliberately excluding specific entity IDs from the similarity key, is exactly the representation the 2026 literature converged on (arXiv:2604.27003, arXiv:2606.04703: abstract/principle-level experience transfers where instance-level does not).

### A2.2 — Writes are abstractions; no raw payload is ever embedded
Amends D1/D2. Three write products, and **nothing else** enters the experiential lane:

| Product | Content | Cardinality |
|---|---|---|
| **Outcome record** | The D7 projection + action path (D2) + outcome. Deterministic; structured. | One per plan |
| **Failure precedent** | Conditions → attempt → failure mode. Feeds the existing failure-weighted `precedent.go`. | One per gated failure |
| **Procedural abstraction** | A reusable capability-tagged routine. | One per recurring pattern — **specified in ADR-0094, not here** |

Hard constraint: **a tool payload is never the text of an experiential document.** Payloads may be referenced (D6's by-reference baselines, `content_cid`) but never embedded. This is the rule whose absence caused the removal, and it is the one rule in A2 that is not measurement-gated — it is a correctness constraint.

**Where these writes land, and how they are classified: ADR-0095.** Each of the three products is a chunk parented to an `experiences` row — the entity that did not exist, and whose absence made the lane ungovernable (an experiential memory had no authoritative row to carry `classification_tags`, so ADR-0091's closed-tag enforcement had nothing to bind to). ADR-0095 D4 additionally requires that **an experience is born tagged and cannot be born untagged**: the kernel stamps the originating surface (ADR-0090 `Session.Surface`) plus a default `internal` classification at creation, unforgeable and narrowable-only per ADR-0035. The parent row must therefore exist before Phase 1 writes anything.

### A2.3 — The write gate is prediction error, CONSTRUCTED from merit and outcome
Amends D1 by adding a prior question: *should this be written at all?*

**Correction (2026-07-27).** A2.3 originally said "the verifier already produces a prediction error per execution — reuse it." **It does not.** `PredictionError` exists only as a phrase in a doc comment (`internal/centralexec/belief/store.go`: `Success = 1 − Verifier prediction-error`). What the verifier produces is a **quality score in [0,1]** — an *outcome*, not an error. There was no prediction to subtract it from.

That distinction is load-bearing, not clerical. `1 − quality` is a **failure gate**, not a **surprise gate**, and surprise is the principle (arXiv:2508.03341). A plan predicted to fail that fails teaches nothing; a plan predicted to succeed that fails is the informative one — and so is a surprising *success*, which a failure gate never records at all.

**Decision: prediction error is constructed as `|prediction − outcome|`, where:**
- **prediction** = merit expected-success for the (agent, capability) pair — `AgentProfile.SuccessRate`, or `CapabilityStats` when ROUTE-06 is armed.
- **outcome** = the plan's own success/failure, refined by verifier quality when a verifier ran.

Why merit rather than the agent's bid confidence: merit is **already live** (`ProfileAggregator` runs unconditionally in the supervision stack), whereas calibrated bids are a default-off arm (ROUTE-05), so a gate keyed on them would inherit that arm's off-by-default status and its calibration debt. Merit is also the kernel's own belief rather than the agent's self-report, which keeps the gate unforgeable in the same sense ADR-0035 means it.

Why the plan outcome rather than verifier quality alone: **the verifier samples ~10%.** `shouldSample` (`internal/supervision/verify/verification_worker.go`) always verifies a low-trust agent's task but otherwise takes `hash(taskID) % 10 == 0`. Keying the gate on verifier quality alone would blind it to roughly nine tenths of a trusted agent's plans. Plan success/failure is known for 100% of plans; verifier quality sharpens the outcome estimate on the sampled subset rather than being the only source of it.

Write when the outcome *surprised* the system; a plan that went as predicted leaves only its structured outcome record.

Why this gate and not another:
- **Deterministic and free.** Both terms are already computed — merit by the ProfileAggregator, outcome by the DAG executor — so the gate is arithmetic on existing state. No extra LLM call, honouring this ADR's determinism-where-possible constraint and the kernel's "no LLM at chunk granularity" rule.
- **Unforgeable.** Kernel-computed from an independent verifier, so an agent cannot inflate its own memorability. Consistent with ADR-0035.
- **Self-throttling.** Volume falls as the system's model of a domain improves — the correct behaviour, and a direct answer to the volume problem behind the removal.
- **Externally corroborated.** arXiv:2508.03341 (NEMORI) reaches the same conclusion independently — future utility is a matter of predictability — after rejecting heuristic write-gates.

The error floor is a benchmark-gated tunable. Below the floor, **nothing** is written; this is not a "log everything quietly" switch.

### A2.4 — Lane separation is enforced at the ranking layer, not the storage layer
Amends D4 by giving it teeth, and settles the "one store or two" question.

**Storage stays unified.** ADR-0093 D2 (2026-07-27) keeps every recall type in `chunks` — *"splitting recall further would have been the easy mistake: tidier DDL, and a quiet reduction in what memory can retrieve."* A2 does not disturb this. One store, one embedding space; cross-lane association (an experience recalling a corpus fact) is a feature.

**Four things separate instead:**
1. **Write gate** — explicit chunked ingestion vs. A2.3. Never the same code path.
2. **Retrieval intent** — D4's lanes (facts / actions / precedents, + procedures per ADR-0094) enforced as distinct query intents rather than one blended pool.
3. **Displacement** — the two-pass non-displacing truncation in `internal/memory/query.go` gains a **lane dimension**: experience may never evict a corpus primary from a knowledge query, and corpus chunks may never evict a precedent from a precedent query. This reuses the exact mechanism that exists because kgExpand once halved MuSiQue support-recall (0.285→0.158); the experiential lane never received that protection.
4. **Trust class** — corpus knowledge is **testimony** (externally asserted, stable, ingest-time-bounded poisoning surface); experience is **observation** (self-generated, validity flips via supersession, *continuous and self-reinforcing* poisoning surface). They already decay differently (fact λ=0.005/hr vs scene λ=0.02/hr); they must also carry distinct provenance and must not be interchangeable as grounding. arXiv:2512.16962 (MemoryGraft) attacks precisely the unearned authority of stored "successful experience."

### A2.5 — Batch consolidation is permitted (reverses a recorded rejection)
This ADR's §Context states *"Nightly/batch consolidation is explicitly rejected."* **That rejection is withdrawn, by owner decision, 2026-07-27.**

Rationale: the original rejection targeted *graph population* — making rich `document_edges` depend on a pass that did not fire reliably. D10 solved that correctly by deriving edges as a byproduct of write and read, and **that solution stands**. Recurrence-based procedural induction (ADR-0094) is a different problem: a pattern cannot be recognised as recurring from inside a single plan, so it is *inherently* cross-plan and therefore inherently offline.

Constraints, so the original concern cannot return:
- **Nothing load-bearing may depend on it.** The online path must be fully correct with the batch pass never running. A missed pass degrades enrichment; it never breaks recall, planning, or the graph.
- **It may only produce abstractions**, never repair or backfill primary records.
- **Idempotent and resumable**, over already-durable inputs.
- Externally corroborated: arXiv:2606.09483 (DCPM System 2), arXiv:2504.13171 (sleep-time compute).

### A2.6 — Agents do not own durable memory
Settles a question this ADR left implicit.

| Level | Owner | Persistence | Decision |
|---|---|---|---|
| Working memory (SDK `working_memory.py`) | The agent, in-process | Per-invocation | **Private. Unchanged.** |
| Experience it produced | The kernel | Durable | **Shared substrate, provenance-tagged.** Not private. |
| Competence about it (`AgentProfile`, `CapabilityStats`) | The kernel | Durable | **Unchanged** — held *about* the agent, not *by* it. |

No agent-owned durable store. In Cambrian's terms: it would make merit non-portable and create incumbency lock-in unrelated to declared capability (a corrosion of the Auction model); it would be an unaudited write channel outside kernel-derived write classification; agents are already stateless per call (ADR-0084 D4); and retaining experience only in its producer forces every newly spawned agent to rediscover what the fleet already knows (arXiv:2606.19911).

**Open knob (not decided here):** whether an agent's *own* prior experience is preferred at retrieval by a **soft provenance term in the ADR-0054 blend** or a **hard per-agent filter**. Both satisfy A2.6. Measurement-gated; see ADR-0094 §Open Questions.

### A2.7 — Sequencing and gate
**Phase 0 is a benchmark, not code.** There is still no memory benchmark in CI. Re-enabling any write path without a corpus-recall A/B repeats the exact failure mode that produced the removal. Defect 4 is *invisible* without it.

**Status of this sequencing (2026-07-28): Phases 0-4 implemented, all behind default-off
arms; Phase 4's belief half deferred to ADR-0037's own gate. Nothing here changes
behaviour until an operator opts in, and nothing can be MEASURED until plans accumulate
under the `capability_contract` arm — the writers, the guard and the instrument are in
place waiting for volume, which is the honest state to leave this in.**

1. **Phase 0 — measure.** ✅ DONE — Stand up a continual/test-time-learning suite (arXiv:2511.20857 Evo-Memory shape; MemoryAgentBench as alternative) in `cambrian-benchmarks`; run existing recall suites with the experiential lane on and off. Gate decision → DECISIONS.md with run-manifest IDs for both arms.
2. ✅ DONE — **Phase 1 — ADR-0095 `experiences` parent table FIRST, then A2.2 outcome records + A2.3 gated failure precedents.** The parent row is what the writes attach to and what classification binds to; writing experiential records before it exists recreates the ungovernable, undeletable state the lane is in today. Raw auto-capture stays off permanently.
3. ✅ DONE — **Phase 2 — A2.4 lane-aware truncation FIRST, then rewire `precedent.go`** through the live `PrimeForPlanning` push and the pull lane. Order is load-bearing: rewiring before the lane guard reintroduces defect 4.
4. ✅ DONE — **Phase 3 — the procedural tier: ADR-0094.** Induction, persistence, lifecycle, the D5 lane, provenance links and the batch scheduler; naturalisation fails open to deterministic intents.
5. 🔶 PARTIAL — **Phase 4 — close the loop.** The prediction-error write gate is done
   (A2.3). The PROCEDURE half of the co-evolution is done: routines shape plans via the
   D5 lane, and plan outcomes update routines via `FeedProcedureOutcome`.

   The `belief.Store` half is **NOT** done and was deliberately not attempted.
   `internal/centralexec/belief` is wired nowhere and is "unwired by design" pending
   ADR-0037's EFE-vs-auction A/B spike (`CONTEXT.md` Known Gaps). Wiring it as a side
   effect of a memory phase would smuggle a gated routing decision through an unrelated
   ADR. It belongs to ADR-0037's gate, not this one.

### A2.8 — Residual, and what A2 does NOT decide
- `execution.experiential_memory_enabled` (the raw path) stays **false permanently**. Renaming it to name the deprecated raw-capture path rather than "experiential memory" as a whole is proposed, not decided (config-schema change).
- Own-experience ranking: soft blend term vs. hard filter (A2.6).
- Prediction-error floor value — benchmark-derived, not editorial.
- This ADR's top `**Status:**` token still reads `Proposed` while `CONTEXT.md` records issues 001–014 Implemented. That drift is pre-existing and known (`docs/adr/README.md`); A2 does not silently flip it.
