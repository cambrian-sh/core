package app

import "fmt"

// factLayerBoot is what the ADR-0136 fact-layer boot line states. Every field is the
// EFFECTIVE value: a switch that cannot act is reported false (see the caller).
type factLayerBoot struct {
	Extract, Lane, Supersession, Assessments bool
	Store                                    string
	ScopedDocuments, ScopedTopK              int
	// ExtractorVersion is "<prompt id>/<generator model>" (FactExtractor.ExtractorVersion,
	// A2.4), or "none" when extraction is off: the version every marker this boot writes.
	ExtractorVersion                                       string
	MaxCallsPerDocument, MaxFactsPerSource, MaxPromptChars int
}

// factLayerBootLine renders the fact layer's boot line, the expect_log anchor of the
// benchmark fact arms (armrun refuses an arm whose line does not match).
//
// Fields are only ever APPENDED: experiment specs anchor on the prefix
// ("fact layer: extract_enabled=… store=substrate"), so inserting a field would break
// every recorded arm. The A2 tail (extractor version and the three budgets) lets an arm
// prove it ran the fact-extract-v2 prompt with the backbone and budget it claims
// (T-20261103-the-fact-layer-boot-line-does-not-log-the, ENG-490).
func factLayerBootLine(b factLayerBoot) string {
	version := b.ExtractorVersion
	if version == "" {
		version = "none"
	}
	return fmt.Sprintf("fact layer: extract_enabled=%t lane_enabled=%t supersession_enabled=%t assessments_enabled=%t store=%s lane_scoped_documents=%d lane_scoped_top_k=%d extractor_version=%s max_calls_per_document=%d max_facts_per_source=%d max_prompt_chars=%d",
		b.Extract, b.Lane, b.Supersession, b.Assessments, b.Store, b.ScopedDocuments, b.ScopedTopK,
		version, b.MaxCallsPerDocument, b.MaxFactsPerSource, b.MaxPromptChars)
}
