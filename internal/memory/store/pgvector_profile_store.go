package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ProfileStore combines domain.ProfileStore and domain.JudicialStore.
// It is the interface expected by MetabolismStack, SupervisionStack, and MemoryStack.
type ProfileStore interface {
	domain.ProfileStore
	domain.JudicialStore
}

// PgVectorProfileStore implements domain.ProfileStore and domain.JudicialStore
// by persisting AgentProfiles and Judicial Records as documents in a VectorStore.
type PgVectorProfileStore struct {
	store domain.VectorStore
}

// NewProfileStore returns a PgVectorProfileStore backed by the given VectorStore.
func NewProfileStore(store domain.VectorStore) *PgVectorProfileStore {
	return &PgVectorProfileStore{store: store}
}

// SaveProfile serialises the AgentProfile to JSON, builds a Document with
// DocumentType = domain.DocTypeAgentProfile, and persists it via VectorStore.Save.
func (p *PgVectorProfileStore) SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error {
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("pgVectorProfileStore: marshal profile for agent %s: %w", agentID, err)
	}

	doc := &domain.Document{
		ID:           fmt.Sprintf("profile:%s:%s", agentID, sourceHash),
		DocumentType: domain.DocTypeAgentProfile,
		Text:         string(profileJSON),
		Embedding: domain.Embedding{
			Vector: embedding,
		},
		Metadata: map[string]interface{}{
			"agent_id":    agentID,
			"source_hash": sourceHash,
		},
	}

	if err := p.store.Save(ctx, doc); err != nil {
		return fmt.Errorf("pgVectorProfileStore: save profile for agent %s: %w", agentID, err)
	}
	return nil
}

// Save implements domain.JudicialStore. It persists a verifier critique
// as a document with DocumentType = domain.DocTypeJudicialRecord.
func (p *PgVectorProfileStore) Save(ctx context.Context, text string, embedding []float32, metadata map[string]interface{}) error {
	doc := &domain.Document{
		ID:           fmt.Sprintf("judicial:%d", time.Now().UnixNano()),
		DocumentType: domain.DocTypeJudicialRecord,
		Text:         text,
		Embedding: domain.Embedding{
			Vector: embedding,
		},
		Metadata: metadata,
	}
	return p.store.Save(ctx, doc)
}

// GetProfile retrieves the AgentProfile stored for (agentID, sourceHash).
// Returns nil, nil when no profile exists.
func (p *PgVectorProfileStore) GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	id := fmt.Sprintf("profile:%s:%s", agentID, sourceHash)
	doc, err := p.store.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: get profile for agent %s: %w", agentID, err)
	}
	if doc == nil {
		return nil, nil
	}

	var profile domain.AgentProfile
	if err := json.Unmarshal([]byte(doc.Text), &profile); err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: unmarshal profile for agent %s: %w", agentID, err)
	}
	return &profile, nil
}

// GetJudicialRecords returns the top-K critique texts stored as judicial_record
// documents for (agentID, sourceHash). Returns nil when no records exist.
func (p *PgVectorProfileStore) GetJudicialRecords(ctx context.Context, agentID, sourceHash string, topK int) ([]string, error) {
	results, err := p.store.Search(ctx, nil, domain.SearchOptions{
		DocumentType: domain.DocTypeJudicialRecord,
		TopK:         topK,
		Filter:       fmt.Sprintf("metadata->>'agent_id' = '%s' AND metadata->>'source_hash' = '%s'", agentID, sourceHash),
		Scope:        domain.ScopeSystem, // ADR-0034: judicial-record retrieval is a kernel read
	})
	if err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: get judicial records for agent %s: %w", agentID, err)
	}

	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Document.Text)
	}
	return out, nil
}

// EmbeddingDistance returns a normalised cosine distance in [0, 1] between two
// vectors. A distance of 0 means identical; 1 means maximally distant.
func (p *PgVectorProfileStore) EmbeddingDistance(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 1.0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 1.0
	}

	cosineSim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if cosineSim > 1.0 {
		cosineSim = 1.0
	}
	if cosineSim < -1.0 {
		cosineSim = -1.0
	}
	return 1.0 - cosineSim
}
