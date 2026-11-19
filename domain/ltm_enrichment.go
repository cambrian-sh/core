package domain

// LTMEnrichment carries the typed LTM content returned by WorkspaceStage.PrimeForPlanning.
// ADR-0025: replaces the flat map[string]string return to support distinct sections.
type LTMEnrichment struct {
	Facts []SearchResult // DocTypeMnemonicFact results
	// Negatives (DocTypeNegativeEdge failure records) were removed with ENG-1645:
	// their writer, the ADR-0049 A2.3 failure record, stopped with ENG-1595.
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
