package memory

import (
	"math"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func bapprox(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func TestStageAScore_AllMaxIsOne(t *testing.T) {
	b := NewBlender(DefaultBlendWeights())
	got := b.StageAScore(StageASignals{
		Cosine: 1, Recency: 1, MeanConfidence: 2, PageRank: 1, Activation: 1, Lexical: 1, GraphCoherence: 1,
	})
	if !bapprox(got, 1.0) {
		t.Fatalf("all-max signals must normalize to 1.0, got %f", got)
	}
}

func TestStageAScore_AllMinIsZero(t *testing.T) {
	b := NewBlender(DefaultBlendWeights())
	got := b.StageAScore(StageASignals{
		Cosine: 0, Recency: 0, MeanConfidence: 0, PageRank: 0, Activation: 0,
	})
	if got > 1e-6 {
		t.Fatalf("all-min signals must be ~0, got %f", got)
	}
}

func TestStageAScore_CosineOnlyEqualsCosine(t *testing.T) {
	b := NewBlender(BlendWeights{Cosine: 1})
	got := b.StageAScore(StageASignals{Cosine: 0.73, Recency: 0.5, MeanConfidence: 2, PageRank: 1, Activation: 1})
	if !bapprox(got, 0.73) {
		t.Fatalf("cosine-only weights must return cosine, got %f", got)
	}
}

func TestStageAScore_LexicalLiftsLowCosine(t *testing.T) {
	// The point of the lexical term: an exact-token chunk with LOW cosine but a
	// top lexical hit should outrank an identical chunk with no lexical match.
	b := NewBlender(DefaultBlendWeights())
	low := StageASignals{Cosine: 0.1, MeanConfidence: 0, PageRank: 0, Lexical: 0}
	lexHit := low
	lexHit.Lexical = 1.0 // top lexical rank
	if b.StageAScore(lexHit) <= b.StageAScore(low) {
		t.Fatalf("a top lexical hit must lift a low-cosine chunk: %f vs %f",
			b.StageAScore(lexHit), b.StageAScore(low))
	}
}

func TestStageAScore_CoherenceLiftsIsland(t *testing.T) {
	// The point of graph coherence: a chunk in the dense same-conversation cluster
	// (high coherence) must outrank an otherwise-identical island at the same
	// cosine/lexical — the cross-conversation distractor sinks.
	b := NewBlender(DefaultBlendWeights())
	island := StageASignals{Cosine: 0.3, Lexical: 0.0, GraphCoherence: 0.0}
	cluster := island
	cluster.GraphCoherence = 1.0
	if b.StageAScore(cluster) <= b.StageAScore(island) {
		t.Fatalf("a coherent chunk must outrank an island: %f vs %f",
			b.StageAScore(cluster), b.StageAScore(island))
	}
}

func TestStageAScore_MonotonicInPageRank(t *testing.T) {
	b := NewBlender(DefaultBlendWeights())
	base := StageASignals{Cosine: 0.5, Recency: 0.5, MeanConfidence: 1, PageRank: 0.1, Activation: 0.2}
	hi := base
	hi.PageRank = 0.9
	if b.StageAScore(hi) <= b.StageAScore(base) {
		t.Fatalf("higher pagerank must raise the score: %f vs %f", b.StageAScore(hi), b.StageAScore(base))
	}
}

func TestStageAScore_DegenerateWeightsFallBackToCosine(t *testing.T) {
	b := NewBlender(BlendWeights{}) // all zero
	got := b.StageAScore(StageASignals{Cosine: 0.42})
	if !bapprox(got, 0.42) {
		t.Fatalf("zero-weight blender must fall back to cosine, got %f", got)
	}
}

func TestStageAScore_ClampsOutOfRange(t *testing.T) {
	b := NewBlender(BlendWeights{Cosine: 1})
	if got := b.StageAScore(StageASignals{Cosine: 5}); !bapprox(got, 1.0) {
		t.Fatalf("cosine > 1 must clamp to 1, got %f", got)
	}
}

func TestStageAScore_MonotonicInRecency(t *testing.T) {
	b := NewBlender(DefaultBlendWeights())
	base := StageASignals{Cosine: 0.5, Recency: 0.0, MeanConfidence: 1, PageRank: 0.5}
	newer := base
	newer.Recency = 1.0 // most-recent-dated among the candidates
	if b.StageAScore(newer) <= b.StageAScore(base) {
		t.Fatalf("higher recency must raise the score: %f vs %f", b.StageAScore(newer), b.StageAScore(base))
	}
}

func TestDocEventTime_PrefersMetadataTimestamp(t *testing.T) {
	// The conversation timestamp (metadata) wins over ingest time (CreatedAt).
	d := domain.Document{Metadata: map[string]any{"timestamp": "2023-09-03T14:14:00"}, CreatedAt: time.Now()}
	if got := docEventTime(d); got.Year() != 2023 || got.Month() != time.September {
		t.Fatalf("expected metadata timestamp 2023-09, got %v", got)
	}
	// No metadata timestamp ⇒ fall back to CreatedAt (prior behaviour, never breaks).
	ca := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := docEventTime(domain.Document{CreatedAt: ca}); !got.Equal(ca) {
		t.Fatalf("expected fallback to CreatedAt %v, got %v", ca, got)
	}
}

// SetBlendWeights hot-swaps the live blend with no restart: CurrentBlendWeights
// reflects the new vector and the live Blender scores by it (ADR-0054 tuning seam).
func TestSetBlendWeights_HotSwap(t *testing.T) {
	q := &QueryService{}
	b := NewBlender(BlendWeights{Cosine: 1}) // start cosine-only
	q.EnableBlend(&b, nil)
	if got := q.CurrentBlendWeights(); got.Cosine != 1 || got.Recency != 0 {
		t.Fatalf("expected cosine-only initial weights, got %+v", got)
	}

	q.SetBlendWeights(BlendWeights{Recency: 1}) // hot-swap to recency-only
	if got := q.CurrentBlendWeights(); got.Cosine != 0 || got.Recency != 1 {
		t.Fatalf("expected recency-only after hot-swap, got %+v", got)
	}
	// The live Blender now ignores cosine and scores by recency.
	bl := q.blender.Load()
	if s := bl.StageAScore(StageASignals{Cosine: 1, Recency: 0}); s != 0 {
		t.Fatalf("after swap to recency-only, a cosine-only signal must score 0, got %f", s)
	}
	if s := bl.StageAScore(StageASignals{Cosine: 0, Recency: 1}); !bapprox(s, 1.0) {
		t.Fatalf("after swap to recency-only, a recency-max signal must score 1, got %f", s)
	}
}

func TestFinalScore_BgeBlend(t *testing.T) {
	if got := FinalScore(0.9, 0.4, 0.0); !bapprox(got, 0.4) {
		t.Fatalf("w_bge=0 must passthrough stageA, got %f", got)
	}
	if got := FinalScore(0.9, 0.4, 1.0); !bapprox(got, 0.9) {
		t.Fatalf("w_bge=1 must return bge, got %f", got)
	}
	if got := FinalScore(0.9, 0.4, 0.5); !bapprox(got, 0.65) {
		t.Fatalf("w_bge=0.5 must average, got %f", got)
	}
}
