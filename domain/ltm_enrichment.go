package domain

// LTMEnrichment carries the typed LTM content returned by WorkspaceStage.PrimeForPlanning.
// ADR-0025: replaces the flat map[string]string return to support distinct fact/negative sections.
type LTMEnrichment struct {
	Facts      []SearchResult // DocTypeMnemonicFact results
	Negatives  []SearchResult // DocTypeNegativeEdge results
	Episodes   []SearchResult // DocTypeEpisodicMemory results above "episodic" policy threshold (ADR-0029)
	Precedents []Precedent    // ADR-0049 D11: world-model transitions for the situation being planned
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
	PlanJSON    string  // serialised ExecutionPlan JSON
	Similarity  float64 // cosine similarity to the current query
	Confidence  float64 // mean auction confidence from Hippocampus
	Outcome     string  // plan_outcome from PlanEvent (e.g. "success", "partial")
	ReplanCount int     // replan_count from PlanEvent
}
