package domain

import "context"

// Generator produces text from an LLM given a prompt.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// StepResult carries a completed step's output and its pre-execution context snapshot.
// Used by MemoryRecorder (ADR-0015) to feed the Tier-1 pending channel.
type StepResult struct {
	Index     int
	Output    string            // step output payload text
	Snapshot  map[string]string // masterContext clone at step dispatch time
	SceneID   string            // ADR-0025: ID of the MnemonicScene written for this step; "" if none
	SessionID SessionID         // ADR-0029: session scope for Tier-2 session_id metadata tag; "" = unscoped
	TaskID    string            // ADR-0049 D3: per-step correlation key (step-{index}-{planID}); "" disables dedup
	// DependsOnTaskIDs are the TaskIDs of this step's dependency steps (ADR-0049 D10),
	// so RecordExecution can write `follows` edges from this step's record to theirs.
	DependsOnTaskIDs []string
}

// MemoryRecorder receives completed step results for async LTM ingestion.
// ADR-0015: step results flow through the Tier-1 bounded channel before Tier-2 batched pgvector commit.
type MemoryRecorder interface {
	RecordExecution(ctx context.Context, result StepResult) error
	// WritePlanScene materializes the ONE immutable scene for a completed plan
	// (ADR-0049 D5/D7) — id `scene-{planID}`, holding the goal + engaged-entity scope
	// (accreted from the plan's actions) + the outcome. Written for BOTH success and
	// failure (a failure scene is the highest-value precedent). Replaces per-step scenes.
	WritePlanScene(ctx context.Context, rec PlanRecord) error
}

// WorkspaceStage enriches the Planner and DAGExecutor with cross-session LTM facts.
// ADR-0016: bounded additive enrichment layer; nil = no enrichment (existing behaviour).
type WorkspaceStage interface {
	// PrimeForPlanning returns typed LTM enrichment (facts + negatives) for Planner injection.
	// ADR-0025: return type changed from map[string]string to LTMEnrichment.
	PrimeForPlanning(ctx context.Context, taskQuery string) (LTMEnrichment, error)
	PrimeForExecution(ctx context.Context, plan *ExecutionPlan, initialContext map[string]string) (map[string]string, error)
	// PrimeForStep selects a capacity-limited working set for a single step dispatch.
	// ADR-0022 Phase 2: uses spreading activation + precision to rank LTM content.
	// priorStepRefs are CIDs for steps in DependsOn — they receive an activation boost.
	// planningFacts are pre-validated facts from the Planner (AGENTCONTEXTREQ REQ1-3);
	// they are filtered by per-step cosine similarity and merged ahead of speculative BFS nodes.
	// maxItems is the hard ceiling (config.MaxContextSlots).
	// stepFactCosineThreshold is the per-step relevance floor (default 0.55).
	// Returns refs sorted by activation descending; BFS-discovered refs carry Precision=-1.0.
	// May return (nil, nil) when the graph is empty and pgvector returns no seeds.
	PrimeForStep(ctx context.Context, query string, priorStepRefs []ContextRef, planningFacts []SearchResult, stepFactCosineThreshold float64, maxItems int) ([]ContextRef, error)
}

// PlanRecord is everything the memory layer needs about a completed plan (ADR-0049
// A2.2). A struct rather than a parameter list because this has already grown once —
// goal, then surprise, now the capability shape — and each widening churned every call
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
	// Capabilities is the ordered capability sequence the plan's steps required — the
	// SHAPE of the routine, which is what ADR-0094 D3 clusters on. Empty when the
	// capability_contract arm is off, in which case the episode is not inducible: the
	// inducer skips it rather than grouping on situation alone, which over-groups.
	Capabilities []string
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
}
