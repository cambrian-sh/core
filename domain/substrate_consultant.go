package domain

import "context"

// SubstrateCitationsID is the reserved id of the synthetic result row carrying
// substrate citations (ADR-0118 D5). It is out-of-band metadata, not a ranked
// hit: it is appended after top-k truncation in the retrieval path, and wire
// handlers must exempt it from any caller-window truncation of their own —
// deleting the citation an answer arrived with would silently unwarranty it.
const SubstrateCitationsID = "_substrate_citations"

// SubstrateCitation is one modelled answer the substrate stands behind for a
// retrieval query: the guarantee label, the typed rows, and the evidence ids
// that receipt them. It rides retrieval results as metadata on a synthetic,
// non-displacing row (ADR-0118 D5) — typed rows never enter the ranked list,
// so no lane can claim the other's guarantees.
type SubstrateCitation struct {
	// Entity is the substrate entity the citation is about (the anchor the
	// consultant resolved from the query).
	Entity string
	// Predicate is the modelled predicate answered, when the shape has one.
	Predicate string
	// Guarantee is the §14 label the query plane attached — carried verbatim,
	// because a row set without its warranty is just another ranked list.
	Guarantee string
	// Rows are the typed answer rows, bounded by the consultant.
	Rows []QueryRow
	// EvidenceIDs receipt the rows (ADR-0105 provenance linkage).
	EvidenceIDs []string
}

// SubstrateConsultant answers the MODELLED part of a retrieval query exactly,
// as the calling agent's principal, through the scoped substrate seam
// (ADR-0118 D5). nil in OSS builds — the retrieval call site is a nil check
// and behaviour is bit-identical (the DecisionObserver idiom).
//
// Contract for implementations:
//   - Consult MUST go through the scope-enforced seam as callerID's principal;
//     it can never surface what the agent could not read directly.
//   - An unmodelled query returns (nil, nil) — that is an answer, not an error.
//   - Failures and refusals are returned as errors; the caller fails open to
//     the ordinary retrieval answer (invariant 5).
//   - Answers are grounded in stored rows by construction (unwritten rule 1);
//     no parametric knowledge, no LLM at this seam in v1.
type SubstrateConsultant interface {
	Consult(ctx context.Context, callerID, query string) ([]SubstrateCitation, error)
}
