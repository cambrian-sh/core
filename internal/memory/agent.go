package memory

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Agent orchestrates memory curation by evaluating, deduplicating, and masking sensitive data.
type Agent struct {
	Manager              *MemoryManager
	MaxNeighborExpansion int

	// RecordOutcomes gates the plan-outcome hook (RecordPlanOutcome: the ADR-0095
	// episode open/close and procedure feedback). The mutation action events and the
	// A2.3 failure record it also gated were stopped on 2026-11-14 (owner decision,
	// ENG-1595: no raw Manager.Save of step results or tool outputs).
	// The RAW path it once sat beside — auto-embedded step results and tool payloads
	// behind `experiential_memory_enabled`, curated by the ADR-0015 tier-2 judge — was
	// deleted on 2026-11-05 (decisions/scenes-cancelled.md). Set from
	// execution.experience_records_enabled, whose config default is TRUE (owner
	// decision 2026-07-28, internal/config/config.go); the field's Go zero value is
	// false, so an Agent built without config records nothing.
	RecordOutcomes bool

	// ExperienceStore persists the episode row of a plan (ADR-0095 D1). May be nil →
	// no episode is recorded.
	ExperienceStore domain.ExperienceStore

	// ProcedureDeprecateBelow is the confidence under which a repeatedly-failing routine
	// is retired (ADR-0094 D4). 0 disables procedure feedback entirely.
	ProcedureDeprecateBelow float64

	// ContentStore holds an ingested source document's body (IngestionManager,
	// ADR-0028). May be nil → the body is not offloaded.
	ContentStore domain.ContentStore

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
//
// The relevance threshold and result cap that used to be passed here served only
// FetchContext, deleted with ENG-1339 (signal enrichment reads through
// QueryService.RecallFor); execution.memory.memory_relevance_threshold and
// max_memory_results now reach the watcher (supwatcher.WatcherConfig).
func NewAgent(manager *MemoryManager, maxNeighborExpansion int) *Agent {
	return &Agent{
		Manager:              manager,
		MaxNeighborExpansion: maxNeighborExpansion,
		planStarted:          map[string]time.Time{},
	}
}

// RecordToolOutput writes NOTHING (owner decision 2026-11-14, ENG-1595: "No bypass
// ... just saving the step result, tool outputs etc is completely shit"). It used to
// embed a mutation's action line and Manager.Save it as a `mnemonic_action` row,
// straight past IngestionManager. Anything remembered from agent work will go through
// the one memory pipeline once the owner-led redesign of agent-experience memory sets
// what is worth keeping; until then the hook stays on the domain.MemoryAgent port so
// the tool path needs no change when that lands. Rows written before 2026-11-14 are
// removed by the owner-run `cmd/agent-memory-purge` (dry-run first).
func (a *Agent) RecordToolOutput(_ context.Context, _ domain.ToolOutputRecord) error {
	return nil
}

// experienceIDForPlan derives an episode parent id from a plan id (ADR-0095 D1).
//
// ONE derivation, used by every writer of the episode row: the begin-of-plan mint and
// the completion upsert (the mid-plan action records and the failure record that
// also hung from it were stopped on 2026-11-14, ENG-1595). Two
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
//  2. feeds the outcome to the ADR-0094 procedures that informed the plan.
//
// It no longer writes the A2.3 FAILURE RECORD (a negative_edge chunk): owner decision
// 2026-11-14, ENG-1595.
//
// It was WritePlanScene until 2026-11-05 and also wrote a `mnemonic_scene` chunk per
// plan. Scenes were cancelled (decisions/scenes-cancelled.md): the scene duplicated the
// episode row, was read at system scope across principals, and was skipped whenever a
// plan engaged no entity.
func (a *Agent) RecordPlanOutcome(ctx context.Context, rec domain.PlanRecord) error {
	planID, success := rec.PlanID, rec.Success
	if planID == "" || a.Manager == nil {
		return nil
	}
	// ADR-0049 §A2.2: the outcome record is an opt-in arm. Off ⇒ the plan leaves no
	// experiential trace at all.
	if !a.RecordOutcomes {
		return nil
	}
	outcome := outcomeOf(success)

	// ADR-0095 D1/D4: CLOSE the episode.
	//
	// The write also stands alone as the FALLBACK: it is an upsert on the same derived
	// id, so a deployment that never reached BeginExperience (a replan that swapped the
	// plan id mid-flight) still gets its parent here.
	//
	// A failure is logged and NOT fatal: procedure feedback below still runs.
	if a.ExperienceStore != nil {
		sessionID, _ := domain.SessionIDFromContext(ctx)
		surface := domain.SurfaceFromContext(ctx)
		exp := domain.Experience{
			ID:        experienceIDForPlan(planID),
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
			slog.WarnContext(ctx, "MemoryAgent: episode not closed", "plan_id", planID, "err", err)
		}
	}

	// The ADR-0049 A2.3 FAILURE RECORD (a negative_edge chunk carrying the executor's
	// failure text, Manager.Save'd past the ingestion pipeline) is no longer written:
	// owner decision 2026-11-14, ENG-1595 — no raw saves of step results. The episode
	// row above still records that the plan failed.

	// ADR-0094 co-evolution: the stored routines that informed this plan learn from its
	// outcome. Procedures are still SERVED after induction was removed (2026-11-05), so
	// the routines already stored keep learning.
	if a.ProcedureDeprecateBelow > 0 && len(rec.FollowedProcedures) > 0 {
		FeedProcedureOutcome(ctx, a.Manager.Store, a.Manager.Embedder,
			rec.FollowedProcedures, success, a.ProcedureDeprecateBelow)
	}
	return nil
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
