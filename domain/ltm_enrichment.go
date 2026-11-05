package domain

// LTMEnrichment carries the typed LTM content returned by WorkspaceStage.PrimeForPlanning.
// ADR-0025: replaces the flat map[string]string return to support distinct fact/negative sections.
type LTMEnrichment struct {
	Facts     []SearchResult // DocTypeMnemonicFact results
	Negatives []SearchResult // DocTypeNegativeEdge results
	// Episodes (ADR-0029: DocTypeEpisodicMemory hits above the "episodic" policy
	// threshold — a policy itself retired 2026-10-31) is gone. Its writer was removed on 2026-07-18 and the reader — a
	// WorkspaceStage lane and the planner's <EpisodicMemory> block — was retired with it.
	// Precedents (ADR-0049 D11, transitions read from `mnemonic_scene` rows) were
	// removed 2026-11-05 with scenes (decisions/scenes-cancelled.md).
	// Procedures are ADR-0094 induced routines for the situation being planned —
	// "how has this kind of work gone here?". ADVISORY (D6): planner input, never a
	// directive. The Gatekeeper still filters and the Dispatcher still selects, which
	// is why a routine names capabilities and never agents.
	Procedures []Procedure
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
