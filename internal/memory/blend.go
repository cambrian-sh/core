package memory

// Stage-A multi-signal blend (ADR-0054 D1). The cheap, CPU-only ranking signal
// computed for every candidate chunk: a weighted, normalized sum of cosine,
// recency, confidence, PageRank, and activation. No GPU, no LLM — the bge
// cross-encoder (Stage B) is a later, opt-in upgrade that combines with this
// via `final = w_bge·bge + (1-w_bge)·stageA`.
//
// Pure (no DB/I/O) so the blend math is unit-testable; the query path supplies
// the per-chunk signals (cosine from the query vector, confidence/pagerank from
// the stores, recency/activation from the document).

// BlendWeights are the Stage-A signal weights. They need NOT sum to 1 — the
// Blender normalizes by their sum, so the score is always in [0,1] (and the raw
// magnitudes only express RELATIVE importance). This is what keeps Stage-A
// comparable to a bare cosine score when bge is disabled.
type BlendWeights struct {
	Cosine     float64
	Recency    float64
	Confidence float64
	PageRank   float64
	Activation float64
	Lexical    float64 // hybrid: full-text/RRF rank signal (ADR-0054)
	// GraphCoherence is the chunk's connectivity to the rest of the candidate
	// set over the chunk_triplets graph (shared entities incl. the `dated at` /
	// `spoke at` timestamp hubs). It is the "is this chunk part of the dominant
	// cluster?" signal: same-conversation chunks share entities/timestamps and
	// cohere; a cross-conversation distractor is an island and gets ~0. This is
	// spreading activation pointed at the live KG (entity/document_edges path
	// deferred), the graph-native conversation disambiguator.
	GraphCoherence float64
}

// DefaultBlendWeights mirrors ADR-0054's Stage-A defaults (the bge 0.50 lives in
// Stage B, not here). Their ratio is what matters; the Blender normalizes. Lexical
// is weighted so an exact-token (low-cosine) chunk surfaced by hybrid still ranks.
func DefaultBlendWeights() BlendWeights {
	return BlendWeights{Cosine: 0.40, Lexical: 0.25, GraphCoherence: 0.20, Confidence: 0.10, PageRank: 0.05, Recency: 0.05}
}

func (w BlendWeights) total() float64 {
	return w.Cosine + w.Recency + w.Confidence + w.PageRank + w.Activation + w.Lexical + w.GraphCoherence
}

// StageASignals are the per-chunk inputs to the blend. The Blender normalizes
// confidence into [0,1] internally; the other signals are already [0,1].
type StageASignals struct {
	Cosine         float64 // query→chunk cosine, [0,1] (clamped)
	Recency        float64 // pre-normalized recency [0,1], 1=most recent; min-max over the candidate set from the conversation timestamp (applyStageABlend), NOT an absolute decay
	MeanConfidence float64 // mean of the chunk's triplet confidences, [0,2]
	PageRank       float64 // precomputed structural prior, [0,1]
	Activation     float64 // documents.activation_strength, [0,1]
	Lexical        float64 // hybrid full-text/RRF rank signal, [0,1] (0 = not lexically matched)
	GraphCoherence float64 // chunk_triplets connectivity to the rest of the pool, [0,1] (0 = island)
}

// Blender holds normalized Stage-A weights.
type Blender struct {
	w   BlendWeights
	sum float64
}

// NewBlender builds a Blender. A non-positive weight total degenerates to
// cosine-only (StageAScore returns the clamped cosine) — a safe fallback if a
// config zeroes every weight.
func NewBlender(w BlendWeights) Blender {
	return Blender{w: w, sum: w.total()}
}

// StageAScore returns the normalized blend in [0,1].
func (b Blender) StageAScore(s StageASignals) float64 {
	if b.sum <= 0 {
		return clamp01(s.Cosine)
	}
	recency := clamp01(s.Recency)           // pre-normalized by the caller (min-max over candidates)
	conf := clamp01(s.MeanConfidence / 2.0) // confidence tiers are 0/1/2 → [0,1]
	raw := b.w.Cosine*clamp01(s.Cosine) +
		b.w.Recency*recency +
		b.w.Confidence*conf +
		b.w.PageRank*clamp01(s.PageRank) +
		b.w.Activation*clamp01(s.Activation) +
		b.w.Lexical*clamp01(s.Lexical) +
		b.w.GraphCoherence*clamp01(s.GraphCoherence)
	return raw / b.sum
}

// FinalScore combines the bge cross-encoder score (Stage B) with the Stage-A
// blend: final = w_bge·bge + (1-w_bge)·stageA. Phase 1 never calls this
// (w_bge=0 ⇒ stageA passthrough); it's here so Stage B (ADR-0054 Phase 2)
// composes without reshaping the blend. w_bge is clamped to [0,1].
func FinalScore(bge, stageA, wBGE float64) float64 {
	wBGE = clamp01(wBGE)
	return wBGE*clamp01(bge) + (1-wBGE)*stageA
}
