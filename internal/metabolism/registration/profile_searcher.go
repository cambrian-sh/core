package registration

import (
	"context"
	"sort"

	"github.com/cambrian-sh/core/domain"
)

// pgvectorProfileSearcher wraps a domain.VectorStore and implements
// domain.ProfileSearcher. It filters results to agent_profile documents
// and applies the caller-supplied similarity threshold.
type pgvectorProfileSearcher struct {
	store domain.VectorStore
}

// NewProfileSearcher returns a domain.ProfileSearcher backed by the given
// VectorStore.
func NewProfileSearcher(store domain.VectorStore) domain.ProfileSearcher {
	return &pgvectorProfileSearcher{store: store}
}

func (s *pgvectorProfileSearcher) SearchByEmbedding(ctx context.Context, embedding []float32, threshold float64, topK int) ([]domain.AgentSearchResult, error) {
	// ADR-0023 Routing Fix:
	// 1. Pass DocumentType so pgvector filters at the SQL level. Without this,
	//    LTM documents (facts, actions) drown out agent profiles in the top-K.
	// 2. Pass RetrievalFloor=1.0 so the floor-multiplier re-ranking becomes a
	//    no-op for agent-profile lookups (agent profiles have no ActivationStrength).
	raw, err := s.store.Search(ctx, embedding, domain.SearchOptions{
		TopK:           topK,
		DocumentType:   domain.DocTypeAgentProfile,
		RetrievalFloor: 1.0,
		Scope:          domain.ScopeSystem, // ADR-0034: agent-profile ANN is a kernel read
	})
	if err != nil {
		return nil, err
	}

	var out []domain.AgentSearchResult
	for _, r := range raw {
		if r.Document.DocumentType != domain.DocTypeAgentProfile {
			continue
		}
		if r.Score < threshold {
			continue
		}
		agentID, _ := r.Document.Metadata["agent_id"].(string)
		sourceHash, _ := r.Document.Metadata["source_hash"].(string)
		out = append(out, domain.AgentSearchResult{
			AgentID:    agentID,
			SourceHash: sourceHash,
			Similarity: r.Score,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Similarity > out[j].Similarity
	})
	return out, nil
}
