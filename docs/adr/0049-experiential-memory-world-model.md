# ADR-0049: Experiential Memory — Typed Records, Online Graph, and Scenes as a World Model

**Status:** Proposed (2026-06-21) — design recorded via a grilling session; not implemented. Sequenced in §Sequencing. Supersedes the per-step `mnemonic_scene` model from ADR-0015.
**Amended 2026-06-22 (§Amendment A1):** ADR-0051 (Grounded Planner) depends on the world model as a **staleness-aware prior**. A1 adds entity **valid-time** (`last_observed_at`), a **passive drift event** on read-enrichment, names the **Scout agent** as a first-class read-enrichment population source, and conditionally accepts **LLM-extracted typed relations** (the D10 semantic-thinness gap). Audited against the graph-memory literature — see `REQ-REACTIVE-PLANNER-GROUNDING.md §8`.
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
