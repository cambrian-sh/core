package scope

import (
	"math"

	"github.com/cambrian-sh/core/domain"
)

// CosineThemeClusterer groups Tier-0 documents into theme clusters by embedding
// cosine similarity (greedy single-link above a threshold). Documents without an
// embedding are skipped. This is the default ThemeClusterer for promotion; it is
// deliberately simple — the k-anonymity floor, not cluster quality, is what makes
// promotion safe. ADR-0034 (D11).
type CosineThemeClusterer struct {
	Threshold float64
}

// NewCosineThemeClusterer builds a clusterer. threshold<=0 defaults to 0.85.
func NewCosineThemeClusterer(threshold float64) *CosineThemeClusterer {
	if threshold <= 0 {
		threshold = 0.85
	}
	return &CosineThemeClusterer{Threshold: threshold}
}

// Cluster groups docs greedily: each not-yet-assigned doc seeds a cluster, and all
// remaining unassigned docs within Threshold cosine of the seed join it.
func (c *CosineThemeClusterer) Cluster(docs []domain.Document) []ThemeCluster {
	n := len(docs)
	if n == 0 {
		return nil
	}
	assigned := make([]bool, n)
	var clusters []ThemeCluster
	for i := 0; i < n; i++ {
		if assigned[i] || len(docs[i].Embedding.Vector) == 0 {
			continue
		}
		assigned[i] = true
		cluster := ThemeCluster{Docs: []domain.Document{docs[i]}}
		for j := i + 1; j < n; j++ {
			if assigned[j] || len(docs[j].Embedding.Vector) == 0 {
				continue
			}
			if cosine(docs[i].Embedding.Vector, docs[j].Embedding.Vector) >= c.Threshold {
				assigned[j] = true
				cluster.Docs = append(cluster.Docs, docs[j])
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// cosine returns the cosine similarity of two equal-length vectors (0 on mismatch
// or zero-norm).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
