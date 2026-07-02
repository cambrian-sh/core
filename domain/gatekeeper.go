package domain

import "context"

// AgentSearchResult is a candidate returned by an interview vector search.
type AgentSearchResult struct {
	AgentID    string
	SourceHash string
	Similarity float64
}

// InterviewSearcher searches agent profiles by embedding similarity.
type InterviewSearcher interface {
	SearchByEmbedding(ctx context.Context, embedding []float32, threshold float64, topK int) ([]AgentSearchResult, error)
}

// ScoredCandidate pairs an agent definition with its Gatekeeper score.
type ScoredCandidate struct {
	Agent AgentDefinition
	Score float64
}

// Gatekeeper is the three-layer interrupt controller (Declaration → Interview → Merit)
// that filters the full agent list to a short candidate slate before the Auction.
type Gatekeeper interface {
	FindCandidates(ctx context.Context, task *AuctionTask) ([]ScoredCandidate, error)
	// FindModelCandidates returns all TraitModel agents filtered by required capabilities
	// and ranked by merit score. Used by the Auctioneer for ADR-0018 TraitModel sub-selection.
	// An empty requiredCapabilities list means no capability floor — all TraitModel agents pass.
	FindModelCandidates(ctx context.Context, requiredCapabilities []string) ([]ScoredCandidate, error)
}
