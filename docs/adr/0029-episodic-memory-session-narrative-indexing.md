# ADR-0029: Episodic Memory — Session Narrative Indexing

## Status

Accepted

## Context

Cambrian sessions are persistent conversation containers (`Active → Paused → Dormant → Completed`). There is no indexed narrative of *what was decided and learned* inside each session.

`REQ-BRAIN-2` specifies an `EpisodicMemory` document produced on session completion and retrievable in response to queries like "What did we decide about the auth flow last Tuesday?"

**Dependency on REQ-CIRCADIAN-001:** The current `CircadianRhythm` nightly daemon model is being replaced by an event-driven `MemoryLifecycleManager` (REQ-CIRCADIAN-001). This ADR is written against the target model — `SessionCompletedEvent` on the event bus is the trigger, not a `CircadianRhythm.runCycle` callback.

**Dependency on REQ-CHATBOT-001:** When Cambrian is used as a Company Brain chatbot engine, `SessionEvent` logs may contain user-typed messages that include PII (emails, phone numbers, names). REQ-CHATBOT-001 is explicit: Cambrian's LTM must contain no customer PII. Episodic extraction must apply PII masking before the LLM processes or stores any derived text.

**Deviations from REQ-BRAIN-2 (grilling session, 2026-05-31):**

| Spec assumption | Decision |
|---|---|
| Standalone `EpisodicExtractor` component | Absorbed into `ConsolidatorAgent` — same LLM call, zero extra cost |
| `Outcome string` field on `EpisodicMemory` | **Dropped** — sessions are conversations, not transactions; success/failure is a plan-level concept |
| Regex pre-filter for decision extraction | Replaced by pure LLM extraction — ConsolidatorAgent already has full context |
| Temporal signal detection gates episodic retrieval | Always retrieve, threshold-gate — consistent with SCENE/FACT/NegativeEdge pattern |
| `CircadianRhythm` callback trigger | `SessionCompletedEvent` on event bus (REQ-CIRCADIAN-001) |

---

## Decision

### 1. EpisodicMemory Domain Type

```go
// internal/domain/episodic_memory.go

type EpisodicMemory struct {
    SessionID    string       `json:"session_id"`
    StartedAt    time.Time    `json:"started_at"`
    CompletedAt  time.Time    `json:"completed_at"`
    Goal         string       `json:"goal"`
    Decisions    []Decision   `json:"decisions"`
    ActionItems  []ActionItem `json:"action_items"`
    Participants []string     `json:"participants"` // agent IDs from TaskEvent records
    KeyFacts     []string     `json:"key_facts"`    // doc IDs: referenced + created during session
}

type Decision struct {
    Text            string           `json:"text"`
    MadeAt          time.Time        `json:"made_at"`
    SourceEventType SessionEventType `json:"source_event_type"`
}

type ActionItem struct {
    Text string `json:"text"`
}
```

**`Outcome` is intentionally absent.** A session is a conversation container. Whether its constituent plans succeeded or failed is already recorded in `PlanEvent` records. Surfacing a session-level success/failure label would be misleading — a session may contain five failed plans followed by a successful one, or may simply be exploratory with no objective to succeed at.

### 2. Trigger: SessionCompletedEvent on the Event Bus

`ConsolidatorAgent` is invoked when `MemoryLifecycleManager` emits `SessionCompletedEvent` — not via a `CircadianRhythm` callback. This is consistent with REQ-CIRCADIAN-001's event-driven lifecycle model.

```
Session transitions to Dormant
    ↓
SessionDormantEvent emitted on event bus
    ↓
MemoryLifecycleManager schedules per-session async timer (TTL wait)
    ↓
TTL expires → consolidation_delay elapses (see §2 below)
    ↓
ConsolidatorAgent.Consolidate(scope={SessionID})
    ↓
ConsolidatorAgent saves EpisodicMemory to pgvector
    ↓
SessionCompletedEvent emitted on event bus
```

**`EpisodicMemory` participates in `MemoryPressureEvent`:** Each `DocTypeEpisodicMemory` document committed to pgvector is counted in `MemoryMetrics.TotalDocuments`. When the index exceeds `ConsolidationThresholdDocCount`, `MemoryPressureEvent` is emitted, which can trigger global consolidation. No special handling required — episodic documents are standard pgvector rows.

### 3. Extraction: ConsolidatorAgent Extended Output

`ConsolidatorAgent` is extended to produce `EpisodicMemory` as a second structured output alongside its existing procedural template and memory consolidation work. This avoids a redundant LLM call per session completion.

**PII Masking (REQ-CHATBOT-001):** Before any `SessionEvent` payload is passed to the LLM, it is processed by `PIIMasker.Mask`. This is a new domain interface:

```go
// internal/domain/pii_masker.go

// PIIMasker redacts personally identifiable information from text before LTM storage.
// It is applied at the episodic extraction boundary — after SessionEvent retrieval,
// before any LLM call or pgvector write.
type PIIMasker interface {
    Mask(text string) string
}
```

The production implementation (`RegexPIIMasker`) extends the existing `maskSensitiveData` patterns in `MemoryAgent` with:
- Email addresses: `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`
- Phone numbers: `(\+?[0-9]{1,3}[\s\-]?)?(\([0-9]{2,4}\)[\s\-]?)?[0-9]{3,4}[\s\-]?[0-9]{4}`
- Numeric IDs that look like customer/order references: `(customer|order|tenant|user)[_\-]?[0-9]{4,}` (case-insensitive)

`PIIMasker` is injected into `ConsolidatorAgent` at construction time and applied to each `SessionEvent.Payload` string before it enters the LLM prompt. `Decision.Text` values returned by the LLM are also passed through `PIIMasker.Mask` before the `EpisodicMemory` document is committed to pgvector.

Note: LLM extraction itself provides a first layer of abstraction (a decision like "we agreed on a contact email" may be extracted verbatim without the email address), but LLM-level abstraction is non-deterministic and cannot be relied on as a PII guard.

**Inputs passed to ConsolidatorAgent (extended):**

| Input | Source | PII-masked? | Purpose |
|---|---|---|---|
| `SessionEvent` log | BBolt `events` bucket | ✅ Yes, before LLM | Narrative: `UserMessage`, `HITLIntervention`, decisions in natural language |
| `PlanEvent` records | BBolt `plan_events` bucket | N/A (no user text) | Structured: step count, agents involved for `Participants` derivation |
| `RetrievalSession` records | BBolt `retrieval_sessions` bucket | N/A (doc IDs only) | Referenced fact doc IDs for `KeyFacts` |

**`Participants` derivation:** Collected deterministically in Go before the LLM call by scanning `PlanEvent` records for all distinct `AgentID` values. Passed as structured context — the LLM does not compute this.

**`KeyFacts` population (two sources):**
1. **Referenced facts:** `RetrievedDoc.DocID` values from all `RetrievalSession` records linked to the session's PlanIDs.
2. **Created facts:** `Document.ID` values where `metadata->>'session_id' = sessionID` AND `document_type = 'mnemonic_fact'` (pgvector query at consolidation time).

This requires a new `session_id` metadata tag written by `MemoryAgent.drainBatch` on every Tier-2 commit. See Implementation Notes.

**ConsolidatorAgent LLM output schema (extended):**

```json
{
  "consolidations": [...],
  "procedural_template": {...},
  "episodic_memory": {
    "decisions": [
      {"text": "...", "made_at": "...", "source_event_type": "user_message"}
    ],
    "action_items": [
      {"text": "..."}
    ]
  }
}
```

`Goal`, `Participants`, `KeyFacts`, `StartedAt`, `CompletedAt`, `SessionID` are filled by Go code — not by the LLM — before the `EpisodicMemory` is committed. All LLM-generated text fields (`Decision.Text`, `ActionItem.Text`) pass through `PIIMasker.Mask` after LLM return.

### 4. Storage: pgvector DocTypeEpisodicMemory

Follows the same pattern as `DocTypeProceduralTemplate` (Hippocampus):

- **`Document.Text`:** `Goal + ": " + strings.Join(decisionTexts, "; ")` — the embedding subject. Captures session intent + decisions for semantic retrieval.
- **`Document.Metadata["episodic"]`:** Full `EpisodicMemory` struct serialized as JSON.
- **`Document.DocumentType`:** New constant `DocTypeEpisodicMemory = "episodic_memory"`.
- **`Document.ActivationStrength`:** Starts at `0.1` (standard engram default, subject to temporal decay per REQ-CIRCADIAN-001).

One pgvector document per completed session. No new BBolt bucket required.

### 5. Retrieval: Always-On Third Lane in PrimeForPlanning

`WorkspaceStage.PrimeForPlanning` runs three parallel pgvector queries:

```
SCENE + FACT query   (existing)
NegativeEdge query   (existing)
EpisodicMemory query (new)  ← DocTypeEpisodicMemory, "episodic" HippocampusPolicy
```

Results are injected into `LTMEnrichment.Episodes []SearchResult`. Episodes scoring below the policy threshold are excluded.

**Temporal query limitation (MVP):** Episodic retrieval is **topic-scoped, not date-scoped**. Semantic similarity captures "auth flow" well but cannot distinguish "last Tuesday's session" from "last month's session" if both discussed the same topic. Five sessions on the same topic will score similarly; the one from "last Tuesday" has no retrieval advantage.

`Document.Metadata["episodic"]` carries `StartedAt` and `CompletedAt` — the raw data for temporal filtering exists. A future v2 path (tracked as a known gap) would accept a `date_range_hint` parameter in `SearchOptions`, expressed as a `WHERE completed_at BETWEEN $since AND $until` clause on the pgvector query. For MVP, **document the limitation rather than simulate it** with unreliable heuristics.

### 6. LTMEnrichment Extended

```go
// internal/domain/ltm_enrichment.go
type LTMEnrichment struct {
    Facts     []SearchResult // DocTypeMnemonicFact results
    Negatives []SearchResult // DocTypeNegativeEdge results
    Episodes  []SearchResult // DocTypeEpisodicMemory results (ADR-0029)
}
```

The `EpisodicMemory` struct is deserialized from `SearchResult.Document.Metadata["episodic"]` at the Planner's injection site — not inside `WorkspaceStage`.

### 7. Episodic HippocampusPolicy

A new named policy added to `DefaultConfig()`:

```go
"episodic": domain.HippocampusPolicy{
    SimilarityThreshold: 0.65,
    ConfidenceFloor:     0.0, // episodic docs have no confidence score; field unused
    MaxAgeHours:         8760, // 1 year
    TopK:                3,
}
```

`SimilarityThreshold: 0.65` is lower than `"cognitive"` (0.85) because episodic embeddings are narrative — high semantic overlap with past discussions is rarer than with factual memories. `MaxAgeHours: 8760` prevents sessions older than a year from surfacing in active project contexts.

---

## Consequences

### What Becomes Possible

- "What did we decide about X?" answered from indexed episodic narrative.
- `PrimeForPlanning` returns an `<EpisodicMemory>` block to the Planner when past sessions are semantically relevant.
- `ConsolidatorAgent` produces a richer output with no additional LLM calls.
- Temporal decay (REQ-CIRCADIAN-001) naturally ages out old episodic memories without a separate GC job.
- PII-masked decision text satisfies REQ-CHATBOT-001 LTM storage constraint.

### What Becomes Harder

1. **`MemoryAgent.drainBatch` write path change:** Every Tier-2 pgvector commit must attach `session_id` to `Document.Metadata`. Requires threading `sessionID string` through `ProcessAndStoreAsync → pendingItem → drainBatch`.
2. **ConsolidatorAgent context window growth:** Passing `PlanEvent` + `RetrievalSession` records alongside `SessionEvent` log widens the LLM prompt. For long sessions (50+ plans), this could approach model context limits. Mitigation: pass summarised `PlanEvent` records (outcome + step count only) rather than full records.
3. **`KeyFacts` lag — now guaranteed, not a race:** Under the event-driven model (REQ-CIRCADIAN-001), `SessionCompletedEvent` fires immediately after TTL expiry. The old nightly model had a ~12-hour window for Tier-2 to drain; the new model has virtually none. Facts created in the final plan of a session will be systematically absent from `KeyFacts`. This is accepted as **eventual consistency** — `KeyFacts` is populated at consolidation time from whatever facts are committed by then. A configurable `episodic_consolidation_delay_ms` (default `300_000` — 5 minutes) is added to `ExecutionConfig` so operators can widen the window when Tier-2 throughput is slow.
4. **Temporal precision gap:** Episodic retrieval is topic-scoped. Users expecting date-scoped answers ("last Tuesday's session specifically") will receive all topically-relevant sessions ranked by semantic score. This is documented in §5 and must be surfaced in user-facing documentation.

### Known Gaps (Deferred)

- **Date-range filtering:** Add `since`/`until` to `SearchOptions` for episodic queries + `WHERE completed_at BETWEEN` clause in pgvector adapter.
- **`ActionItem` extraction quality:** If the user never stated explicit follow-ups, this field will be empty. A future pass could infer action items from incomplete plan steps.
- **`RegexPIIMasker` coverage:** The initial regex set targets emails, phones, and numeric IDs. Coverage of names, addresses, and domain-specific identifiers (order numbers, ticket IDs) requires operational tuning over time.
- **`TopK=3` ceiling:** May be insufficient for users with many parallel sessions on the same topic. A future `episodic_top_k` config override per query would help.

### Rejected Alternatives

| Alternative | Why Rejected |
|---|---|
| Standalone `EpisodicExtractor` component | Doubles per-session LLM cost; `ConsolidatorAgent` already has all required inputs |
| `Outcome` field (`success/partial/failure`) | Sessions are not transactions — see §1 |
| Temporal regex gate on episodic retrieval | Hardcodes temporal language patterns in Go; semantic threshold handles topic discrimination naturally |
| BBolt as primary episodic store + pgvector index | Introduces a new BBolt bucket and two-store retrieval path; contra ADR-0025 direction |
| Embed `Goal` only as `Document.Text` | Loses `Decisions` from the embedding — the highest-signal content for episodic queries |
| Apply PII masking only to metadata (not text) | REQ-CHATBOT-001 prohibits PII in LTM; `Decision.Text` is LTM text content |

---

## Implementation Notes

### New Domain Types

- `internal/domain/episodic_memory.go` — `EpisodicMemory`, `Decision`, `ActionItem`
- `internal/domain/pii_masker.go` — `PIIMasker` interface, `RegexPIIMasker` implementation
- `internal/domain/vector_store.go` — add `DocTypeEpisodicMemory = "episodic_memory"` constant
- `internal/domain/ltm_enrichment.go` — add `Episodes []SearchResult` to `LTMEnrichment`

### Write Path Changes

1. **`MemoryAgent.drainBatch`** — attach `"session_id"` to `Document.Metadata`. Requires threading `sessionID string` through `ProcessAndStoreAsync → pendingItem → drainBatch`.
2. **`ConsolidatorAgent`** — inject `PIIMasker`; mask `SessionEvent.Payload` before LLM call; mask `Decision.Text` after LLM return; extend output schema and prompt; populate Go-owned fields (`Goal`, `Participants`, `KeyFacts`, `StartedAt`, `CompletedAt`, `SessionID`) before saving.
3. **`MemoryLifecycleManager`** (REQ-CIRCADIAN-001) — on `SessionCompletedEvent`, after `consolidation_delay_ms` elapses, invoke `ConsolidatorAgent.Consolidate(scope={SessionID})`.
4. **`WorkspaceStage.PrimeForPlanning`** — add third parallel query for `DocTypeEpisodicMemory` using `"episodic"` policy; populate `LTMEnrichment.Episodes`.
5. **`internal/awareness/planner.go`** — inject `<EpisodicMemory>` XML block from `LTMEnrichment.Episodes` when non-empty.
6. **`internal/config/config.go`** — add `"episodic"` entry to `DefaultConfig()` `HippocampusPolicies`; add `EpisodicConsolidationDelayMs int` to `ExecutionConfig` (default `300_000`).

### New pgvector Query

```go
SearchOptions{
    DocumentType: domain.DocTypeEpisodicMemory,
    TopK:         policy.TopK, // default 3
}
// Threshold-gated at WorkspaceStage using policy.SimilarityThreshold (0.65)
```

### Prompt Registration (PROMPTREQ)

The extended `ConsolidatorAgent` prompt must be registered in `domain.PromptRegistry` via `init()` in `internal/awareness/`. The episodic extraction output schema must be a distinct registered entry from the existing consolidation schema.

---

## Related

- ADR-0012 (Session lifecycle, ConsolidatorAgent)
- ADR-0015 (Engram Engine — activation_strength, Tier-2 dual-coding)
- ADR-0016 (WorkspaceStage, PrimeForPlanning)
- ADR-0025 (Memory Architecture Reform — LTMEnrichment typed return)
- ADR-0027 (HippocampusPolicy — named threshold configuration)
- CAMBRIAN-OS.md §4.2 (REQ-BRAIN-2)
- REQ-CIRCADIAN-001 (Event-driven memory lifecycle — trigger model)
- REQ-CHATBOT-001 (Company Brain chatbot engine — PII boundary)
