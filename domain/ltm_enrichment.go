package domain

// LTMEnrichment carries the typed LTM content returned by WorkspaceStage.PrimeForPlanning.
// ADR-0025: replaces the flat map[string]string return to support distinct fact/negative sections.
type LTMEnrichment struct {
	Facts     []SearchResult // DocTypeMnemonicFact results
	Negatives []SearchResult // DocTypeNegativeEdge results
	// Episodes (ADR-0029: DocTypeEpisodicMemory hits above the "episodic" policy
	// threshold) is gone. Its writer was removed on 2026-07-18 and the reader — a
	// WorkspaceStage lane and the planner's <EpisodicMemory> block — was retired with it.
	Precedents []Precedent // ADR-0049 D11: world-model transitions for the situation being planned
	// Procedures are ADR-0094 induced routines for the situation being planned —
	// "how has this kind of work gone here?". ADVISORY (D6): planner input, never a
	// directive. The Gatekeeper still filters and the Dispatcher still selects, which
	// is why a routine names capabilities and never agents.
	Procedures []Procedure
}

// Precedent is a world-model TRANSITION (ADR-0049 D11): a past situation, what was DONE
// in it (the action path), and how it turned OUT. The planner/agent LLM reasons over
// these to anticipate which approach worked or failed under similar conditions — memory
// is the model, the LLM is the inference engine. Failure-weighted, similarity-gated.
type Precedent struct {
	SceneID    string   // the engaging scene's id (scene-{planID})
	Situation  string   // the abstracted situation (scene projection, else reconstruction text)
	Outcome    string   // "success" | "failure"
	Success    bool     // outcome as a boolean for failure-weighting
	Actions    []string // the action path taken in that situation (compact lines)
	Similarity float64  // cosine similarity of the precedent scene to the current situation
}

// PlanLTMEntry carries a prior successful ExecutionPlan and its review metadata
// for injection into the <PlanLTM> Planner prompt section. ADR-0025.
type PlanLTMEntry struct {
	PlanJSON   string  // serialised ExecutionPlan JSON
	Similarity float64 // cosine similarity to the current query
	Confidence float64 // mean per-step selection confidence from Hippocampus (an auction-era
	// field name: it was the mean BID confidence until ADR-0100 P3 removed bids)
	Outcome     string // plan_outcome from PlanEvent (e.g. "success", "partial")
	ReplanCount int    // replan_count from PlanEvent
}
