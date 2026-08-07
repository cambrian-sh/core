package domain

import "context"

// ProceduralMemory stores and retrieves ExecutionPlan templates (Hippocampus).
type ProceduralMemory interface {
	Store(ctx context.Context, plan *ExecutionPlan, meanConfidence float64) error
	// Retrieve returns (plan, similarity, confidence, error).
	// Similarity is the raw cosine score from vector search.
	// Confidence is the stored mean auction confidence.
	// Delegates to RetrieveWithPolicy with the default policy.
	Retrieve(ctx context.Context, userInput string) (*ExecutionPlan, float64, float64, error)
	// RetrieveWithPolicy uses the named policy's SimilarityThreshold, ConfidenceFloor,
	// and MaxAgeHours. Unknown policy names fall back to the default policy. (ADR-0027)
	RetrieveWithPolicy(ctx context.Context, userInput string, policyName string) (*ExecutionPlan, float64, float64, error)
}

// MemoryFetcher returns a memory context string for injection into prompts.
type MemoryFetcher interface {
	FetchContext(ctx context.Context, userInput string) string
}

// MemoryAgent is the full memory curation interface used by components that need
// both read and write access to the episodic memory layer.
// ADR-0025: MemoryFetcher (FetchContext) removed — no longer part of the planning path.
// The Watcher retains its own local MemoryContextProvider interface for signal enrichment.
//
// The MemoryIngester seam (IngestSync / ProcessAndStoreAsync) was REMOVED: it was the
// per-item LLM-importance write path, superseded by the ADR-0015 Tier-1/Tier-2 drain
// (MemoryRecorder.RecordExecution -> bounded channel -> batch score -> commit). Its last
// two callers had already moved off it — the step-result Memory Barrier (ADR-0049 D3) and
// the premium drift writer, which needs the classification-carrying ingest instead.
type MemoryAgent interface {
	IngestNegativeEdge(ctx context.Context, errorMsg, lastOutput, agentID string) error
	PoisonMemory(ctx context.Context, memoryID string, correction string) error
	MemoryRecorder
}
