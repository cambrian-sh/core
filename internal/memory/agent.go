package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// scoringPromptTemplate is the Tier-2 LLM-as-Judge prompt (ADR-0015).
// The hash of this template is written as scoring_prompt_version at commit time.
// After PROMPTREQ migration the canonical prompt is built via domain.PromptBuild;
// this constant holds the static text for hashing purposes only.
const scoringPromptTemplate = `You are a memory quality judge. Score each step result below across three dimensions (1-10 each):
- relevance: how relevant is this result to the task?
- specificity: how specific and actionable is the information?
- explicitness: how clear and explicit is the information?

Return ONLY a JSON array. Each element: {"relevance":N,"specificity":N,"explicitness":N,"tier":"FULL"|"FACT_ONLY"|"DROP"}
Tier rules: average >= 7 → FULL (fact + scene), average >= 4 → FACT_ONLY (fact only), average < 4 → DROP.
Use deterministic mode.`

// scoringPromptHash is the 8-char SHA-256 of the tier-2 scoring prompt (ADR-0015).
// Written to Document.ScoringPromptVersion at Tier-2 commit time.
var scoringPromptHash = domain.PromptHashOf(scoringPromptTemplate)

// ── Importance scoring prompt ─────────────────────────────────────────────────

const importanceRole = "You are a memory importance judge."
const importanceRules = `- 10: Critical facts — decisions, API keys, explicit user preferences.
- 7–9: Important operational data — configurations, agent outputs, workflow steps.
- 4–6: Moderately useful context.
- 1–3: Trivial, redundant, or low-signal logs.`

var importancePromptHash = domain.PromptHashOf(importanceRole + importanceRules)

func init() {
	domain.PromptRegistry[importancePromptHash] = domain.PromptEntry{
		ID:      "memory.importance",
		Version: "1.0.0",
		Hash:    importancePromptHash,
	}
	domain.PromptRegistry[scoringPromptHash] = domain.PromptEntry{
		ID:      "memory.tier2_score",
		Version: "1.0.0",
		Hash:    scoringPromptHash,
		Schema:  scoringSchema,
	}
}

// scoringSchema is the JSON Schema for the Tier-2 scorer output array.
const scoringSchema = `{
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "relevance":    { "type": "integer", "minimum": 1, "maximum": 10 },
      "specificity":  { "type": "integer", "minimum": 1, "maximum": 10 },
      "explicitness": { "type": "integer", "minimum": 1, "maximum": 10 },
      "tier": { "type": "string", "enum": ["FULL", "FACT_ONLY", "DROP"] },
      "descriptor": { "type": "string" }
    },
    "required": ["relevance", "specificity", "explicitness", "tier", "descriptor"]
  }
}`

// scoredItem holds a scored pending item with its tier and raw scores.
type scoredItem struct {
	Item         pendingItem
	Relevance    int
	Specificity  int
	Explicitness int
	Tier         string // "FULL", "FACT_ONLY", "DROP"
	// Descriptor is a one-sentence summary of the item, produced by the same
	// Tier-2 judge pass that scores it (ADR-0048 #2 — no extra LLM call). For a
	// tool-output promotion it becomes the indexed + recall-rendered Text, with the
	// raw payload preserved in metadata; so recall introduces content by MEANING
	// instead of a raw JSON head, and the embedding ranks on meaning, not syntax.
	Descriptor string
}

// Agent orchestrates memory curation by evaluating, deduplicating, and masking sensitive data.
type Agent struct {
	Manager              *MemoryManager
	LLMClient            domain.Generator
	RelevanceThreshold   float64
	MaxResults           int
	MaxNeighborExpansion int

	// Tier-1 bounded channel (ADR-0015): pre-embedded step results before Tier-2 pgvector commit.
	pendingItems []pendingItem
	pendingMu    sync.RWMutex
	pendingCap   int

	// Tier-2 drain configuration (ADR-0015).
	tier2MaxIdle       time.Duration
	tier2BatchSize     int
	tier2LLMTimeout    time.Duration
	tier2FallbackCount atomic.Int64
	tier2Stop          chan struct{}

	// GraphStore for synchronous edge writing (ADR-0017). May be nil.
	GraphStore domain.GraphStore
	// EdgeWriter for discussed_in edges written at Tier-2 commit time (ADR-0025). May be nil.
	EdgeWriter domain.EdgeWriter
	// ContentStore offloads a promoted fact's full body to CAS at Tier-2 commit so
	// recall can serve {summary + content_cid} (ADR-0048 #1). May be nil → offload
	// skipped and the full text simply stays in pgvector Text.
	ContentStore domain.ContentStore

	// EventBus emits passive world_delta drift signals when a READ observation finds an
	// entity field changed from its cached value (ADR-0049 §A1.2 / ADR-0051 D3). May be
	// nil → drift detection still runs but no event is published (the entity is updated
	// regardless). Never blocks the write path.
	EventBus domain.EventBus

	// actionCounts tracks how many action records each step (by TaskID) produced, so
	// RecordExecution can drop a single-action step's redundant prose synthesis
	// (ADR-0049 D3). Incremented as actions are recorded; read+cleared at step-end.
	actionCounts  map[string]int
	actionCountMu sync.Mutex

	// stepDocs maps a step's TaskID to its recorded fact docID (ADR-0049 D10), so a
	// later dependent step can write a `follows` edge to its predecessors' records.
	stepDocs   map[string]string
	stepDocsMu sync.Mutex

	// follows-edge deferral (ADR-0049 D10): step records are persisted ASYNCHRONOUSLY by
	// the Tier-2 drain (and may be dropped), so a `follows` edge written eagerly would
	// reference a row that does not exist yet → FK violation. Edges are BUFFERED here and
	// flushed only once BOTH endpoints have committed (mirrors writeDiscussedInEdge).
	followsMu      sync.Mutex
	committedDocs  map[string]bool       // step docIDs the Tier-2 drain has persisted
	pendingFollows []domain.DocumentEdge // edges awaiting both endpoints' commit

	// docSeq makes generated doc IDs unique even under a coarse clock (Windows
	// UnixNano resolution can repeat for rapid sequential calls, which would collide
	// IDs and break follows-edge linkage). ADR-0049.
	docSeq atomic.Uint64

	// planEntities accretes engaged-entity refs → first-touch baseline CID per plan
	// (ADR-0049 D5/D6), fed from action args+output during the plan; WritePlanScene
	// drains it into the plan's single immutable scene. The baseline is BY REFERENCE
	// (a CAS cid), never inline — so the scene stays a map, not the territory.
	planEntities   map[string]map[string]string
	planEntitiesMu sync.Mutex

	// ADR-0022 Phase 2B: notified after every Tier-2 commitBatch so
	// WorkspaceStageImpl can clear its query → []ContextRef LRU cache.
	invalidatorsMu sync.Mutex
	invalidators   []domain.CacheInvalidator
}

// pendingItem is a pre-embedded step result waiting for Tier-2 drain.
type pendingItem struct {
	Embedding []float32
	Doc       *domain.Document
	SceneID   string // ADR-0025: scene doc ID for this step; drives discussed_in edge
	SessionID string // ADR-0029: session scope; written to Doc.Metadata["session_id"] at commit time
}

// NewAgent creates a memory Agent.
func NewAgent(manager *MemoryManager, llmClient domain.Generator, relevanceThreshold float64, maxResults, maxNeighborExpansion, channelCap, batchSize, maxIdleSec, llmTimeoutSec int) *Agent {
	if channelCap <= 0 {
		channelCap = 256
	}
	if batchSize <= 0 {
		batchSize = 32
	}
	if maxIdleSec <= 0 {
		maxIdleSec = 300
	}
	if llmTimeoutSec <= 0 {
		llmTimeoutSec = 30
	}
	return &Agent{
		Manager:              manager,
		LLMClient:            llmClient,
		RelevanceThreshold:   relevanceThreshold,
		MaxResults:           maxResults,
		MaxNeighborExpansion: maxNeighborExpansion,
		pendingCap:           channelCap,
		tier2BatchSize:       batchSize,
		tier2MaxIdle:         time.Duration(maxIdleSec) * time.Second,
		tier2LLMTimeout:      time.Duration(llmTimeoutSec) * time.Second,
		actionCounts:         map[string]int{},
		stepDocs:             map[string]string{},
		committedDocs:        map[string]bool{},
		planEntities:         map[string]map[string]string{},
	}
}

// IngestSync evaluates and stores info synchronously.
func (a *Agent) IngestSync(ctx context.Context, text string, sourceAgent string) error {
	cleanText := a.maskSensitiveData(text)

	score, err := a.evaluateImportance(ctx, cleanText)
	if err != nil {
		slog.Error("memory evaluation failed", "err", err)
		return err
	}
	if score < 5 {
		slog.Debug("memory score below threshold, discarding", "score", score, "text", cleanText)
		return nil
	}

	results, err := a.Manager.Query(ctx, cleanText, 1)
	if err == nil && len(results) > 0 && results[0].Score > 0.95 {
		slog.Debug("duplicate memory found, discarding", "score", results[0].Score)
		return nil
	}

	docID := fmt.Sprintf("mem-%d", time.Now().UnixNano())
	metadata := map[string]interface{}{
		"timestamp":    time.Now().Format(time.RFC3339),
		"source_agent": sourceAgent,
		"priority":     score,
		"tags":         []string{"autogenerated", sourceAgent},
	}

	if score < 0 {
		metadata["is_poisoned"] = true
	}

	doc := &domain.Document{
		ID:                 docID,
		DocumentType:       domain.DocTypeMnemonicFact,
		Text:               cleanText,
		ActivationStrength: 0.1,
		Metadata:           metadata,
	}

	return a.Manager.Ingest(ctx, doc)
}

// ProcessAndStoreAsync evaluates and stores info asynchronously in a goroutine.
func (a *Agent) ProcessAndStoreAsync(ctx context.Context, text string, sourceAgent string) {
	go func() {
		_ = a.IngestSync(context.WithoutCancel(ctx), text, sourceAgent)
	}()
}

// FetchContext returns relevant memories using GraphRAG neighbor expansion and Poisoned Memory filtering.
func (a *Agent) FetchContext(ctx context.Context, userInput string) string {
	results, err := a.Manager.Query(ctx, userInput, 10)
	if err != nil || len(results) == 0 {
		return ""
	}

	var sb strings.Builder
	var poisonedSb strings.Builder
	seenIDs := make(map[string]struct{})
	memoryCount := 0
	hasPoisoned := false

	sb.WriteString("\n\n--- RELEVANT MEMORY CONTEXT ---\n")
	poisonedSb.WriteString("\n--- CRITICAL FAILURES TO AVOID (ANTI-ANTIBODIES) ---\n")

	for _, res := range results {
		if _, ok := seenIDs[res.Document.ID]; ok {
			continue
		}

		if res.Document.DocumentType == domain.DocTypeNeuralTrace {
			continue
		}

		isPoisoned := false
		if val, ok := res.Document.Metadata["is_poisoned"].(bool); ok && val {
			isPoisoned = true
		}

		if isPoisoned {
			hasPoisoned = true
			correction, _ := res.Document.Metadata["correction"].(string)
			poisonedSb.WriteString(fmt.Sprintf("- ERROR: %s\n", res.Document.Text))
			if correction != "" {
				poisonedSb.WriteString(fmt.Sprintf("  CORRECTION: %s\n", correction))
			}
			seenIDs[res.Document.ID] = struct{}{}
			continue
		}

		if res.Score > a.RelevanceThreshold && memoryCount < a.MaxResults {
			sb.WriteString(fmt.Sprintf("%d. (Rel: %.2f) %s\n", memoryCount+1, res.Score, res.Document.Text))
			seenIDs[res.Document.ID] = struct{}{}
			memoryCount++
			go a.Manager.IncrementAccess(ctx, res.Document.ID)
		}
	}

	final := ""
	if hasPoisoned {
		poisonedSb.WriteString("---------------------------------------------------\n")
		final += poisonedSb.String()
	}
	if memoryCount > 0 {
		sb.WriteString("-------------------------------\n")
		final += sb.String()
	}
	return final
}

// RecordExecution embeds a completed step result and places it into the Tier-1 bounded channel.
// Non-blocking: if the channel is full, the item is silently dropped.
// ADR-0015: items in the channel are immediately searchable by Query via linear cosine scan.
func (a *Agent) RecordExecution(ctx context.Context, result domain.StepResult) error {
	// ADR-0049 D3: a single-action step's prose output merely restates its one action
	// record — drop it (structural, by action count; no text comparison, no LLM).
	// Multi-action and uncorrelated/zero-action steps keep the synthesis.
	if dropStepSynthesis(a.takeActionCount(result.TaskID)) {
		slog.Debug("MemoryAgent: dropping single-action step synthesis", "task_id", result.TaskID, "step", result.Index)
		return nil
	}

	text := fmt.Sprintf("step_%d: %s", result.Index, result.Output)

	vec, err := a.Manager.Embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("MemoryAgent: failed to embed step result for Tier-1 channel", "err", err)
		return nil
	}

	docID := a.nextDocID("tier1")
	metadata := map[string]interface{}{
		"timestamp":    time.Now().Format(time.RFC3339),
		"source_agent": "System",
		"step_index":   result.Index,
	}
	if result.TaskID != "" {
		metadata["task_id"] = result.TaskID // groups a multi-action step's synthesis with its actions
	}
	// ADR-0049 D5: per-step snapshots are no longer scenes (scenes are plan-wide, written
	// once at completion by WritePlanScene). Not stored, so the Tier-2 createSceneDoc no-ops.

	doc := &domain.Document{
		ID:                 docID,
		DocumentType:       domain.DocTypeMnemonicFact,
		Text:               text,
		ActivationStrength: 0.1,
		Metadata:           metadata,
	}

	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if len(a.pendingItems) >= a.pendingCap {
		slog.Debug("MemoryAgent: Tier-1 channel full, dropping item", "doc_id", docID)
		return nil
	}
	a.pendingItems = append(a.pendingItems, pendingItem{Embedding: vec, Doc: doc, SceneID: result.SceneID, SessionID: result.SessionID})

	// ADR-0017: Synchronous pattern-matched edge writing (closes, discussed_in).
	if a.GraphStore != nil {
		a.writeSyncEdges(ctx, docID, result.Output)
	}
	// ADR-0049 D10: BUFFER structural `follows` edges from this step's record to its
	// dependency steps' records (deterministic, from the DAG structure). They are written
	// later by commitFollows — once this record (and its targets) are actually persisted —
	// because the record here is only pending and the Tier-2 drain may yet drop it.
	a.bufferFollowsEdges(result.TaskID, docID, result.DependsOnTaskIDs)

	return nil
}

// RecordToolOutput feeds a successful tool output into the Tier-1 pending channel
// (ADR-0048 D6); Tier-2 curation then LLM-scores it for promotion to durable LTM.
// Unlike step records (source_agent="System", excluded from same-session recall by
// D1), a tool output carries source_agent="ToolOutput" so it stays recallable. The
// kernel only feeds it here — the keep/drop value judgment is the Tier-2 LLM's.
func (a *Agent) RecordToolOutput(ctx context.Context, rec domain.ToolOutputRecord) error {
	if rec.IsMutation {
		// A mutation additionally writes an action EVENT ("what I did"); entity
		// engagement happens inside, stamped with the action's docID as provenance.
		return a.recordActionRecord(ctx, rec)
	}
	// A READ is DISCOVERY (ADR-0049): it still engages its entities — reading a file/url
	// tells us the thing exists and what its content is, which is world-model knowledge
	// worth keeping. No action event (a read is not "something I did"); it flows to the
	// fact lane. No action-docID provenance (a read has no durable event record).
	a.engageEntities(ctx, rec, "")
	return a.recordReadFact(ctx, rec)
}

// engageEntities populates the world model from ONE tool engagement — read or write
// (ADR-0049 D5/D6/D8/D9). It offloads the observed content ONCE to CAS (the baseline; a
// read's output is the real content), then for every engaged thing: mints/enriches its
// entity record (field-LWW) and accretes it into the plan's scene scope. actionDocID is
// the provenance link for a mutation (its action event), "" for a read.
func (a *Agent) engageEntities(ctx context.Context, rec domain.ToolOutputRecord, actionDocID string) {
	ents := engagedEntities(rec.ToolName, rec.ArgsJSON)
	if len(ents) == 0 {
		return
	}
	baseline := a.offloadBaseline(rec.Output)
	if baseline == "" {
		baseline = contentFingerprint(rec.Output) // change-detection even without a ContentStore
	}
	a.accreteEngaged(rec.TaskID, ents, baseline)                               // scene scope, CANONICAL keys (D5/D6)
	a.mintEntities(ctx, rec.ToolName, ents, baseline, actionDocID, rec.TaskID) // entity records (D8/D9)
}

// accreteEngaged adds an engagement's CANONICAL entity keys to its plan's scene scope
// (ADR-0049 D5/D6), keyed by the planID parsed from the TaskID. First-touch wins (the
// baseline ref is the scene's record of the thing's initial observed state). Using the
// canonical entity key (not a raw arg ref) means the scene's engaged set lines up exactly
// with the entity records — so scene→entity graph edges are FK-safe, and degenerate paths
// (the cwd) are already excluded upstream by engagedEntities.
func (a *Agent) accreteEngaged(taskID string, ents []canonicalEntity, baseline string) {
	planID := planIDFromTaskID(taskID)
	if planID == "" {
		return
	}
	a.planEntitiesMu.Lock()
	defer a.planEntitiesMu.Unlock()
	if a.planEntities[planID] == nil {
		a.planEntities[planID] = map[string]string{}
	}
	for _, e := range ents {
		if _, seen := a.planEntities[planID][e.Key()]; !seen {
			a.planEntities[planID][e.Key()] = baseline
		}
	}
}

// recordActionRecord persists a mutation as a durable `mnemonic_action` EVENT
// (ADR-0049 D1): a compact deterministic line ("what was done where"), embedded and
// committed DIRECTLY — bypassing the Tier-2 keep/drop judgment, because a write that
// happened is history, not relevance-scored knowledge to maybe discard.
func (a *Agent) recordActionRecord(ctx context.Context, rec domain.ToolOutputRecord) error {
	text := domain.FormatActionLine(rec.ToolName, rec.ArgsJSON, rec.Output)
	vec, err := a.Manager.Embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("MemoryAgent: failed to embed action record", "err", err)
		return nil
	}
	sid, _ := domain.SessionIDFromContext(ctx)
	doc := &domain.Document{
		ID:                 a.nextDocID("action"),
		DocumentType:       domain.DocTypeMnemonicAction,
		Text:               text,
		ActivationStrength: 0.1,
		Embedding:          domain.Embedding{Vector: vec},
		Metadata: map[string]interface{}{
			"timestamp":    time.Now().Format(time.RFC3339),
			"source_agent": "ToolOutput",
			"tool":         rec.ToolName,
		},
	}
	if sid != "" {
		doc.Metadata["session_id"] = sid
	}
	if rec.TaskID != "" {
		doc.Metadata["task_id"] = rec.TaskID
		// ADR-0049 D11: stamp the planID so a scene's action PATH is resolvable by
		// metadata containment (the precedent lane walks plan_id → its actions).
		if planID := planIDFromTaskID(rec.TaskID); planID != "" {
			doc.Metadata["plan_id"] = planID
		}
	}
	if err := a.Manager.Store.Save(ctx, doc); err != nil {
		slog.Warn("MemoryAgent: failed to save action record", "doc_id", doc.ID, "err", err)
	}
	a.bumpActionCount(rec.TaskID)      // ADR-0049 D3: count for the step's dedup decision
	a.engageEntities(ctx, rec, doc.ID) // ADR-0049 D5/D6/D8/D9: world model, w/ action provenance
	return nil
}

// mintEntities upserts a first-class `mnemonic_entity` record for each real thing this
// engagement touched (ADR-0049 D8/D9) — read OR write, since a read is discovery too.
// Identity is the canonical kind:id, so the store upsert collapses the same thing (and
// all an api's endpoints) onto ONE record — no fragmentation, no embedding (entities are
// reached by exact id, not semantic search). The record's materialized fields are
// field-LWW folded (D9): this engagement's observation (exists + the observed content
// baseline, or exists=false for a delete) is merged onto the stored view by ordinal, and
// the engaging action's docID (empty for a read) is appended as a provenance link — so
// the cache is rebuildable from provenance.
func (a *Agent) mintEntities(ctx context.Context, toolName string, ents []canonicalEntity, contentRef, actionDocID, taskID string) {
	if a.Manager == nil || a.Manager.Store == nil {
		return
	}
	seq := a.docSeq.Add(1) // monotonic ordinal — the field-LWW key for this engagement
	now := time.Now()
	for _, ent := range ents {
		fields := observedFields(ent.Kind, toolName, contentRef)
		if ent.Endpoint != "" {
			fields["endpoint"] = ent.Endpoint // last-observed endpoint (an attribute of the api)
		}
		obs := entityObservation{Seq: seq, At: now, Fields: fields}
		a.upsertEntity(ctx, ent, obs, toolName, actionDocID, taskID, now)
	}
}

// upsertEntity read-modify-writes one entity: it merges this observation onto the stored
// materialized field set (field-LWW) and appends the provenance link. The merge is
// idempotent under replay (ordinal-keyed), so a lost race is self-healing — the cache is
// rebuildable from provenance (ADR-0049 D9). Reconstruction "what's true now" (Issue 012)
// reads these same materialized fields.
func (a *Agent) upsertEntity(ctx context.Context, ent canonicalEntity, obs entityObservation, toolName, actionDocID, taskID string, now time.Time) {
	existing, _ := a.Manager.Store.GetByID(ctx, ent.Key())
	cur := decodeEntityFields(existing)
	// ADR-0049 §A1.2: detect fields this observation CHANGES from cache. Only a READ
	// (no mutating verb) discovering a difference is "drift" (the world moved under us);
	// a write's change is intentional supersession, not drift.
	var drift []fieldDelta
	if actionVerb(toolName) == "" {
		drift = detectFieldDrift(cur, obs)
	}
	merged := mergeObservation(cur, obs)
	prov := appendProvenance(decodeProvenance(existing), actionDocID)

	meta := map[string]interface{}{
		"kind":         ent.Kind,
		"canonical_id": ent.ID,
		"last_tool":    toolName,
		"last_seen":    now.Format(time.RFC3339),
		// ADR-0049 §A1.1 / ADR-0051 D3: the entity-level valid-time staleness signal the
		// Scout reads to decide trust-prior-vs-re-observe. Equals the most recent
		// observation time; refreshed on every engagement (read or write).
		"last_observed_at": materializedObservedAt(merged).Format(time.RFC3339),
		"fields":           encodeJSON(merged), // the materialized cache (rebuildable from provenance)
		"provenance":       encodeJSON(prov),   // links to engaging actions — history is walked, not stored inline
	}
	// ADR-0051 D3 (issue-002): stamp the engaging session so the Scout's staleness lookup
	// can tell "we engaged this entity THIS session" (trusted for controlled kinds) from a
	// stale prior-session observation.
	if sid, ok := domain.SessionIDFromContext(ctx); ok && sid != "" {
		meta["session_id"] = sid
	}
	if planID := planIDFromTaskID(taskID); planID != "" {
		meta["last_scene"] = "scene-" + planID // most-recent engaging scene (Issue 012 access path)
	}
	// Mirror exists at top-level so a reconstruction query can exclude superseded
	// (deleted) entities via QueryByMetadata containment.
	if ex, ok := merged["exists"]; ok {
		if b, ok := ex.Value.(bool); ok {
			meta["exists"] = b
		}
	}
	text := ent.Kind + " " + ent.ID
	if ent.Endpoint != "" {
		text += " " + ent.Endpoint
	}
	doc := &domain.Document{
		ID:                 ent.Key(),
		DocumentType:       domain.DocTypeMnemonicEntity,
		Text:               text,
		ActivationStrength: 0.1,
		Metadata:           meta,
	}
	if err := a.Manager.Store.Save(ctx, doc); err != nil {
		slog.Warn("MemoryAgent: failed to mint entity", "entity", ent.Key(), "err", err)
		return
	}
	// ADR-0049 §A1.2: emit a PASSIVE world_delta per changed field after the entity is
	// durably updated (write-then-emit). No propagation, no in-loop rescan — just a
	// signal. Nil-safe; never blocks the write path.
	if a.EventBus != nil {
		for _, d := range drift {
			_ = a.EventBus.Publish(domain.WorldDeltaEvent{
				EntityKey:  ent.Key(),
				Kind:       ent.Kind,
				Field:      d.Field,
				OldValue:   fmt.Sprintf("%v", d.OldValue),
				NewValue:   fmt.Sprintf("%v", d.NewValue),
				ObservedAt: now,
				// SessionID is not derivable here — TaskID is step-{i}-{planID}, no session.
			})
		}
	}
}

func (a *Agent) bumpActionCount(taskID string) {
	if taskID == "" {
		return
	}
	a.actionCountMu.Lock()
	a.actionCounts[taskID]++
	a.actionCountMu.Unlock()
}

// takeActionCount reads and clears the action count for a step (by TaskID). Returns
// -1 for an unknown/uncorrelated step ("" TaskID) so dedup stays off for it.
func (a *Agent) takeActionCount(taskID string) int {
	if taskID == "" {
		return -1
	}
	a.actionCountMu.Lock()
	defer a.actionCountMu.Unlock()
	n, ok := a.actionCounts[taskID]
	delete(a.actionCounts, taskID)
	if !ok {
		return 0
	}
	return n
}

// dropStepSynthesis reports whether a step's prose output is a redundant restatement
// of its single action and should not be recorded (ADR-0049 D3). True iff EXACTLY one
// action was recorded for the step. -1 (uncorrelated) or 0/≥2 → keep the synthesis.
func dropStepSynthesis(actionCount int) bool { return actionCount == 1 }

// nextDocID builds a process-unique document ID: a timestamp plus a monotonic
// sequence so rapid sequential records never collide under a coarse clock (ADR-0049).
func (a *Agent) nextDocID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), a.docSeq.Add(1))
}

// refKind maps an engaged ref ("kind:value") to an abstract entity category, so the
// retrieval projection compares by SHAPE, not specific identity (ADR-0049 D7).
func refKind(ref string) string {
	prefix := ref
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		prefix = ref[:i]
	}
	switch prefix {
	case "path", "file", "filename":
		return "file"
	case "dir", "directory":
		return "directory"
	case "api", "url", "uri", "host", "endpoint":
		return "web resource"
	case "command":
		return "shell command"
	default:
		return "resource"
	}
}

// sceneProjection renders the ABSTRACTED, embeddable RETRIEVAL face of a scene
// (ADR-0049 D7): the goal + engaged-entity KIND counts — NOT specific IDs (those are
// the entity index) and NOT the outcome (that's a field you read after matching).
// Pure/deterministic; this is what "situations like this?" matches against.
func sceneProjection(goal string, refs []string) string {
	counts := map[string]int{}
	for _, r := range refs {
		counts[refKind(r)]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	proj := "goal: " + goal
	if len(parts) > 0 {
		proj += " | engages: " + strings.Join(parts, ", ")
	}
	return proj
}

// planIDFromTaskID extracts the planID embedded in a step TaskID ("step-{i}-{planID}").
func planIDFromTaskID(taskID string) string {
	parts := strings.SplitN(taskID, "-", 3)
	if len(parts) == 3 && parts[0] == "step" {
		return parts[2]
	}
	return ""
}

// offloadBaseline stores an entity's first-touch baseline (the observed tool output)
// in CAS and returns its CID (ADR-0049 D6 — baselines by reference). Ownerless/public
// (durable, cross-session, resolvable via get_context_node). "" when there is no
// ContentStore or on failure — the ref is still recorded, just without a baseline.
func (a *Agent) offloadBaseline(output []byte) string {
	if a.ContentStore == nil || len(output) == 0 {
		return ""
	}
	cid, err := a.ContentStore.Put(context.Background(), output, "scene_baseline", nil, "")
	if err != nil {
		return ""
	}
	return string(cid)
}

// WritePlanScene materializes the ONE immutable scene for a completed plan (ADR-0049
// D5): id `scene-{planID}` (pre-allocated/derivable, so mid-plan actions reference it
// via their planID), holding the goal + the engaged-entity scope accreted during the
// plan. Written once at completion; the per-plan accumulator is then cleared.
func (a *Agent) WritePlanScene(ctx context.Context, planID, goal string, success bool) error {
	if planID == "" || a.Manager == nil {
		return nil
	}
	goal = normalizeSceneGoal(goal)
	a.planEntitiesMu.Lock()
	set := a.planEntities[planID]
	delete(a.planEntities, planID)
	a.planEntitiesMu.Unlock()

	// ADR-0049 D5/D7: a scene anchors a WORLD-STATE transition. A plan that engaged no
	// entity transitioned no world state — its "scene" would be a bare goal label that
	// only ever matches on goal-text similarity, polluting situational retrieval. Skip
	// it: no engaged entities → no scene. (Pure-reasoning/math plans correctly produce
	// none; their value, if any, is an episodic record, not a world-model scene.)
	if len(set) == 0 {
		slog.Debug("MemoryAgent: skipping contentless scene (no engaged entities)", "plan_id", planID, "goal", goal)
		return nil
	}

	refs := make([]string, 0, len(set))
	for r := range set {
		refs = append(refs, r)
	}
	sort.Strings(refs)

	// RECONSTRUCTION face (Text): refs + baseline CIDs — descriptors and pointers, never
	// raw content (ADR-0049 D6). RETRIEVAL face (Embedding): the abstracted projection
	// (ADR-0049 D7) — match on situation SHAPE, not specific IDs.
	rendered := make([]string, 0, len(refs))
	for _, r := range refs {
		if cid := set[r]; cid != "" {
			rendered = append(rendered, fmt.Sprintf("%s (baseline cid:%s)", r, cid))
		} else {
			rendered = append(rendered, r)
		}
	}
	// Inline ACTION SUMMARY (the "what I did" path): resolve this plan's action records
	// — saved synchronously before this call — into compact lines, so the scene is a
	// self-contained transition at a glance (situation + what was done + outcome), not
	// just a situational index that must be joined to the action lane at query time.
	actionLines := a.planActionLines(ctx, planID)

	text := "plan goal: " + goal
	if len(rendered) > 0 {
		text += " | engaged: " + strings.Join(rendered, ", ")
	}
	if len(actionLines) > 0 {
		text += " | actions: " + strings.Join(actionLines, "; ")
	}

	projection := sceneProjection(goal, refs)
	vec, err := a.Manager.Embedder.Embed(ctx, projection) // embed the ABSTRACTED projection
	if err != nil {
		slog.Warn("MemoryAgent: failed to embed plan scene", "plan_id", planID, "err", err)
		return nil
	}
	outcome := "failure"
	if success {
		outcome = "success"
	}
	doc := &domain.Document{
		ID:                 "scene-" + planID,
		DocumentType:       domain.DocTypeMnemonicScene,
		Text:               text,
		ActivationStrength: 0.1,
		Embedding:          domain.Embedding{Vector: vec},
		Metadata: map[string]interface{}{
			"timestamp":  time.Now().Format(time.RFC3339),
			"plan_id":    planID,
			"engaged":    set,         // map ref → baseline cid (by reference)
			"projection": projection,  // the abstracted retrieval face
			"outcome":    outcome,     // a FIELD, not part of the similarity key
			"actions":    actionLines, // the inline action path ("what I did")
		},
	}
	if err := a.Manager.Store.Save(ctx, doc); err != nil {
		slog.Warn("MemoryAgent: failed to save plan scene", "plan_id", planID, "err", err)
		return nil
	}

	// ADR-0049: connect the world model in the graph. A scene→entity `engaged` edge per
	// engaged thing — both endpoints are persisted (the scene just above; the entities
	// during the plan), so these are FK-SAFE and populate document_edges reliably (unlike
	// follows/co_activated, which depend on Tier-2 survival or non-empty recall). This is
	// what makes spreading activation traverse the experiential world model.
	if a.GraphStore != nil {
		for _, r := range refs {
			edge := domain.DocumentEdge{SourceID: doc.ID, TargetID: r, EdgeType: domain.EdgeEngaged, Weight: 0.6}
			if err := a.GraphStore.SaveEdge(ctx, edge); err != nil {
				slog.Debug("MemoryAgent: scene→entity edge write failed", "entity", r, "err", err)
			}
		}
	}
	return nil
}

// maxInlineSceneActions caps how many action lines a scene embeds inline, so a long plan
// cannot bloat the scene Text. The full path stays queryable via the action lane (plan_id).
const maxInlineSceneActions = 20

// planActionLines resolves a plan's action records (its "what I did" path) into compact
// lines for the scene's inline summary. Reuses the precedent lane's resolver (plan_id →
// time-ordered action docs). Capped; best-effort (a nil/empty store yields nothing).
func (a *Agent) planActionLines(ctx context.Context, planID string) []string {
	if a.Manager == nil || a.Manager.Store == nil {
		return nil
	}
	docs := resolveActionPath(ctx, a.Manager.Store, planID)
	lines := make([]string, 0, len(docs))
	for _, d := range docs {
		if t := strings.TrimSpace(d.Text); t != "" {
			lines = append(lines, t)
		}
		if len(lines) >= maxInlineSceneActions {
			break
		}
	}
	return lines
}

// normalizeSceneGoal canonicalizes a plan goal for the scene's situation (ADR-0049 D7).
// A replan plan's Subject is LLM-prefixed with "Replan: <original>" (replan_handler) and
// gets a fresh planID — so its scene would otherwise read as a generic "Replan: …"
// situation, distinct from the original it actually continues. Stripping the prefix makes
// the failed original and its replan project to the SAME situation, which is exactly what
// the precedent lane should match on. Pure; idempotent; strips repeated prefixes.
func normalizeSceneGoal(goal string) string {
	g := strings.TrimSpace(goal)
	for {
		trimmed := strings.TrimSpace(strings.TrimPrefix(g, "Replan:"))
		if trimmed == g {
			return g
		}
		g = trimmed
	}
}

// followsEdges derives the `follows` edges for a step (ADR-0049 D10): one edge from
// the step's record to each already-recorded predecessor's record. Pure — a function
// of the step docID + resolved predecessor docIDs. Skips empties and self-loops.
func followsEdges(stepDocID string, predDocIDs []string) []domain.DocumentEdge {
	if stepDocID == "" {
		return nil
	}
	edges := make([]domain.DocumentEdge, 0, len(predDocIDs))
	for _, pred := range predDocIDs {
		if pred == "" || pred == stepDocID {
			continue
		}
		edges = append(edges, domain.DocumentEdge{
			SourceID: stepDocID, TargetID: pred, EdgeType: domain.EdgeFollows, Weight: 0.5,
		})
	}
	return edges
}

func (a *Agent) resolvePredDocs(predTaskIDs []string) []string {
	if len(predTaskIDs) == 0 {
		return nil
	}
	a.stepDocsMu.Lock()
	defer a.stepDocsMu.Unlock()
	out := make([]string, 0, len(predTaskIDs))
	for _, tid := range predTaskIDs {
		if id, ok := a.stepDocs[tid]; ok {
			out = append(out, id)
		}
	}
	return out
}

// maxPendingFollows caps the follows-edge buffer so a never-committed source (e.g. a
// Tier-2-dropped step record) cannot leak edges unboundedly.
const maxPendingFollows = 512

// bufferFollowsEdges records this step's docID under its TaskID and BUFFERS its `follows`
// edges to its predecessors (ADR-0049 D10). The edges are not written here: the step's
// fact record is only PENDING (async Tier-2 drain may persist or drop it), so writing now
// would FK-violate against a non-existent row. commitFollows flushes them once both
// endpoints are committed. No-op without a GraphStore. A single-action step (whose
// synthesis is dropped, D3) never reaches here — it is simply absent from the chain.
func (a *Agent) bufferFollowsEdges(taskID, docID string, predTaskIDs []string) {
	if taskID != "" && docID != "" {
		a.stepDocsMu.Lock()
		a.stepDocs[taskID] = docID
		a.stepDocsMu.Unlock()
	}
	if a.GraphStore == nil {
		return
	}
	edges := followsEdges(docID, a.resolvePredDocs(predTaskIDs))
	if len(edges) == 0 {
		return
	}
	a.followsMu.Lock()
	a.pendingFollows = append(a.pendingFollows, edges...)
	if len(a.pendingFollows) > maxPendingFollows {
		a.pendingFollows = a.pendingFollows[len(a.pendingFollows)-maxPendingFollows:]
	}
	a.followsMu.Unlock()
}

// commitFollows marks docID as persisted and writes any buffered `follows` edges whose
// BOTH endpoints are now committed (ADR-0049 D10 FK-safety). Called from commitItem
// after a step record is Saved. Edges whose other endpoint is still pending (or was
// dropped) stay buffered / are eventually capped — never written against a missing row.
func (a *Agent) commitFollows(ctx context.Context, docID string) {
	if a.GraphStore == nil || docID == "" {
		return
	}
	a.followsMu.Lock()
	a.committedDocs[docID] = true
	ready := make([]domain.DocumentEdge, 0, len(a.pendingFollows))
	remaining := a.pendingFollows[:0]
	for _, e := range a.pendingFollows {
		if a.committedDocs[e.SourceID] && a.committedDocs[e.TargetID] {
			ready = append(ready, e)
		} else {
			remaining = append(remaining, e)
		}
	}
	a.pendingFollows = remaining
	// When nothing is awaiting an endpoint, the commit markers are no longer needed —
	// clearing them bounds memory over a long session.
	if len(a.pendingFollows) == 0 {
		a.committedDocs = map[string]bool{}
	}
	a.followsMu.Unlock()

	for _, e := range ready {
		if err := a.GraphStore.SaveEdge(ctx, e); err != nil {
			slog.Debug("MemoryAgent: follows edge write failed", "err", err)
		}
	}
}

// recordReadFact feeds a READ tool's payload into the Tier-1 → Tier-2 fact pipeline
// (ADR-0048 D6): its payload is knowledge, scored/curated like any other fact.
func (a *Agent) recordReadFact(ctx context.Context, rec domain.ToolOutputRecord) error {
	text := fmt.Sprintf("tool[%s]: %s", rec.ToolName, string(rec.Output))

	vec, err := a.Manager.Embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("MemoryAgent: failed to embed tool output for Tier-1 channel", "err", err)
		return nil
	}

	sid, _ := domain.SessionIDFromContext(ctx)
	docID := a.nextDocID("tier1-tool")
	doc := &domain.Document{
		ID:                 docID,
		DocumentType:       domain.DocTypeMnemonicFact,
		Text:               text,
		ActivationStrength: 0.1,
		Metadata: map[string]interface{}{
			"timestamp":    time.Now().Format(time.RFC3339),
			"source_agent": "ToolOutput",
			"tool":         rec.ToolName,
		},
	}

	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if len(a.pendingItems) >= a.pendingCap {
		slog.Debug("MemoryAgent: Tier-1 channel full, dropping tool output", "doc_id", docID)
		return nil
	}
	a.pendingItems = append(a.pendingItems, pendingItem{Embedding: vec, Doc: doc, SessionID: sid})
	return nil
}

// EnqueueExternal appends pre-embedded external document chunks into the Tier-1
// pending channel so they flow through the Tier-2 dual-coding pipeline (ADR-0028).
// Items that exceed pendingCap are silently dropped with a WARN log.
func (a *Agent) EnqueueExternal(ctx context.Context, items []pendingItem) error {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	for _, item := range items {
		if len(a.pendingItems) >= a.pendingCap {
			slog.Warn("IngestionManager: Tier-1 channel full, dropping external chunk",
				"doc_id", item.Doc.ID)
			continue
		}
		a.pendingItems = append(a.pendingItems, item)
	}
	return nil
}

// Query performs a linear cosine scan over the Tier-1 channel first, then queries pgvector.
// Results from both sources are merged and returned together. ADR-0015.
func (a *Agent) Query(ctx context.Context, prompt string, topK int) ([]domain.SearchResult, error) {
	queryVec, err := a.Manager.Embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to embed query: %w", err)
	}

	var merged []domain.SearchResult

	// Pass 1: linear cosine scan over Tier-1 pending channel
	a.pendingMu.RLock()
	pendingCopy := make([]pendingItem, len(a.pendingItems))
	copy(pendingCopy, a.pendingItems)
	a.pendingMu.RUnlock()

	for _, p := range pendingCopy {
		if len(p.Embedding) == 0 {
			continue
		}
		sim := cosineSimilarity(queryVec, p.Embedding)
		merged = append(merged, domain.SearchResult{
			Document: *p.Doc,
			Score:    sim,
		})
	}

	// Pass 2: pgvector semantic search — DocTypeMemory retired (ADR-0025); query mnemonic_fact.
	pgResults, err := a.Manager.Store.Search(ctx, queryVec, domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact,
		TopK:         topK,
		Scope:        domain.ScopeSystem, // ADR-0034: kernel curation read (dedup/neighbor expansion)
	})
	if err != nil {
		slog.Warn("MemoryAgent: pgvector search failed, returning only Tier-1 results", "err", err)
	} else {
		merged = append(merged, pgResults...)
	}

	// Sort by score descending, then take top topK
	sortSearchResults(merged)
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged, nil
}

func sortSearchResults(results []domain.SearchResult) {
	for i := 1; i < len(results); i++ {
		j := i
		for j > 0 && results[j].Score > results[j-1].Score {
			results[j], results[j-1] = results[j-1], results[j]
			j--
		}
	}
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// IngestNegativeEdge stores a failure record as a negative_edge document.
func (a *Agent) IngestNegativeEdge(ctx context.Context, errorMsg, lastOutput, agentID string) error {
	text := fmt.Sprintf("FAILURE: %s\nLAST OUTPUT: %s", errorMsg, lastOutput)
	docID := fmt.Sprintf("neg-edge-%d", time.Now().UnixNano())
	doc := &domain.Document{
		ID:                 docID,
		DocumentType:       domain.DocTypeNegativeEdge,
		Text:               text,
		ActivationStrength: 0.1,
		Metadata: map[string]interface{}{
			"timestamp":    time.Now().Format(time.RFC3339),
			"source_agent": agentID,
			"doc_type":     "negative_edge",
		},
	}
	return a.Manager.Store.Save(ctx, doc)
}

// PoisonMemory marks a specific memory as "Poisoned" and adds a correction.
func (a *Agent) PoisonMemory(ctx context.Context, memoryID string, correction string) error {
	doc, err := a.Manager.GetByID(ctx, memoryID)
	if err != nil || doc == nil {
		return fmt.Errorf("memory not found: %w", err)
	}

	if doc.Metadata == nil {
		doc.Metadata = make(map[string]interface{})
	}
	doc.Metadata["is_poisoned"] = true
	doc.Metadata["correction"] = correction

	return a.Manager.Store.Save(ctx, doc)
}

func (a *Agent) maskSensitiveData(text string) string {
	apiKeyRegex := regexp.MustCompile(`(?i)(api_?key|secret|token|password)[\s:=]+['"]?([a-zA-Z0-9_\-]{8,})['"]?`)
	masked := apiKeyRegex.ReplaceAllString(text, "$1=***MASKED***")
	skRegex := regexp.MustCompile(`sk-[a-zA-Z0-9]{32,}`)
	return skRegex.ReplaceAllString(masked, "***MASKED_API_KEY***")
}

func (a *Agent) evaluateImportance(ctx context.Context, text string) (int, error) {
	prompt := domain.PromptBuild(
		domain.PromptSystem(importanceRole, importanceRules),
		domain.PromptTask(fmt.Sprintf("Text to evaluate:\n\"%s\"", text)),
		domain.PromptOutputSchemaInteger(1, 10),
	)

	resp, err := a.LLMClient.Generate(ctx, prompt)
	if err != nil {
		return 0, err
	}

	re := regexp.MustCompile(`\d+`)
	match := re.FindString(resp)
	if match != "" {
		score, _ := strconv.Atoi(match)
		return score, nil
	}
	return 0, fmt.Errorf("could not parse score: %s", resp)
}

// ── Tier-2 drain (ADR-0015) ──────────────────────────────────────────────

// StartTier2Drain launches the background goroutine that drains the Tier-1 channel
// in batches, scores items via the Generator, and commits to pgvector.
// Call StopTier2Drain to shut down.
func (a *Agent) StartTier2Drain(ctx context.Context) {
	a.tier2Stop = make(chan struct{})
	go a.tier2DrainLoop(context.WithoutCancel(ctx))
}

// StopTier2Drain signals the Tier-2 goroutine to stop.
func (a *Agent) StopTier2Drain() {
	if a.tier2Stop != nil {
		close(a.tier2Stop)
	}
}

func (a *Agent) tier2DrainLoop(ctx context.Context) {
	timer := time.NewTimer(a.tier2MaxIdle)
	defer timer.Stop()

	for {
		a.pendingMu.RLock()
		n := len(a.pendingItems)
		a.pendingMu.RUnlock()

		if n >= a.tier2BatchSize {
			a.drainBatch(ctx)
			timer.Reset(a.tier2MaxIdle)
			continue
		}

		select {
		case <-a.tier2Stop:
			a.drainBatch(ctx)
			return
		case <-ctx.Done():
			a.drainBatch(ctx)
			return
		case <-timer.C:
			a.drainBatch(ctx)
			timer.Reset(a.tier2MaxIdle)
		}
	}
}

func (a *Agent) drainBatch(ctx context.Context) {
	a.pendingMu.Lock()
	if len(a.pendingItems) == 0 {
		a.pendingMu.Unlock()
		return
	}
	batch := make([]pendingItem, len(a.pendingItems))
	copy(batch, a.pendingItems)
	a.pendingItems = a.pendingItems[:0]
	a.pendingMu.Unlock()

	channelDepth := len(batch)
	slog.Info("MemoryAgent: Tier-2 draining batch", "batch_size", channelDepth)

	// Heuristic error pre-filter (ADR-0025): route error items to negative_edge before LLM batch.
	var clean []pendingItem
	for _, item := range batch {
		if isErr, kind := IsErrorOutput(item.Doc.Text); isErr {
			a.commitErrorItem(ctx, item, kind)
		} else {
			clean = append(clean, item)
		}
	}

	if len(clean) == 0 {
		a.commitBatch(ctx, nil, channelDepth)
		return
	}

	scored, err := a.scoreBatch(ctx, clean)
	if err != nil {
		slog.Warn("MemoryAgent: Tier-2 LLM scoring failed, using heuristic", "err", err)
		a.tier2FallbackCount.Add(1)
		scored = a.scoreBatchHeuristic(clean)
		slog.Info("MemoryAgent: Tier-2 heuristic fallback",
			"batch_size", channelDepth,
			"tier2_llm_fallback_count", a.tier2FallbackCount.Load())
	}

	a.commitBatch(ctx, scored, channelDepth)
}

// commitErrorItem writes a standardised negative_edge document for a pre-filter match. ADR-0025.
func (a *Agent) commitErrorItem(ctx context.Context, item pendingItem, kind ErrorKind) {
	agentID, _ := item.Doc.Metadata["agent_id"].(string)
	stepIndex, _ := item.Doc.Metadata["step_index"].(int)
	doc := &domain.Document{
		ID:                 fmt.Sprintf("neg-prefilter-%d", time.Now().UnixNano()),
		DocumentType:       domain.DocTypeNegativeEdge,
		Text:               item.Doc.Text,
		ActivationStrength: 0.1,
		Metadata: map[string]interface{}{
			"error_type": string(kind),
			"raw_output": item.Doc.Text,
			"agent_id":   agentID,
			"step_index": stepIndex,
			"timestamp":  time.Now().Format(time.RFC3339),
		},
	}
	if err := a.Manager.Store.Save(ctx, doc); err != nil {
		slog.Warn("MemoryAgent: failed to commit error item as negative_edge", "err", err)
	}
	slog.Info("MemoryAgent: error item routed to negative_edge",
		"error_kind", kind, "agent_id", agentID, "step_index", stepIndex)
}

func (a *Agent) scoreBatch(ctx context.Context, batch []pendingItem) ([]scoredItem, error) {
	var textsB strings.Builder
	for _, item := range batch {
		textsB.WriteString(item.Doc.Text)
		textsB.WriteString("\n---\n")
	}

	prompt := domain.PromptBuild(
		domain.PromptSystem(
			"You are a memory quality judge.",
			`Score each result across three dimensions (1-10 each):
- relevance: how relevant is this result to the task?
- specificity: how specific and actionable is the information?
- explicitness: how clear and explicit is the information?

Tier rules: average >= 7 → FULL, average >= 4 → FACT_ONLY, average < 4 → DROP. Use deterministic mode.

Also emit "descriptor": ONE plain-language sentence (<= 160 chars) stating what the
result CONTAINS — a legible introduction for when it later surfaces in context
(e.g. "Web search results listing the world's longest rivers."). Summarize the
content; do NOT copy a raw JSON head.`,
		),
		domain.PromptTask("Results to score:\n"+textsB.String()),
		domain.PromptOutputSchemaJSON(scoringSchema),
	)

	scoreCtx, cancel := context.WithTimeout(ctx, a.tier2LLMTimeout)
	defer cancel()

	resp, err := a.LLMClient.Generate(scoreCtx, prompt)
	if err != nil {
		return nil, err
	}

	type llmScore struct {
		Relevance    int    `json:"relevance"`
		Specificity  int    `json:"specificity"`
		Explicitness int    `json:"explicitness"`
		Tier         string `json:"tier"`
		Descriptor   string `json:"descriptor"`
	}

	var raw []llmScore
	match := domain.ExtractJSONArray(resp)
	if match == "" || json.Unmarshal([]byte(match), &raw) != nil {
		return nil, fmt.Errorf("Tier-2: unparseable LLM response")
	}

	if len(raw) != len(batch) {
		return nil, fmt.Errorf("Tier-2: score count %d != batch count %d", len(raw), len(batch))
	}

	scored := make([]scoredItem, len(batch))
	for i, s := range raw {
		tier := s.Tier
		if tier != "FULL" && tier != "FACT_ONLY" && tier != "DROP" {
			tier = "FACT_ONLY"
		}
		scored[i] = scoredItem{
			Item:         batch[i],
			Relevance:    s.Relevance,
			Specificity:  s.Specificity,
			Explicitness: s.Explicitness,
			Tier:         tier,
			Descriptor:   strings.TrimSpace(s.Descriptor),
		}
	}
	return scored, nil
}

func (a *Agent) scoreBatchHeuristic(batch []pendingItem) []scoredItem {
	scored := make([]scoredItem, len(batch))
	for i, item := range batch {
		avg := 5
		if len(item.Doc.Text) > 200 {
			avg = 6
		} else if len(item.Doc.Text) < 20 {
			avg = 3
		}
		scored[i] = scoredItem{
			Item:         item,
			Relevance:    avg,
			Specificity:  avg,
			Explicitness: avg,
			Tier:         "FACT_ONLY",
			Descriptor:   heuristicDescriptor(item.Doc.Text),
		}
	}
	return scored
}

// heuristicDescriptor derives a clean one-line descriptor from a raw item text
// WITHOUT an LLM (the Tier-2 fallback path, ADR-0048 #2). It is still better than
// the raw payload as an indexed/recall surface: a "tool[name]: …" output collapses
// to "Output of tool 'name' (N chars).", and any other text to a whitespace-
// collapsed head. Never the full JSON dump.
func heuristicDescriptor(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if m := toolOutputPrefixRE.FindStringSubmatch(text); m != nil {
		return fmt.Sprintf("Output of tool %q (%d chars).", m[1], len(text))
	}
	const cap = 160
	if len(t) <= cap {
		return t
	}
	return t[:cap] + "…"
}

// toolOutputPrefixRE matches the "tool[name]: …" envelope RecordToolOutput writes.
var toolOutputPrefixRE = regexp.MustCompile(`^tool\[([^\]]+)\]:`)

// RegisterCacheInvalidator registers an invalidator to be called after every
// Tier-2 pgvector commit. WorkspaceStageImpl uses this to clear its LRU cache
// so stale query→ContextRef entries don't outlive the next pgvector drain.
func (a *Agent) RegisterCacheInvalidator(inv domain.CacheInvalidator) {
	a.invalidatorsMu.Lock()
	defer a.invalidatorsMu.Unlock()
	a.invalidators = append(a.invalidators, inv)
}

func (a *Agent) commitBatch(ctx context.Context, scored []scoredItem, channelDepth int) {
	var dropped int
	for _, s := range scored {
		a.commitItem(ctx, s)
		if s.Tier == "DROP" {
			dropped++
		}
	}

	dropRate := float64(0)
	if channelDepth > 0 {
		dropRate = float64(dropped) / float64(channelDepth)
	}

	slog.Info("MemoryAgent: Tier-2 batch committed",
		"tier2_channel_depth", channelDepth,
		"tier2_drop_rate", fmt.Sprintf("%.2f", dropRate),
		"tier2_llm_fallback_count", a.tier2FallbackCount.Load())

	// Notify cache invalidators — new documents in pgvector may change PrimeForStep results.
	a.invalidatorsMu.Lock()
	invs := a.invalidators
	a.invalidatorsMu.Unlock()
	for _, inv := range invs {
		inv.InvalidateContextRefCache()
	}
}

func (a *Agent) commitItem(ctx context.Context, s scoredItem) {
	if s.Item.SessionID != "" && s.Item.Doc.Metadata != nil {
		s.Item.Doc.Metadata["session_id"] = s.Item.SessionID
	}
	// ADR-0048 #1: EVERY promoted memory carries a one-line Summary. The Tier-2 judge
	// emits a descriptor for each item, so a step record, an external chunk, and a
	// tool output all get a gist — recall serves the Summary as the agent-facing
	// surface while the full body stays in Text. A TOOL OUTPUT additionally offloads
	// its (often large, JSON-heavy) body to CAS and re-embeds on the summary, so recall
	// can hand back {summary + content_cid} and ranking is on meaning, not JSON syntax.
	if s.Tier != "DROP" && s.Descriptor != "" {
		s.Item.Doc.Summary = s.Descriptor
		if isToolOutputItem(s.Item.Doc) {
			a.offloadFullBody(ctx, &s)
		}
	}
	switch s.Tier {
	case "FULL":
		s.Item.Doc.DocumentType = domain.DocTypeMnemonicFact
		s.Item.Doc.ScoringPromptVersion = scoringPromptHash
		s.Item.Doc.Embedding = domain.Embedding{Vector: s.Item.Embedding}
		if err := a.Manager.Store.Save(ctx, s.Item.Doc); err != nil {
			slog.Warn("MemoryAgent: Tier-2 failed to save FACT", "doc_id", s.Item.Doc.ID, "err", err)
			return
		}
		a.writeDiscussedInEdge(ctx, s.Item.Doc.ID, s.Item.SceneID)
		a.commitFollows(ctx, s.Item.Doc.ID) // ADR-0049 D10: flush follows edges now the row exists
		// ADR-0049 D3: write a Tier-2 scene ONLY when the eager WriteScene didn't
		// already produce one for this step (SceneID set) — otherwise it's a duplicate
		// scene of the same snapshot. The eager scene owns it; this is the fallback.
		if s.Item.SceneID == "" {
			if a.createSceneDoc(ctx, s) != nil {
				slog.Warn("MemoryAgent: Tier-2 failed to save SCENE", "doc_id", s.Item.Doc.ID+"-scene")
			}
		}
	case "FACT_ONLY":
		s.Item.Doc.DocumentType = domain.DocTypeMnemonicFact
		s.Item.Doc.ScoringPromptVersion = scoringPromptHash
		s.Item.Doc.Embedding = domain.Embedding{Vector: s.Item.Embedding}
		if err := a.Manager.Store.Save(ctx, s.Item.Doc); err != nil {
			slog.Warn("MemoryAgent: Tier-2 failed to save FACT_ONLY", "doc_id", s.Item.Doc.ID, "err", err)
			return
		}
		a.writeDiscussedInEdge(ctx, s.Item.Doc.ID, s.Item.SceneID)
		a.commitFollows(ctx, s.Item.Doc.ID) // ADR-0049 D10: flush follows edges now the row exists
	case "DROP":
		slog.Debug("MemoryAgent: Tier-2 dropping item", "doc_id", s.Item.Doc.ID)
	}
}

// isToolOutputItem reports whether a pending doc originated from RecordToolOutput
// (ADR-0048 D6) — the only items that get descriptor-indexed (#2). Step records and
// external chunks carry different source_agent values and keep their raw text.
func isToolOutputItem(doc *domain.Document) bool {
	if doc == nil || doc.Metadata == nil {
		return false
	}
	src, _ := doc.Metadata["source_agent"].(string)
	return src == "ToolOutput"
}

// offloadFullBody moves a promoted tool output's full body to the ContentStore so
// recall can serve {summary + content_cid} (ADR-0048 #1), and recomputes the
// embedding on the summary so retrieval ranks on meaning, not the raw JSON body. The
// Summary itself is set by the caller (commitItem) for every promoted memory; this
// adds the offload + re-embed that only a heavy tool-output body warrants.
func (a *Agent) offloadFullBody(ctx context.Context, s *scoredItem) {
	doc := s.Item.Doc

	// Offload the full body to CAS for {summary + cid} recall. Unstamped (the drain
	// ctx carries no session) ⇒ a public blob any session may resolve via
	// get_context_node — correct for durable cross-session LTM. Best-effort: a nil
	// store or Put error just leaves the full text in pgvector Text.
	if a.ContentStore != nil {
		if cid, err := a.ContentStore.Put(ctx, []byte(doc.Text), "ltm_fact", nil, s.Descriptor); err == nil {
			if doc.Metadata == nil {
				doc.Metadata = map[string]interface{}{}
			}
			doc.Metadata["content_cid"] = string(cid)
		} else {
			slog.Warn("MemoryAgent: content offload failed; full text stays in pgvector",
				"doc_id", doc.ID, "err", err)
		}
	}

	// Embed the SUMMARY so recall ranks on meaning. A re-embed failure degrades to
	// the prior (raw-body) vector rather than crashing the drain.
	if vec, err := a.Manager.Embedder.Embed(ctx, s.Descriptor); err == nil && len(vec) > 0 {
		s.Item.Embedding = vec
	} else if err != nil {
		slog.Warn("MemoryAgent: descriptor re-embed failed; indexing with prior vector",
			"doc_id", doc.ID, "err", err)
	}
}

// writeDiscussedInEdge writes a discussed_in edge from a committed FACT to its originating scene. ADR-0025.
func (a *Agent) writeDiscussedInEdge(ctx context.Context, factDocID, sceneID string) {
	if a.EdgeWriter == nil || sceneID == "" {
		return
	}
	if err := a.EdgeWriter.WriteEdge(ctx, factDocID, sceneID, "discussed_in", 0.7); err != nil {
		slog.Warn("MemoryAgent: failed to write discussed_in edge", "fact_id", factDocID, "scene_id", sceneID, "err", err)
	}
}

func (a *Agent) createSceneDoc(ctx context.Context, s scoredItem) error {
	if s.Item.Doc.Metadata == nil {
		return nil
	}
	snapshotRaw, _ := s.Item.Doc.Metadata["snapshot"]
	snapshot, _ := snapshotRaw.(string)
	if snapshot == "" {
		return nil
	}

	sceneMeta := map[string]interface{}{
		"parent_doc_id": s.Item.Doc.ID,
		"timestamp":     time.Now().Format(time.RFC3339),
	}
	if s.Item.SessionID != "" {
		sceneMeta["session_id"] = s.Item.SessionID
	}
	sceneDoc := &domain.Document{
		ID:                   s.Item.Doc.ID + "-scene",
		DocumentType:         domain.DocTypeMnemonicScene,
		Text:                 snapshot,
		ActivationStrength:   0.1,
		ScoringPromptVersion: scoringPromptHash,
		Metadata:             sceneMeta,
	}
	return a.Manager.Store.Save(ctx, sceneDoc)
}

// writeSyncEdges scans the step output for explicit references and creates document_edges.
// ADR-0017: synchronous (non-blocking) edge writing for closes + discussed_in relations.
func (a *Agent) writeSyncEdges(ctx context.Context, sourceDocID, output string) {
	if a.GraphStore == nil {
		return
	}

	// Ticket reference: ENG-1234, ABC-567, #12345
	ticketRe := regexp.MustCompile(`(?:[A-Z]+-\d+|#\d+)`)
	// PR/commit URLs
	prRe := regexp.MustCompile(`github\.com/[\w.-]+/[\w.-]+/(?:pull|commit)/[\w]+`)

	for _, match := range ticketRe.FindAllString(output, -1) {
		targetID := "ref:" + match
		a.writeEdge(ctx, sourceDocID, targetID, domain.EdgeCloses)
	}
	for _, match := range prRe.FindAllString(output, -1) {
		targetID := "ref:" + match
		a.writeEdge(ctx, sourceDocID, targetID, domain.EdgeCloses)
	}
}

func (a *Agent) writeEdge(ctx context.Context, sourceID, targetID string, edgeType domain.EdgeType) {
	var weight float32
	switch edgeType {
	case domain.EdgeCloses:
		weight = 0.6
	case domain.EdgeDiscussedIn:
		weight = 0.5
	default:
		weight = 0.5
	}
	edge := domain.DocumentEdge{
		SourceID: sourceID,
		TargetID: targetID,
		EdgeType: edgeType,
		Weight:   weight,
	}
	if err := a.GraphStore.SaveEdge(ctx, edge); err != nil {
		slog.Debug("MemoryAgent: sync edge write failed", "err", err)
	}
}

// JSON extraction from LLM responses (reasoning-wrapper-tolerant) lives in
// domain as domain.ExtractJSONArray / domain.ExtractJSONObject —
// shared with the awareness planner so the two no longer drift.
