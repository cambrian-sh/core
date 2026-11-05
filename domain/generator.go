package domain

import "context"

// Generator produces text from an LLM given a prompt.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// MemoryRecorder receives a plan's lifecycle for the experiential record (ADR-0049
// §A2.2, ADR-0095): the episode is opened at plan start and closed at completion.
//
// It used to receive every step result too (RecordExecution, StepResult), feeding the
// ADR-0015 Tier-1 channel and tier-2 memory judge. That raw capture path, and the
// per-step SceneID it carried, were deleted on 2026-11-05 with scenes
// (cambrian-knowledge/decisions/scenes-cancelled.md).
type MemoryRecorder interface {
	// BeginExperience mints the EPISODE PARENT for a plan at its START (ADR-0095 D1).
	//
	// The id is derived from the plan id precisely "so records written mid-plan can
	// reference a parent that already exists". Minting it only at completion made that
	// sentence false: action records are written WHILE the plan runs, and the adapter
	// resolves a missing parent to NULL rather than failing, so every mid-plan record
	// silently landed unparented — the episode could not be listed, deleted or governed
	// as one thing, which is the whole point of the row.
	//
	// Never fatal: an episode that cannot be opened is logged and the plan proceeds
	// unparented (ADR-0095 D5 — a write must never fail over bookkeeping).
	BeginExperience(ctx context.Context, planID string) error
	// RecordPlanOutcome is the plan-completion hook, called for BOTH success and
	// failure. It closes the ADR-0095 episode, writes the ADR-0049 A2.3 failure record
	// when the failure was surprising or exhausted the replan ladder, and feeds the
	// outcome to the ADR-0094 procedures that informed the plan. It was WritePlanScene
	// until 2026-11-05, when the plan scene it also wrote was removed.
	RecordPlanOutcome(ctx context.Context, rec PlanRecord) error
}

// WorkspaceStage enriches the Planner and DAGExecutor with cross-session LTM facts.
// ADR-0016: bounded additive enrichment layer; nil = no enrichment (existing behaviour).
type WorkspaceStage interface {
	// PrimeForPlanning returns typed LTM enrichment (facts + negatives) for Planner injection.
	// ADR-0025: return type changed from map[string]string to LTMEnrichment.
	PrimeForPlanning(ctx context.Context, taskQuery string) (LTMEnrichment, error)
	PrimeForExecution(ctx context.Context, plan *ExecutionPlan, initialContext map[string]string) (map[string]string, error)
}

// PlanRecord is everything the memory layer needs about a completed plan (ADR-0049
// A2.2). A struct rather than a parameter list because this has already grown once —
// goal, then surprise, then the capability shape — and each widening churned every call
// site and test fake. Fields are added here without touching any of them.
type PlanRecord struct {
	PlanID  string
	Goal    string
	Success bool
	// Surprise is the A2.3 prediction error: the LARGEST |expected - actual| across the
	// plan's steps, or -1 when no step had a merit history to predict from. The maximum,
	// not the mean, because one badly-mispredicted step is what makes an episode worth
	// remembering; averaging dilutes it away. -1 is deliberately distinct from 0.0.
	Surprise float64
	// Capabilities (the plan's capability sequence, ADR-0094 D3's clustering key) was
	// removed 2026-11-05 with procedure induction, its only reader.
	// FollowedProcedures are the ADR-0094 routines that were in the planner's context
	// when this plan was built. They close the co-evolution loop: a routine that shaped
	// a plan learns from how that plan turned out.
	//
	// "In context" rather than "provably obeyed" is deliberate. A procedure is ADVISORY
	// (D6) — the planner may adapt or ignore it — so demanding proof of compliance
	// before feeding anything back would leave the loop permanently open. Attributing
	// the outcome to what informed the plan is the honest approximation, and the slow
	// consolidation rate in ApplyOutcome is what keeps that approximation safe.
	FollowedProcedures []string
	// FailedStep is the index of the step that produced the plan's FIRST error.
	// Meaningful only when Success is false: the executor's counter is zero-initialized
	// and reset to 0 on replan, so a successful plan carries 0, not a sentinel. Read it
	// together with Success or a clean plan reads as a step-0 failure.
	FailedStep int
	// FailureSummary is the FAILURE MODE half of A2.2's "conditions → attempt → failure
	// mode": a bounded deterministic rendering of the first error, with the failed
	// step's query when the executor could reach it. Empty on success and whenever no
	// error text was available.
	//
	// Deterministic and never LLM-paraphrased on purpose — a precedent is only useful if
	// two occurrences of the same failure render the same way.
	FailureSummary string
	// ReplanExhausted marks a failure that survived the FULL recovery ladder: the
	// executor replanned as many times as it was allowed and the plan still failed.
	//
	// Carried because it is the one failure shape that is decision-relevant whether or
	// not merit predicted it, and because the A2.3 surprise gate is structurally blind to
	// exactly the agents that need it most: the oracle returns "unknown" for an agent
	// with no merit history, so a NEW agent's failures can never clear the floor and the
	// earliest, most instructive failures leave no precedent at all.
	ReplanExhausted bool
}
