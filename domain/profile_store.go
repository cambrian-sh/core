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
