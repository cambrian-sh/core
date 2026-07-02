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

// MemoryIngester synchronously evaluates and stores a memory fragment.
type MemoryIngester interface {
	IngestSync(ctx context.Context, text string, sourceAgent string) error
}

// MemoryAgent is the full memory curation interface used by components that need
// both read and write access to the episodic memory layer.
// ADR-0025: MemoryFetcher (FetchContext) removed — no longer part of the planning path.
// The Watcher retains its own local MemoryContextProvider interface for signal enrichment.
type MemoryAgent interface {
	MemoryIngester
	ProcessAndStoreAsync(ctx context.Context, text string, sourceAgent string)
	IngestNegativeEdge(ctx context.Context, errorMsg, lastOutput, agentID string) error
	PoisonMemory(ctx context.Context, memoryID string, correction string) error
	MemoryRecorder
}
