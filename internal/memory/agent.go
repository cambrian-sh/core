package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Agent orchestrates memory curation by evaluating, deduplicating, and masking sensitive data.
type Agent struct {
	Manager              *MemoryManager
	RelevanceThreshold   float64
	MaxResults           int
	MaxNeighborExpansion int

	// RecordOutcomes gates the ADR-0049 §A2.2 experiential records: the mutation
	// action events (RecordToolOutput) and the plan-outcome hook (RecordPlanOutcome:
	// the ADR-0095 episode close, the A2.3 failure record and procedure feedback).
	// The RAW path it once sat beside — auto-embedded step results and tool payloads
	// behind `experiential_memory_enabled`, curated by the ADR-0015 tier-2 judge — was
	// deleted on 2026-11-05 (decisions/scenes-cancelled.md). Set from
	// execution.experience_records_enabled, whose config default is TRUE (owner
	// decision 2026-07-28, internal/config/config.go); the field's Go zero value is
	// false, so an Agent built without config records nothing.
	RecordOutcomes bool

	// ExperienceStore persists the episode parent row an outcome record hangs from
	// (ADR-0095 D1). May be nil → the record is still written, with a NULL parent:
	// losing the memory because its parentage could not be recorded would be worse
	// than losing the link (ADR-0095 D5).
	ExperienceStore domain.ExperienceStore

	// SurpriseFloor is the ADR-0049 A2.3 gate for the FAILURE RECORD (a negative edge
	// RecordPlanOutcome writes for a surprising failure). 0 disables the surprise arm;
	// a replan-exhausted failure is recorded regardless.
	SurpriseFloor float64

	// ProcedureDeprecateBelow is the confidence under which a repeatedly-failing routine
	// is retired (ADR-0094 D4). 0 disables procedure feedback entirely.
	ProcedureDeprecateBelow float64

	// ContentStore holds an ingested source document's body (IngestionManager,
	// ADR-0028). May be nil → the body is not offloaded.
	ContentStore domain.ContentStore

	// docSeq makes generated doc IDs unique even under a coarse clock (Windows
	// UnixNano resolution can repeat for rapid sequential calls, which would collide
	// IDs). ADR-0049.
	docSeq atomic.Uint64

	// planStarted holds, per in-flight plan, the instant its episode parent was opened
	// by BeginExperience — so the completion upsert can carry StartedAt forward instead
	// of writing a zero time over it.
	//
	// Carried in memory rather than read back from the store because the completion
	// write is an UPSERT of a whole row: the Postgres adapter's ON CONFLICT deliberately
	// leaves started_at alone, but a store that overwrote every column would otherwise
	// erase when the episode began, and the Go-side struct is the only place that
	// difference is testable without a database.
	planStarted   map[string]time.Time
	planStartedMu sync.Mutex
}

// NewAgent creates a memory Agent.
func NewAgent(manager *MemoryManager, relevanceThreshold float64, maxResults, maxNeighborExpansion int) *Agent {
	return &Agent{
		Manager:              manager,
		RelevanceThreshold:   relevanceThreshold,
		MaxResults:           maxResults,
		MaxNeighborExpansion: maxNeighborExpansion,
		planStarted:          map[string]time.Time{},
	}
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

// RecordToolOutput records a MUTATION tool output as a durable `mnemonic_action`
// EVENT (ADR-0049 D1): what was done where, in deterministic structured form, never a
// raw-JSON dump. Gated by RecordOutcomes.
//
// A READ records nothing. Its payload used to be embedded whole as a fact
// (recordReadFact, behind `experiential_memory_enabled`, curated by the ADR-0015
// tier-2 judge), and its engaged entities used to accrete the plan's scene scope.
// Both were deleted on 2026-11-05 with scenes (decisions/scenes-cancelled.md): the
// first is the raw path ADR-0049 A2.2 removed, the second had no reader but the
// scene. Knowledge in a tool output that is worth keeping goes through explicit
// ingestion, where the ADR-0136 fact layer extracts and reconciles it.
func (a *Agent) RecordToolOutput(ctx context.Context, rec domain.ToolOutputRecord) error {
	if !a.RecordOutcomes || !rec.IsMutation {
		return nil
	}
	return a.recordActionRecord(ctx, rec)
}

// recordActionRecord persists a mutation as a durable `mnemonic_action` EVENT
// (ADR-0049 D1): a compact deterministic line ("what was done where"), embedded and
// committed DIRECTLY, because a write that happened is history, not relevance-scored
// knowledge to maybe discard.
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
	if len(rec.ClassificationTags) > 0 {
		// The tool's domain tags ride as the write chokepoint's narrowing hint, so
		// the action record is classified like the artifact write already is
		// instead of landing untagged.
		//
		// ADR-0146 D5: a tool's declared owner tag fences the TOOL; it is never an
		// owner the kernel derived for the row, so it does not ride the hint (the
		// conversation owner is stamped by the write chokepoint).
		doc.Metadata["tags"] = domain.DropCallerOwnerTags(ctx, "tool classification tags (action record)", rec.ClassificationTags)
	}
	if sid != "" {
		// Metadata is an untyped, JSON-serialized edge: store the plain string, or
		// every reader's .(string) assertion silently misses.
		doc.Metadata[domain.MetaSessionID] = string(sid)
	}
	if rec.TaskID != "" {
		doc.Metadata["task_id"] = rec.TaskID
		// Stamp the planID so a plan's action path is joinable by metadata
		// containment (ADR-0137 erasure by plan, the operator's episode view).
		if planID := planIDFromTaskID(rec.TaskID); planID != "" {
			doc.Metadata["plan_id"] = planID
			// ADR-0095 D1: and hang the record off its EPISODE, so deleting the
			// episode takes its action path with it. This is a MID-plan write — the
			// parent exists because BeginExperience opened it at plan start; if it
			// somehow does not, the adapter's subselect resolves it to NULL rather
			// than failing the write, which is the pre-ADR-0095 behaviour and not a
			// reason to lose the record.
			//
			// Gated on the store as well as the arm: with no ExperienceStore wired no
			// parent row is ever written, so stamping an id here would point every
			// action at an episode that cannot exist.
			if a.RecordOutcomes && a.ExperienceStore != nil {
				doc.ExperienceID = experienceIDForPlan(planID)
			}
		}
	}
	if err := a.Manager.Save(ctx, doc); err != nil {
		slog.Warn("MemoryAgent: failed to save action record", "doc_id", doc.ID, "err", err)
	}
	return nil
}

// nextDocID builds a process-unique document ID: a timestamp plus a monotonic
// sequence so rapid sequential records never collide under a coarse clock (ADR-0049).
func (a *Agent) nextDocID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), a.docSeq.Add(1))
}

// planIDFromTaskID extracts the planID embedded in a step TaskID ("step-{i}-{planID}").
func planIDFromTaskID(taskID string) string {
	parts := strings.SplitN(taskID, "-", 3)
	if len(parts) == 3 && parts[0] == "step" {
		return parts[2]
	}
	return ""
}

// experienceIDForPlan derives an episode parent id from a plan id (ADR-0095 D1).
//
// ONE derivation, used by every writer that parents a record: the begin-of-plan mint,
// the completion upsert, the mid-plan action records and the failure record. Two
// spellings of this would produce two episodes for one plan execution, and the FK is
// nullable — so the disagreement would show up as silently unparented rows, never as
// an error.
func experienceIDForPlan(planID string) string { return "exp-" + planID }

// outcomeRunning is the outcome an episode carries between its start and its
// completion. Distinct from "unknown" (the column default, meaning nobody ever said)
// and from a terminal success/failure: a row sitting here is a plan still in flight, or
// one whose process died before it could finish.
const outcomeRunning = "running"

// BeginExperience opens the EPISODE for a plan (ADR-0095 D1), before any of its records
// are written.
//
// The parent row has to exist FIRST. `chunks.experience_id` is written through a
// subselect so an absent parent resolves to NULL instead of failing the insert — which
// is the right call for the record (D5: never lose a memory over bookkeeping) and
// exactly why minting the parent late is invisible. Action records are written mid-plan;
// with the parent minted only at completion, every one of them resolved to NULL and the
// episode was never whole.
//
// Gated on RecordOutcomes so the default-off arm still writes nothing at all, and a nil
// store is a no-op for the same reason RecordPlanOutcome tolerates one.
func (a *Agent) BeginExperience(ctx context.Context, planID string) error {
	if planID == "" || !a.RecordOutcomes || a.ExperienceStore == nil {
		return nil
	}
	started := time.Now().UTC()
	// Remembered even if the save below fails: the completion upsert then INSERTs the
	// row for the first time, and it should still say when the work actually began.
	a.planStartedMu.Lock()
	if a.planStarted == nil {
		a.planStarted = map[string]time.Time{}
	}
	a.planStarted[planID] = started
	a.planStartedMu.Unlock()

	surface := domain.SurfaceFromContext(ctx)
	sessionID, _ := domain.SessionIDFromContext(ctx)
	exp := domain.Experience{
		ID:        experienceIDForPlan(planID),
		SessionID: string(sessionID),
		Surface:   surface.Kind,
		// ADR-0095 D4: born tagged, kernel-derived. Identical stamp to the completion
		// write — the two must agree, or the upsert would rewrite the classification of
		// a row that mid-plan records are already hanging from.
		Tags:      domain.BornTags(surface),
		Outcome:   outcomeRunning,
		StartedAt: started,
	}
	if err := a.ExperienceStore.SaveExperience(ctx, exp); err != nil {
		// Warn and continue. A plan that cannot open its episode still runs and still
		// records; its records keep a NULL parent, which is the pre-ADR-0095 baseline.
		slog.WarnContext(ctx, "MemoryAgent: episode not opened; this plan's records will have no parent",
			"plan_id", planID, "err", err)
	}
	return nil
}

// takePlanStart reads and clears the recorded start instant for a plan. A zero time
// means BeginExperience never ran for this plan id (an older deployment, or a replan
// that swapped the id mid-flight) — the adapter writes a zero as NULL rather than
// 0001-01-01, and its ON CONFLICT leaves started_at untouched, so a zero here never
// erases a start that was already recorded.
func (a *Agent) takePlanStart(planID string) time.Time {
	a.planStartedMu.Lock()
	defer a.planStartedMu.Unlock()
	t := a.planStarted[planID]
	delete(a.planStarted, planID)
	return t
}

// RecordPlanOutcome is the plan-completion hook (ADR-0049 §A2.2, ADR-0095). Once per
// completed plan it:
//
//  1. CLOSES the ADR-0095 episode — the same upsert BeginExperience opened it with, now
//     carrying the terminal outcome and completion time;
//  2. writes the A2.3 FAILURE RECORD (a negative edge parented on the episode) when the
//     failure was surprising or survived the whole recovery ladder;
//  3. feeds the outcome to the ADR-0094 procedures that informed the plan.
//
// It was WritePlanScene until 2026-11-05 and also wrote a `mnemonic_scene` chunk per
// plan. Scenes were cancelled (decisions/scenes-cancelled.md): the scene duplicated the
// episode row, was read at system scope across principals, and was skipped whenever a
// plan engaged no entity. That skip used to return before steps 2 and 3, so a
// pure-reasoning plan that failed left no failure record and never fed its procedures.
// Every step now runs for every plan, and the failure record's text is built from the
// PlanRecord (goal and outcome) instead of from the scene.
func (a *Agent) RecordPlanOutcome(ctx context.Context, rec domain.PlanRecord) error {
	planID, success, surprise := rec.PlanID, rec.Success, rec.Surprise
	if planID == "" || a.Manager == nil {
		return nil
	}
	// ADR-0049 §A2.2: the outcome record is an opt-in arm. Off ⇒ the plan leaves no
	// experiential trace at all.
	if !a.RecordOutcomes {
		return nil
	}
	goal := strings.TrimSpace(rec.Goal)
	outcome := outcomeOf(success)

	// ADR-0095 D1/D4: CLOSE the episode.
	//
	// The write also stands alone as the FALLBACK: it is an upsert on the same derived
	// id, so a deployment that never reached BeginExperience (a replan that swapped the
	// plan id mid-flight) still gets its parent here.
	//
	// A failure is logged and NOT fatal: the failure record is still written and simply
	// keeps a NULL parent. Losing the memory over bookkeeping about its parentage would
	// be the worse outcome (ADR-0095 D5).
	experienceID := experienceIDForPlan(planID)
	if a.ExperienceStore != nil {
		sessionID, _ := domain.SessionIDFromContext(ctx)
		surface := domain.SurfaceFromContext(ctx)
		exp := domain.Experience{
			ID:        experienceID,
			SessionID: string(sessionID),
			Surface:   surface.Kind,
			Tags:      domain.BornTags(surface),
			Outcome:   outcome,
			// Carried forward from the mint, so closing an episode never overwrites
			// when it started. Zero when BeginExperience did not run for this plan.
			StartedAt:   a.takePlanStart(planID),
			CompletedAt: time.Now().UTC(),
		}
		if err := a.ExperienceStore.SaveExperience(ctx, exp); err != nil {
			slog.WarnContext(ctx, "MemoryAgent: experience parent not recorded; failure record will have no parent",
				"plan_id", planID, "err", err)
			experienceID = ""
		}
	} else {
		experienceID = ""
	}

	// ADR-0049 A2.3 — the FAILURE RECORD. A failed episode that merit did NOT see coming
	// is the single most decision-relevant thing this system can remember, so it earns a
	// record in the negative-edge lane the WorkspaceStage already queries. A failure by
	// an agent already known to fail is unsurprising and writes nothing: that is the
	// difference between a surprise gate and a failure gate.
	//
	// A2.2 shapes it as conditions → attempt → failure mode: the goal is the conditions
	// and the attempt, the executor's rendered first error is the failure mode. The goal
	// is also the fallback failure mode when the executor had no error text to render.
	//
	// REPLAN EXHAUSTION is the one carve-out from the surprise gate: the system already
	// ran its whole recovery ladder and still failed, which is decision-relevant whether
	// or not merit saw it coming, and the surprise oracle returns "unknown" (-1) for an
	// agent with no merit history, so a NEW agent's failures could otherwise never clear
	// the floor. Deliberately independent of SurpriseFloor.
	if !success && (rec.ReplanExhausted || (a.SurpriseFloor > 0 && surprise >= a.SurpriseFloor)) {
		failureMode := rec.FailureSummary
		if failureMode == "" {
			failureMode = "unexpected plan failure: " + goal
		}
		// Parented, unlike the bare error-path negative edge: this one IS part of the
		// episode, so "forget this episode" (ADR-0137) takes it with the rest.
		if err := a.ingestNegativeEdgeForPlan(ctx, failureMode, planFailureConditions(goal, outcome, rec.ReplanExhausted), "", planID, experienceID); err != nil {
			slog.WarnContext(ctx, "MemoryAgent: failure record not written",
				"plan_id", planID, "surprise", surprise, "err", err)
		}
	}

	// ADR-0094 co-evolution: the stored routines that informed this plan learn from its
	// outcome. Procedures are still SERVED after induction was removed (2026-11-05), so
	// the routines already stored keep learning.
	if a.ProcedureDeprecateBelow > 0 && len(rec.FollowedProcedures) > 0 {
		FeedProcedureOutcome(ctx, a.Manager.Store, a.Manager.Embedder,
			rec.FollowedProcedures, success, a.ProcedureDeprecateBelow)
	}
	return nil
}

// planFailureConditions renders the "conditions → attempt" half of an A2.2 failure
// record from the PlanRecord: deterministic, so two occurrences of the same failure
// render the same way.
func planFailureConditions(goal, outcome string, replanExhausted bool) string {
	text := "plan goal: " + goal + " | outcome: " + outcome
	if replanExhausted {
		text += " | replan exhausted"
	}
	return text
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Mismatched lengths return 0 (no similarity) instead of panicking: under
// ADR-0107 projection coexistence the legacy `embedding` column can hold a
// different model's vector — or none — so a dimension mismatch is a reachable
// data condition on the read path, not a programming error.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
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
//
// UNPARENTED, deliberately. Its callers are failures that are not one plan execution —
// a caller reporting an agent fault — so there is no episode for them to belong to. A plan's OWN failure precedent goes through
// ingestNegativeEdgeForPlan instead.
func (a *Agent) IngestNegativeEdge(ctx context.Context, errorMsg, lastOutput, agentID string) error {
	return a.ingestNegativeEdgeForPlan(ctx, errorMsg, lastOutput, agentID, "", "")
}

// ingestNegativeEdgeForPlan writes the same negative-edge record, additionally parented
// on the episode that produced it (ADR-0095 D1) when one is known.
//
// A variant rather than a widened IngestNegativeEdge: that signature is the
// domain.MemoryAgent port, and episode parentage is an internal concern of the write
// path — the caller across the port has no business supplying one, and could not be
// trusted to if it did (ADR-0035: an agent never authors its own classification).
func (a *Agent) ingestNegativeEdgeForPlan(ctx context.Context, errorMsg, lastOutput, agentID, planID, experienceID string) error {
	text := fmt.Sprintf("FAILURE: %s\nLAST OUTPUT: %s", errorMsg, lastOutput)
	doc := &domain.Document{
		ID:                 negativeEdgeID(planID, agentID, text),
		DocumentType:       domain.DocTypeNegativeEdge,
		Text:               text,
		ActivationStrength: 0.1,
		ExperienceID:       experienceID,
		Metadata: map[string]interface{}{
			"timestamp":    time.Now().Format(time.RFC3339),
			"source_agent": agentID,
			"doc_type":     "negative_edge",
		},
	}
	if planID != "" {
		// The join key the action lane already uses. The parent alone is not enough:
		// experience_id is nullable and resolves to NULL for an absent parent, so the
		// metadata is what keeps the precedent attributable when the row is missing.
		doc.Metadata["plan_id"] = planID
	}
	return a.Manager.Save(ctx, doc)
}

// negativeEdgeID derives a failure record's id from its natural key — the plan it
// belongs to (empty for an unparented report), the reporting agent and the rendered
// record text — never from a clock (AGENTS.md §2.6). A retried RecordPlanOutcome or a
// re-delivered failure report therefore upserts the same row instead of writing a
// duplicate that the WorkspaceStage negatives lane would then read twice
// (T-20261113-memory-agent-mints-clock-derived-ids-for). Two DIFFERENT failures of one
// plan render different text and keep two rows. The "neg-edge-" prefix of the old
// clock-derived ids is kept; nothing parses the suffix, and rows already stored under
// the old ids are left as they are.
func negativeEdgeID(planID, agentID, text string) string {
	h := sha256.Sum256([]byte(planID + "\x00" + agentID + "\x00" + text))
	return "neg-edge-" + hex.EncodeToString(h[:16])
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

	return a.Manager.Save(ctx, doc)
}

// The ADR-0015 tier-2 memory judge (the Tier-1 pending channel, StartTier2Drain, the
// batched LLM scoring prompt and its heuristic fallback), its only feeders
// (RecordExecution, recordReadFact), Agent.Query (a scan of that channel only tests
// called) and the scene machinery around them (createSceneDoc, discussed_in and
// follows edges, scene dedup and reinforcement) were deleted on 2026-11-05
// (decisions/scenes-cancelled.md; investigations/2026-11-05-memory-judge-assessment.md).
// The judge guarded a capture path ADR-0049 A2.2 forbids and was never measured.

// outcomeOf renders a plan's success as the stored outcome vocabulary.
func outcomeOf(success bool) string {
	if success {
		return "success"
	}
	return "failure"
}
