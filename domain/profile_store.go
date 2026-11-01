package domain

import "context"

// ProfileStore persists and retrieves AgentProfiles and Judicial Records in the
// vector store. It is the cross-stack interface implemented by the supervision
// infrastructure adapter.
type ProfileStore interface {
	SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile AgentProfile) error
	GetProfile(ctx context.Context, agentID, sourceHash string) (*AgentProfile, error)
	GetJudicialRecords(ctx context.Context, agentID, sourceHash string, topK int) ([]string, error)
	EmbeddingDistance(a, b []float32) float64
}

// ProfileHistory reads an agent's profiles across versions (ADR-0002 D2 /
// ADR-0006 D5). A profile is keyed by (agentID, sourceHash), so on a version
// change GetProfile with the NEW hash finds nothing; inherited merit needs the
// PREVIOUS version's profile, which only an agent-scoped listing can find
// (T-20261101-interview-reinterview-never-finds-the-previous).
//
// A separate port rather than a fifth ProfileStore method so the many
// ProfileStore consumers that never read history are not forced to fake it;
// stores that serve the interview worker compose both (AGENTS.md §3.1).
type ProfileHistory interface {
	// GetPreviousProfile returns the most recently written profile for agentID
	// whose source hash differs from currentSourceHash, or nil, nil when the
	// agent has no other version.
	GetPreviousProfile(ctx context.Context, agentID, currentSourceHash string) (*AgentProfile, error)
}
