package clusterer

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

func BenchmarkSweep_50(b *testing.B) {
	embeddings := make([]AgentEmbedding, 50)
	rng := rand.New(rand.NewSource(42))
	for i := range embeddings {
		emb := make([]float32, 128)
		for j := range emb {
			emb[j] = rng.Float32()
		}
		embeddings[i] = AgentEmbedding{
			AgentID:     fmt.Sprintf("agent-%04d", i),
			SourceHash:  fmt.Sprintf("hash-%04d", i),
			Embedding:   emb,
			Description: fmt.Sprintf("agent %d performs text processing and data analysis", i),
			Trait:       "cognitive",
		}
	}

	src := &sweepBenchSource{embeddings: embeddings}
	store := &sweepBenchStore{}
	gen := &sweepBenchGenerator{}
	c := New(src, store, gen, 0.7, 0.05, 2)

	b.ResetTimer()
	for range b.N {
		_ = c.runSweep(context.Background())
	}
}

type sweepBenchSource struct {
	embeddings []AgentEmbedding
}

func (s *sweepBenchSource) GetAllAgentEmbeddings(ctx context.Context) ([]AgentEmbedding, error) {
	return s.embeddings, nil
}

type sweepBenchStore struct{}

func (s *sweepBenchStore) SetCapabilities(agentID string, caps []string) error { return nil }
func (s *sweepBenchStore) SetClusterName(repID string, name string) error     { return nil }
func (s *sweepBenchStore) GetClusterName(repID string) (string, error)        { return "", nil }

type sweepBenchGenerator struct{}

func (g *sweepBenchGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return "test-cluster", nil
}
