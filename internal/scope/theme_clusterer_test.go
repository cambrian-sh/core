package scope_test

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"
)

func embDoc(id string, vec ...float32) domain.Document {
	return domain.Document{ID: id, Embedding: domain.Embedding{Vector: vec}}
}

func TestCosineThemeClusterer_GroupsSimilar(t *testing.T) {
	c := scope.NewCosineThemeClusterer(0.9)
	docs := []domain.Document{
		embDoc("a1", 1, 0, 0),
		embDoc("a2", 0.99, 0.01, 0), // near a1
		embDoc("b1", 0, 1, 0),       // orthogonal theme
	}
	clusters := c.Cluster(docs)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 themes, got %d", len(clusters))
	}
	// The first cluster should contain a1+a2.
	if len(clusters[0].Docs) != 2 {
		t.Errorf("expected a1+a2 grouped, got %d docs", len(clusters[0].Docs))
	}
}

func TestCosineThemeClusterer_SkipsEmbeddinglessDocs(t *testing.T) {
	c := scope.NewCosineThemeClusterer(0.9)
	docs := []domain.Document{{ID: "no-emb"}, embDoc("v", 1, 0)}
	clusters := c.Cluster(docs)
	if len(clusters) != 1 || clusters[0].Docs[0].ID != "v" {
		t.Errorf("docs without embeddings must be skipped, got %+v", clusters)
	}
}
