package domain

import "time"

// Decision provenance seam (ADR-0103 D3).
//
// This is the ONLY thing the OSS kernel knows about evidence receipts, and it
// deliberately never uses that word. A downstream package subscribes to completed
// retrievals and does whatever it likes with them — sign them, chain them, store
// them, ignore them. With nothing registered the call site is a nil check and
// kernel behaviour is bit-identical, which is what keeps the open-core boundary
// (ADR-0057 invariant #1) intact: the core names only concepts it already owns.
//
// The contract is one-way and lossy by design. An observer receives a value copy
// after assembly is finished and cannot influence what was retrieved, cannot
// return an error, and cannot make the caller wait. Retrieval reads fail open
// (memory invariant #5); a provenance lane that could stall or fail a query would
// invert the kernel's most load-bearing availability property in exchange for a
// durability one.

// RetrievedChunk is one row as it left the retrieval pipeline, identified rather
// than copied: ADR-0103 D1 keeps chunk TEXT out of the record so that deleting a
// chunk later (subject erasure) does not force a choice between honouring the
// deletion and preserving a verifiable history.
type RetrievedChunk struct {
	// ChunkID is Document.ID — the row actually returned.
	ChunkID string
	// DocumentType is the lane the row came from (DocTypeMnemonicFact, …).
	DocumentType string
	// SectionPath is the ADR-0060 structural breadcrumb when the row carries one
	// ("Ops Review > 3.2 Incidents"); empty for flat documents.
	SectionPath string
	// Score is the final post-blend score the row was ranked on.
	Score float64
	// RawScore is the pre-multiplier cosine similarity.
	RawScore float64
	// LexicalScore is the RRF lexical signal in [0,1] (ADR-0054).
	LexicalScore float64
	// Primary distinguishes a genuine dense/lexical hit from an associatively
	// injected one (entity seed, kgExpand). This is the existing non-displacing
	// distinction, surfaced rather than discarded.
	//
	// It is NOT full stage attribution: it does not say WHICH stage admitted an
	// injected row. Recording that means threading provenance through the two-pass
	// truncation whose invariant exists because graph injection once nearly halved
	// MuSiQue support-recall, so it is deferred to its own benchmarked change
	// (ADR-0103 D8).
	Primary bool
}

// RetrievalDecision is the value-copy record of one completed retrieval.
type RetrievalDecision struct {
	// QueryID identifies this retrieval. Unique per call, including per hop of an
	// agentic loop — each hop is a separate decision with its own evidence.
	QueryID string
	// SessionID is the session the retrieval ran under; empty when there is none.
	SessionID string
	// PrincipalID is the caller the read was authorized as.
	PrincipalID string
	// QueryTextHash is a hash of the query, never the query itself: the text can
	// carry exactly the sensitive content a receipt exists to avoid duplicating,
	// while a hash still proves two receipts answered the same question.
	QueryTextHash string
	// DocType is the lane searched.
	DocType string
	// TopK is the requested window size, so an observer can distinguish "only two
	// results existed" from "two results were asked for".
	TopK int
	// Retrieved is the assembled result set, in returned order.
	Retrieved []RetrievedChunk
	// ConfigFingerprint identifies the retrieval configuration that produced this
	// ranking (ADR-0103 D7). Without it a record of what was retrieved cannot
	// answer why it was ranked that way, and cannot be reproduced.
	ConfigFingerprint string
	// At is the emission time.
	At time.Time
}

// DecisionObserver receives completed retrievals.
//
// Implementations MUST return promptly and MUST NOT panic. The kernel calls this
// synchronously on the retrieval path: the expected implementation is a non-blocking
// hand-off to a buffered channel that drops and counts when full, so a slow or
// wedged consumer degrades the provenance lane rather than retrieval itself.
type DecisionObserver interface {
	ObserveRetrieval(d RetrievalDecision)
}

// MultiDecisionObserver fans one retrieval out to several observers. The kernel
// composes registered plugins into one of these so the call site stays a single
// nil check.
type MultiDecisionObserver []DecisionObserver

// ObserveRetrieval forwards to each observer in registration order.
func (m MultiDecisionObserver) ObserveRetrieval(d RetrievalDecision) {
	for _, o := range m {
		if o != nil {
			o.ObserveRetrieval(d)
		}
	}
}
