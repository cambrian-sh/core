# ADR-0015: Engram Engine — Biologically-Grounded LTM Architecture

**Status:** Implemented
**Superseded-by (pending):** ADR-0049 (Experiential Memory / World Model — `Proposed`).
ADR-0049 supersedes the per-step `mnemonic_scene` model from this ADR but is not yet built,
so ADR-0015 remains the live implementation. Flip to `Superseded-by-ADR-0049` only when 0049 ships.
**Date:** 2026-05-17
**Deciders:** Afsin, Claude
**Prerequisites:** ADR-0012 (Synaptic Bridge), ADR-0013 (Semantic Checkpoint), ADR-0014 (Thalamic Gating)

**Implementation Dependency Graph:**
- ADR-0015 unblocks ADR-0016: Tier-1 channel (merged retrieval), `DocTypeMnemonicFact`/`Scene` constants, `activation_strength` column
- ADR-0015 unblocks ADR-0017: `document_edges` schema, `activation_strength` field, `DocType` constants

Parallelisable slices (no downstream dependency):
- `documents` table schema migration
- `PgVectorAdapter` interface changes
- `MemoryRecorder` interface definition
- `scoring_prompt_version` column addition

---

## Context

Cambrian's current LTM is a flat pgvector search: every step result is written directly to the `documents` table, all documents have equal retrieval weight, and nothing is ever forgotten. This produces three compounding problems:

1. **No lifecycle discrimination.** A memory written five minutes ago by an unverified agent is indistinguishable from one retrieved and confirmed across twenty sessions. The Planner receives both with equal weight.
2. **Hallucination reinforcement.** A hallucinated memory, once written, is retrieved again, reinforcing the agent's belief in the false state. `importance_score` is set at write time and never changes.
3. **Unconstrained LTM growth.** Every step output enters LTM regardless of its information content. Over time this degrades retrieval signal-to-noise.

The plan also uses L2 distance (`<->`) for all retrieval — correct for magnitude-weighted spaces but wrong for semantic similarity, where cosine distance is the geometrically correct metric.

**Design Philosophy Note:** The biological mechanisms referenced in this ADR (Ebbinghaus decay, hippocampal buffering, sleep-phase consolidation) are architectural constraints that define the solution space, not decorative metaphors. The constants are derived from neuroscience literature scaled to machine-time constants (e.g., `λ = 0.001` approximates a human-month forgetting curve at nightly machine cycles). This ADR assumes the biological-grounding philosophy is accepted; its purpose is to specify the implementation and its empirical validation path (SPC metrics, instrumentation), not to re-justify the philosophy.

---

## Decision

### 1. Two-Tier Write Pipeline

Step results no longer write directly to pgvector. Instead:

- **Tier 1 — Pending Channel (immediate):** When `DAGExecutor.mergeStepResult` fires, a new `MemoryRecorder` interface (injected into `DAGExecutor`, same pattern as `ThoughtFn` and `CheckpointStore`) calls `MemoryAgent.RecordExecution(stepResult, contextSnapshot)`. The step output and its `masterContext` snapshot are embedded immediately via `Embedder.Embed` and placed into a bounded in-memory channel. Items in the channel are **immediately searchable** — retrieval does a linear cosine scan over channel embeddings before querying pgvector, merging both result sets.

- **Tier 2 — LTM Commit (background):** A background goroutine drains the channel in batches. For each batch it calls the `Generator` (LLM) to score each item across Relevance, Specificity, and Explicitness dimensions. Based on the score it commits to pgvector as FULL dual-trace (FACT + SCENE), FACT-only (compressed), or drops permanently. Drain is triggered by either condition: `channel_length >= batch_size` (load-driven) OR `idle_time > max_idle_seconds` (default 300s, configurable in `ExecutionConfig`). The idle-time condition ensures low-traffic periods flush small batches promptly rather than leaving them unscored until the next load spike.

The channel is the biological hippocampal buffer. The background LLM batch is sleep-phase consolidation. Items lost on process restart are acceptable — they are ephemeral episodic events, not validated knowledge.

**Tier-2 LLM-as-Judge constraints (required implementation details):**

The Tier-2 `Generator.Generate` call for batch scoring MUST:
- Use `temperature=0` for deterministic commit-tier decisions across identical inputs
- Accept a JSON-schema-constrained response: three integer scores (Relevance, Specificity, Explicitness 1–10) plus one commit-tier enum (`FULL` / `FACT_ONLY` / `DROP`)
- Use `context.WithTimeout(ctx, cfg.Tier2LLMTimeout)` where `tier2_llm_timeout` defaults to 30s in `ExecutionConfig`

On timeout or Generator failure: apply heuristic scoring to the **entire batch** — `text_length_score × TrustScore` — and commit all items as FACT-only (never FULL or SCENE). Log `tier2_llm_timeout=true, batch_size=N` and increment `tier2_llm_fallback_count`. No per-item retry within a timed-out batch; per-item retry under timeout pressure converts a latency problem into a cascade.

A `scoring_prompt_version VARCHAR(8)` column is added to `documents` and written at Tier-2 commit time (short hash of the scoring prompt template). Recalibration pipeline (rescoring existing documents when prompt version changes) is deferred to a future ADR; the column provides the predicate when that pipeline is built.

**Observability fields** (written as structured log entries at each Tier-2 drain cycle):
- `tier2_llm_fallback_count` — cumulative count of batches that fell back to heuristic scoring
- `tier2_channel_depth` — channel length at drain time
- `tier2_drop_rate` — fraction of items dropped (score below commit threshold) in the batch

### 2. Dual-Trace Encoding via DocType Extension

The existing `Document` type gains two new `document_type` constants:

- `DocTypeMnemonicFact = "mnemonic_fact"` — the structured step output (tool response, agent result). Treated as factual evidence in retrieval.
- `DocTypeMnemonicScene = "mnemonic_scene"` — the `masterContext` snapshot at step completion time: environment variables, active plan index, directory state, DAG node context. Treated as mnemonic/contextual only.

No new `MnemonicTrace` domain struct is introduced. The existing `Document` type with its `DocumentType` discriminator is sufficient; `SearchOptions.DocumentType` already provides retrieval-time filtering. The Planner's factual retrieval path filters to `DocTypeMnemonicFact` only — SCENE documents never enter the evidence context. This prevents scene mnemonic narratives from being treated as ground truth (confabulation risk).

### 3. Silent Engram Lifecycle

`activation_strength DOUBLE PRECISION DEFAULT 0.1` is added to the `documents` table. This is the sole lifecycle metric for a memory — `importance_score` is dropped entirely. Migration:

| Old usage | New behaviour |
|---|---|
| `ImportanceScore < 0` — poison detection | `Metadata["is_poisoned"]` (already written alongside; check migrated to metadata only) |
| `ImportanceScore: 3.0` — NegativeEdge | `activation_strength: 0.1` (silent engram default) |
| `ImportanceScore: 7` — procedural template | `activation_strength: 0.5` (pre-warmed; not yet mature) |
| `ImportanceScore < 9` in `GetStaleMemories` | `activation_strength < threshold` |
| `ImportanceScore < 3 && AccessCount < 2` in `worker.go` | `activation_strength < 0.3 && access_count < 2` |

`activation_strength` grows via the `retrieve_and_update_memories` stored procedure: `+0.05` per retrieval, capped at `0.8` (maturation ceiling). At `+0.05`, an engram requires approximately fourteen cross-session retrievals to mature — enforcing genuine multi-session validation rather than within-session reward. Maturation is driven by natural retrieval frequency alone; no direct VerifierPool coupling is introduced (see Considered Options).

### 4. Database-Native Decay via pg_cron

The nightly `apply_ebbinghaus_decay` stored procedure runs via `pg_cron` at 03:00 UTC — not via `CircadianRhythm`. Cambrian may not be running at 03:00; PostgreSQL always is. The decay operation has zero dependency on Go runtime context. `CircadianRhythm` is repurposed as a health probe: on startup it checks stale engram count and warns if unexpectedly high (indicating `pg_cron` may be disabled).

Decay formula — retrieval-weighted multiplicative damping:

```
S(t) = (S₀ + η·R_count) · e^{-λt} · (1 - ε)
```

Where `λ = 0.001`, `ε = 0.02` (interference damping), `η = 0.005` (retrieval bonus coefficient). `activation_strength` is bounded to `[0.0, 1.0]` — it can never go negative regardless of interference load. High-access memories start from a higher base `(S₀ + η·R_count)` and survive longer.

Memories with `activation_strength ≤ 0.05` AND `access_count = 0` AND `created_at < NOW() - INTERVAL 'min_gc_age_days days'` are garbage-collected by the same procedure. `min_gc_age_days` defaults to 30 and is configurable in `ExecutionConfig`. This minimum age gate protects recently-written memories from premature GC while the right future query has not yet arrived — a memory written last week with low activation is not yet "forgotten", it simply has not been needed yet.

#### Decay Parameter Sensitivity — Operational Envelope

*The following bounds define the safe operating envelope for each tunable constant. Values outside this range are not prohibited but require corresponding adjustment to related parameters as noted.*

**`λ` — base decay rate (default 0.001)**
- At `λ=0.001`: nightly effective decay rate ≈ 2.1%; a fresh zero-access memory (`AS=0.1`) reaches GC threshold (`0.05`) at ~33 days.
- At `λ=0.002`: decay rate ≈ 3.1%; fresh memory reaches GC threshold at ~22 days. `min_gc_age_days` must be ≥22 or cliff-GC fires immediately when the protection window opens at day 30.
- At `λ=0.0005`: decay rate ≈ 0.6%; fresh memory reaches GC threshold at ~66 days — acceptable on low-traffic deployments; LTM growth risk above ~50 active sessions/day without GC compensation.
- **Calibration constraint:** operators who raise `λ` must verify `min_gc_age_days` < time-to-GC-threshold; otherwise the protection window is always active and unaccessed memories accumulate indefinitely.

**`ε` — interference damping (default 0.02)**
- At `ε=0.02`: 2% suppression per nightly cycle; after 35 cycles (~5 weeks), compound interference factor ≈ 0.49 — recoverable by a single retrieval bump (+0.05).
- At `ε=0.04`: 4% per cycle; after 35 cycles, compound factor ≈ 0.24 — exceeds the recovery capacity of +0.05 per retrieval for memories accessed fewer than 2× per week.
- **Upper bound: `ε > 0.04` must not be combined with `η < 0.005`** — interference will outrun retrieval recovery; the anti-hallucination decay property becomes blanket amnesia.
- At `ε=0.00`: pure Ebbinghaus behaviour, no interference term; safe but loses the confabulation-suppression mechanism.

**`η` — retrieval bonus coefficient (default 0.005)**
- At `η=0.005`: a memory with `R_count=20` adds 0.10 to its daily survival base; effective GC-threshold crossing deferred by ~7 days vs. zero-access equivalent.
- At `η=0.01`: `R_count=20` adds 0.20; high-access memories survive ~2× longer than zero-access. Appropriate if retrieval frequency is expected to be low (< 5 accesses/week typical).
- At `η=0.02`: `R_count=20` adds 0.40 — high-access memories approach semi-permanent status. Only appropriate for curated long-lived procedural knowledge.
- **Upper bound: `η × max_expected_R_count` should not exceed 0.7** (maturation ceiling minus S₀=0.1); at `η=0.005` this caps at `R_count=140`.

**GC threshold (default 0.05)**
- Raising to `0.10`: fresh memory GC at ~22 days (default `λ`) — risk of premature GC for valid memories awaiting their first relevant query.
- Lowering to `0.02`: extends decay-to-GC period to ~65 days — minor LTM growth overhead, acceptable on deployments with `min_gc_age_days ≥ 30`.
- **Do not raise above `S₀ × 0.9 = 0.09`** — at GC threshold ≥ initial AS, every newly committed memory is immediately GC-eligible on day 31.

**SPC monitoring signal:** if `activation_strength p50` across all documents trends toward the GC threshold rather than distributing between 0.1–0.8, the decay rate is too aggressive. All four parameters are in `ExecutionConfig` — operator-tunable without code changes.

### 5. Cosine Distance and Hybrid Re-ranking

All retrieval switches from L2 (`<->`) to cosine distance (`<=>`). The HNSW index is rebuilt with `vector_cosine_ops`. This is consistent with the `CapabilityClusterer` (ADR-0014), which already uses cosine similarity for all agent embedding comparisons. L2 is sensitive to vector magnitude — two semantically identical embeddings from different-length inputs appear dissimilar under L2.

Retrieval is two-phase:
1. **ANN cosine pass** — HNSW index returns `max_limit × 3` candidates via `<=>`.
2. **Activation re-rank** — candidates scored as `cosine_similarity × (α + (1-α) × activation_strength)`, top `max_limit` returned. `α = retrieval_floor` (default 0.2, configurable in `ExecutionConfig`).

The floor multiplier guarantees every memory earns at least `α × cosine_similarity` as its minimum score — preventing total suppression of highly-relevant recent memories while preserving the anti-hallucination property. A mature validated memory (AS=0.8, cosine=0.7) scores `0.7 × 0.84 = 0.588`; a new high-similarity memory (AS=0.1, cosine=0.9) scores `0.9 × 0.28 = 0.252`; a new high-similarity hallucination (AS=0.1, cosine=0.95) scores `0.95 × 0.28 = 0.266`. The validated mature memory dominates by a 2.2× margin.

**Exploration policy for future LTR training:** The floor-multiplier creates a positive feedback loop (Matthew effect): high-scoring documents are retrieved more often, increasing `activation_strength`, which further increases their score. To prevent the heuristic from entrenching its own rankings before labeled data exists, `MemoryAgent.Query` MUST reserve 5% of retrieval slots for documents sampled uniformly from the top-3K cosine hits, ignoring `activation_strength`. These exploratory retrievals are logged with `exploration_slot=true` in their metadata.

The LTR trigger condition: a future ADR will evaluate LTR replacement when the system has accumulated ≥100 retrieval sessions where ≥50% of slots were followed by a VerifierPool-scored plan completion (retrieved documents fed into a plan whose output was independently verified). Raw retrieval count or `activation_strength` volume is not a valid training signal — the trigger must be tied to labeled outcomes, not volume.

### 6. Scope Boundary

This ADR covers the **pgvector LTM layer only**: schema changes, write pipeline, retrieval, decay, and dual-trace encoding. The `masterContext map[string]string` → bounded Global Workspace Stage replacement is **ADR-0016**. The two-tier write pipeline's channel is not the GWS — it is a write-staging buffer internal to `MemoryAgent`, not a system-wide working memory broadcast.

---

## Considered Options

**FEP/EFE paging for retrieval eviction (from the Engram Engine plan).** Expected Free Energy minimisation requires computing `q(s|π)` (internal belief distribution) and `ln p(o)` (log marginal likelihood of observations) at runtime. Neither has a concrete representation in a Go application. Replaced by the `cosine_similarity × activation_strength` scoring function, which achieves the same pragmatic utility vs. epistemic value trade-off using directly observable quantities.

**IIT Φ deadlock monitoring (from the Engram Engine plan).** Computing partition-minimised Φ over an asynchronous goroutine network is NP-hard. The existing `LoopDetector` (ADR-0005) and `SemanticCheckpoint` (ADR-0013) already provide this coverage via cosine similarity thresholding on consecutive step outputs and coherence-gated `REPLAN_SIGNAL`.

**Explicit VerifierPool → `activation_strength` coupling.** Would require a `stepTaskID → documentID` mapping that does not yet exist. The VerifierPool → TrustScore → auction win rate chain already suppresses hallucinated outputs from being repeatedly selected, which naturally limits retrieval frequency and therefore `activation_strength` growth for hallucinated memories. Explicit coupling (hybrid `+0.10` bonus for verified tasks) is the intended evolution once the two-tier write pipeline is stable.

**`MnemonicTrace` as a new domain struct.** The existing `Document` type with `document_type` discriminator already supports type-discriminated storage and filtered retrieval via `SearchOptions.DocumentType`. Two new constants are non-breaking and sufficient. A parallel struct would duplicate the pgvector adapter surface without adding capability.

**`CircadianRhythm` as decay trigger.** Decay must fire at 03:00 regardless of whether Cambrian is running. The decay stored procedure has zero dependency on Go runtime state. `pg_cron` owns this schedule; `CircadianRhythm` becomes a health probe.

**Cold Storage / Deep Archive instead of hard GC.** Proposed as a way to preserve long-tail events that look like noise today but may matter in the future. Rejected: an archive table grows unbounded with no defined reader and no pruning mechanism — it defers garbage rather than managing it. The minimum age gate (`min_gc_age_days = 30`) in the GC predicate addresses the real concern (recently-written unaccessed memories) at zero operational cost.

**Lazy Decay (on-the-fly at retrieval time).** Proposed to eliminate the pg_cron single point of failure by computing decay when a memory is read using `last_decay_time + delta_t`. Rejected: lazy decay only updates memories that are retrieved. Memories with zero access count — the primary decay target — would never decay at all, making unretrieved memories immortal. This is the opposite of Ebbinghaus forgetting, which is unconditional. `CircadianRhythm`'s health probe provides sufficient visibility into pg_cron failures.

**Event-driven drain for `apply_ebbinghaus_decay`.** Proposed to replace the fixed 03:00 pg_cron schedule with a Little's Law threshold trigger. Rejected for the decay procedure: Ebbinghaus decay is time-indexed by biological definition and must fire unconditionally regardless of load. The event-driven threshold is accepted for the Tier-2 goroutine drain (load-driven OR idle-timeout), but decay stays time-based.

**Tentative Fact flag for Tier-1/Tier-2 context drift.** Proposed to mark Tier-1 documents as `tentative=true` in search results and trigger `SemanticCheckpointError` if a Planner decision is based on a tentative fact that later gets dropped by Tier-2. Rejected: the Planner is stateless — there is no mechanism to link a specific enrichment map key back to its Tier-1 origin after the plan is executing. The failure mode (Tier-1 fact influences plan AND is subsequently dropped by Tier-2 AND causes incoherence in a running plan) is a very narrow race, and `SemanticCheckpoint` (ADR-0013) already catches execution incoherence at the step level.

**Log-linear re-ranking formula** (`w₁·ln(1+cosine) + w₂·activation_strength`). Proposed to prevent suppression of new high-similarity memories by low `activation_strength`. Rejected in favour of the floor-multiplier: the additive log-linear formula makes `activation_strength` contribute independently of cosine, allowing old low-similarity memories to surface on maturity alone. The floor-multiplier `cosine × (α + (1-α) × activation_strength)` preserves the multiplicative shape and the anti-hallucination property while guaranteeing new high-similarity memories a minimum score of `α × cosine_similarity`.

**Learned-to-rank (LTR) model to replace floor-multiplier.** Proposed as a P0 prerequisite: replace the handcrafted floor-multiplier with logistic regression on `{cosine, AS, doc_age, doc_type}`. Rejected as a current requirement: an LTR model requires labeled relevance judgments linking retrieved documents to plan outcomes. Cambrian has no such labels pre-production. The floor-multiplier is the bootstrap data collector; the 5% exploration clause prevents it from entrenching its own biases. LTR replacement is a future ADR, triggered when ≥100 retrieval sessions with VerifierPool-scored plan completions exist.

---

## Consequences

- `Document.ImportanceScore` and the `importance_score` column are removed. All callers in `internal/memory/` must be migrated.
- `DAGExecutor` gains a `MemoryRecorder` field (nullable, like `EventWriter`) — no forced dependency on `internal/memory/` from `internal/metabolism/`.
- `PgVectorAdapter` must expose `UpdateActivationStrength(ctx, docID string, delta float64) error` for the stored procedure bump path.
- The HNSW index must be rebuilt with `vector_cosine_ops` — a one-time migration on first deploy.
- The bounded write channel introduces a new in-process state component in `MemoryAgent`. On process restart, uncommitted channel items are lost. This is acceptable: they are pre-consolidation episodic fragments, not validated knowledge.
- `pg_cron` must be enabled on the PostgreSQL instance. This is a deployment prerequisite, not a code change.
- `ExecutionConfig` gains six new fields: `retrieval_floor float64` (default 0.2), `min_gc_age_days int` (default 30), `tier2_max_idle_seconds int` (default 300), `tier2_llm_timeout int` (default 30, seconds for Tier-2 batch scoring LLM call), `exploration_rate float64` (default 0.05, fraction of retrieval slots reserved for uniform exploration sampling).
- `documents` table gains `scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT ''` — written at Tier-2 commit time with the short hash of the scoring prompt template.
- Tier-2 drain emits three structured log fields per cycle: `tier2_llm_fallback_count` (cumulative), `tier2_channel_depth` (depth at drain time), `tier2_drop_rate` (fraction dropped in batch).
- `MemoryAgent.Query` reserves 5% of retrieval slots for uniform exploration over top-3K cosine hits; these slots carry `exploration_slot=true` in result metadata.
