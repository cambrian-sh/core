package memory

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 Phase 0: the ranking-v2 switches. Each decision ships behind its own
// flag, default off, read in the same change (AGENTS.md §3.4):
//
//	D1 Calibrated      execution.retrieval.stage_a_calibrated   + lexical_saturation_k
//	D3 TemporalIntent  execution.retrieval.temporal_intent_enabled + temporal_fit_weight
//
// D2 (a per-document cap in assembleLanes) was built, measured and rejected by the
// owner on 2026-11-03 (ADR-0136 D2); it was removed, not left behind a flag.
//
// With all of them at their zero values the ranking is the pre-ADR-0136 function
// (TestRankingV2_FlagsOffIsByteIdentical pins it against a golden written by the old
// code).
type RankingV2 struct {
	// Calibrated (D1) scores Stage A from pool-INDEPENDENT features: each feature's
	// value for a chunk depends on that chunk and the query only, never on which other
	// candidates are in the pool (TestStageA_FeaturesArePoolIndependent).
	Calibrated bool
	// LexicalSaturationK is k in the calibrated lexical feature s/(s+k), s = the
	// adapter's ts_rank_cd. ≤ 0 ⇒ DefaultLexicalSaturationK.
	LexicalSaturationK float64
	// TemporalIntent (D3) reads a target band and an order from the query text
	// (TemporalIntent) and turns them into the temporal-fit feature, the recency
	// direction, and — for absolute ranges only — a hard event-time window.
	TemporalIntent bool
	// TemporalFitWeight is the Stage-A weight of the temporal-fit feature, applied
	// only to queries that name a time band.
	TemporalFitWeight float64

	// FactLane (ADR-0136 D4, execution.facts.lane_enabled) runs a fact vector search
	// beside the dense and lexical lanes; each fact hit contributes its SOURCE CHUNK
	// as a primary hit (fact_lane.go). Needs the fact reader the QueryService was
	// constructed with (NewQueryServiceWithFacts); without one it is inert.
	FactLane bool
	// FactLaneTopK is how many facts the lane fetches per query (≤ 0 ⇒ 20).
	FactLaneTopK int
	// FactLaneScopedDocuments (ADR-0136 A4, execution.facts.lane_scoped_documents) is N
	// of the document-scoped lane: the lane ALSO searches only the facts of the first N
	// distinct source documents of the fused dense+lexical pool (fact_lane.go). 0 = off;
	// needs FactLane.
	FactLaneScopedDocuments int
	// FactLaneScopedTopK is how many facts one scoped search keeps (≤ 0 ⇒ 20).
	FactLaneScopedTopK int
	// FactMatchWeight is the Stage-A weight of the fact_match feature, joining only
	// when the lane found a fact for the query.
	FactMatchWeight float64
	// Supersession (D5, execution.facts.supersession_enabled) makes the lane read
	// supersession: the current_fact feature for current/latest queries and the
	// value chain for history/earliest ones (both need TemporalIntent's order).
	Supersession bool
	// CurrentFactWeight is the Stage-A weight of current_fact.
	CurrentFactWeight float64
}

// DefaultLexicalSaturationK puts the half-credit point of s/(s+k) at the scale of a
// STRONG lexical match, not at the scorer's floor.
//
// How ts_rank_cd (normalisation 0) scores here: the adapter's tsquery is an OR of
// the query's lexemes, so every single occurrence of any query word is one cover of
// weight D = 0.1, and s ≈ 0.1 × (occurrences of query words in the chunk).
//
// The first value, 0.1, sat AT that floor: one occurrence of one common query word
// already earned half the credit and the median lexical-list row 0.75, so on the
// OR-query's list (up to over_fetch rows, any word) the feature became a step —
// "contains any query word" — worth about +0.13 of Stage A at the shipped weights,
// as large as the whole cosine spread. Measured on the ADR-0136 gate
// (T-20261103-adr-0136-d1-stage-a-calibrated-lowers-evidence): the share of
// lexically matched rows in the top 50 rose 0.63 → 0.89 and evidence turns that
// match the question by meaning but share no word with it (8 of the 15 newly
// missing) fell out.
//
// 1.5 is the least-squares fit of s/(s+k) to the legacy feature, 1/rank, over the
// first ten lexical ranks — the scale the shipped hand weights were tuned against
// (ADR-0054) — measured on stored CONTROL runs from ts_rank_cd estimated per
// returned passage: 1.48 on the temporal-ku diagnosis run, 1.35 on the event-time
// run, and 1.64 / 1.50 on the two gate controls. The typical top lexical hit there
// scores s ≈ 1.1-1.3 (feature ≈ 0.45), a typical tenth hit s ≈ 0.4 (0.21), a lone
// common word s = 0.1 (0.06). It is a fixed property of the scorer, not of any
// pool, so D1's pool independence holds. The weights are still the hand weights;
// fitting them is D7.
const DefaultLexicalSaturationK = 1.5

// defaultCalibratedHalfLife is the recency half-life when the configured one is
// unusable (config only validates the half-life in decay mode; the calibrated
// path always decays). It is the shipped blend_recency_half_life_hours, 90 days.
const defaultCalibratedHalfLife = 2160 * time.Hour

// coherenceCap is the fixed divisor of the calibrated coherence feature. Coherence
// sums 1/df over the entities a chunk shares with each query-relevant seed; an
// entity shared by two pool chunks contributes 0.5, so 1.0 is reached by two such
// rare shared entities (or one shared with two seeds). Beyond that the chunk is
// clearly in the seeds' cluster and more overlap should not add credit. It replaces
// the per-pool divide-by-max, under which the best-connected candidate always
// scored 1 however weakly it was connected.
const coherenceCap = 1.0

// SetRankingV2 installs the ADR-0136 switches. Set once at composition (app.go,
// next to EnableBlend), before the service serves; a ranking option, not a
// control, so a post-construction setter is the existing pattern here
// (SetRecencyCurve), not an AGENTS.md §2.3 enforcement seam.
func (q *QueryService) SetRankingV2(r RankingV2) {
	if r.LexicalSaturationK <= 0 {
		r.LexicalSaturationK = DefaultLexicalSaturationK
	}
	if r.TemporalFitWeight < 0 {
		r.TemporalFitWeight = 0
	}
	if r.FactLaneTopK <= 0 {
		r.FactLaneTopK = defaultFactLaneTopK
	}
	if r.FactLaneScopedDocuments < 0 {
		r.FactLaneScopedDocuments = 0
	}
	if r.FactLaneScopedTopK <= 0 {
		r.FactLaneScopedTopK = defaultFactLaneTopK
	}
	if r.FactMatchWeight < 0 {
		r.FactMatchWeight = 0
	}
	if r.CurrentFactWeight < 0 {
		r.CurrentFactWeight = 0
	}
	q.rankingV2 = r
}

// saturatedLexical is D1's lexical feature: the adapter's ts_rank_cd mapped by
// s/(s+k). 0 (not lexically matched) stays 0.
func saturatedLexical(s, k float64) float64 {
	if s <= 0 {
		return 0
	}
	if k <= 0 {
		k = DefaultLexicalSaturationK
	}
	return s / (s + k)
}

// pageRankCorpusSize is the optional capability that turns a raw PageRank into a
// fixed-scale feature: the number of chunks the last PageRank recompute covered.
// *postgres.PgVectorAdapter has it (PageRankMeta).
type pageRankCorpusSize interface {
	PageRankMeta(ctx context.Context) (computedAt time.Time, chunkCount, tripletCount int, err error)
}

// calibratedPageRank is D1's PageRank feature, INTERIM: x·N / (x·N + 1), N = the
// recompute's corpus size. PageRank is a probability distribution, so x·N is a
// chunk's centrality relative to the uniform 1/N: an average chunk scores 0.5, a
// chunk ten times as central 0.91. It is fixed per corpus and independent of the
// pool, which is what D1 requires. ADR-0136 asks for a global percentile computed
// by the PageRank worker; that needs a worker + schema change and is ticketed
// (T-20261103-adr-0136-d1-pagerank-feature-is-an-interim-x-n). N ≤ 0
// (never computed) ⇒ 0: a missing signal contributes nothing.
func calibratedPageRank(x float64, n int) float64 {
	if x <= 0 || n <= 0 {
		return 0
	}
	v := x * float64(n)
	return v / (v + 1)
}

// calibratedRecency is D1's recency feature: the A1 decay 2^(-age/halfLife), age
// measured from the asker's reference time ONLY. The A1 curve's fallback anchor —
// the newest candidate — depends on the pool, so without a reference time the
// calibrated feature is 0 rather than pool-relative. dir is D3's direction: +1
// decay (latest/current), -1 inverted (earliest), 0 off. A chunk dated after the
// reference time had not happened yet for the asker and scores 0 (ADR-0134 D4).
func calibratedRecency(event, ref time.Time, halfLife time.Duration, dir int) float64 {
	if dir == 0 || ref.IsZero() || event.IsZero() || event.After(ref) {
		return 0
	}
	if halfLife <= 0 {
		halfLife = defaultCalibratedHalfLife
	}
	f := math.Exp2(-ref.Sub(event).Seconds() / halfLife.Seconds())
	if dir < 0 {
		return 1 - f
	}
	return f
}

// calibratedSeedRelevance orders the calibrated coherence seeds by a relevance
// that, like the features, does not depend on the pool: max(cosine RawScore, the
// saturated ts_rank_cd). The legacy seeds use RRF's 1/rank, under which one new
// top lexical hit halves every other lexical row's relevance and reshuffles the seeds.
func calibratedSeedRelevance(r domain.SearchResult, k float64) float64 {
	s := r.RawScore
	if l := saturatedLexical(r.LexicalRaw, k); l > s {
		s = l
	}
	return s
}

// calibratedCoherence is D1's coherence feature: the raw IDF-weighted seed overlap
// divided by the fixed coherenceCap and clamped, instead of min-max over the pool.
func calibratedCoherence(raw float64) float64 {
	return clamp01(raw / coherenceCap)
}

// calibratedFeatures computes D1's per-chunk feature values. Every value is a
// function of the chunk, the query and the corpus — never of the other candidates:
//
//	cosine     clamp01(cos): the theoretical range with negatives floored, exactly as
//	           the legacy blend reads it (cosine was already pool-independent)
//	lexical    saturatedLexical(ts_rank_cd, k) — not RRF's 1/rank
//	recency    calibratedRecency from reference_time, only when D3 asks for an order
//	pagerank   calibratedPageRank (interim; see its comment)
//	coherence  raw IDF seed overlap / coherenceCap — not divided by the pool's max
//	confidence mean triplet confidence (the Blender maps 0..2 → 0..1), unchanged
//	activation documents.activation_strength, unchanged
//	temporal   TimeIntent.Fit, when the query names a band
//
// Coherence keeps two inputs that are pool-level by construction: its seeds (the
// top-N candidates by calibratedSeedRelevance) and its IDF (document frequency
// within the pool). A candidate that is not relevant enough to be a seed and shares
// no entity changes neither, which is what TestStageA_FeaturesArePoolIndependent
// pins; a new, more relevant SEED legitimately changes whom a chunk is coherent
// with. The ADR-0136 observations record this.
//
// A signal that is missing for a chunk contributes 0 (ADR-0136 D1): no feature is
// ever stretched to the full [0,1] range because of who else is in the pool.
func (q *QueryService) calibratedFeatures(ctx context.Context, results []domain.SearchResult, queryVec []float32, w BlendWeights, pageranks, confidences map[string]float64, ti TimeIntent, haveIntent bool) []StageASignals {
	rv := q.rankingV2
	var corpusN int
	if w.PageRank > 0 {
		if pc, ok := q.rankSignals.(pageRankCorpusSize); ok {
			if _, n, _, err := pc.PageRankMeta(ctx); err != nil {
				slog.WarnContext(ctx, "blend: pagerank corpus size lookup failed; calibrated pagerank feature is 0 this query", "err", err)
			} else {
				corpusN = n
			}
		}
	}
	var coherence map[string]float64
	if w.GraphCoherence > 0 {
		k := rv.LexicalSaturationK
		coherence, _ = chunkCoherenceRaw(ctx, q.chunkTriplets, results, defaultCoherenceSeedN,
			func(r domain.SearchResult) float64 { return calibratedSeedRelevance(r, k) })
	}
	var ref time.Time
	if tq, ok := domain.EventTimeQueryFromContext(ctx); ok {
		ref = tq.ReferenceTime
	}
	dir := 0
	if rv.TemporalIntent && haveIntent {
		dir = ti.RecencyDirection()
	}
	facts := factHitsFromContext(ctx)
	out := make([]StageASignals, len(results))
	for i := range results {
		d := results[i].Document
		cos := results[i].RawScore
		if cos == 0 {
			cos = results[i].Score
		}
		if len(queryVec) > 0 && len(d.Embedding.Vector) > 0 {
			cos = cosineSimilarity(queryVec, d.Embedding.Vector)
		}
		var fit float64
		if haveIntent {
			start, end := domain.StoredSpan(d)
			fit = ti.Fit(start, end)
		}
		out[i] = StageASignals{
			Cosine:         cos,
			Recency:        calibratedRecency(docEventTime(d), ref, q.recencyCurve.HalfLife, dir),
			MeanConfidence: confidences[d.ID],
			PageRank:       calibratedPageRank(pageranks[d.ID], corpusN),
			Activation:     d.ActivationStrength,
			Lexical:        saturatedLexical(results[i].LexicalRaw, rv.LexicalSaturationK),
			GraphCoherence: calibratedCoherence(coherence[d.ID]),
			TemporalFit:    fit,
			FactMatch:      facts.match(d.ID),
			CurrentFact:    facts.current(d.ID),
		}
	}
	return out
}

// applyCalibratedStageA is D1's Stage A: the convex combination Σ wᵢ·fᵢ / Σ wᵢ over
// calibratedFeatures, sorted descending. The weights are the configured blend
// weights, already conditioned on the query by stageAWeightsFor.
func (q *QueryService) applyCalibratedStageA(ctx context.Context, results []domain.SearchResult, queryVec []float32, blender Blender, pageranks, confidences map[string]float64, ti TimeIntent, haveIntent bool) []domain.SearchResult {
	feats := q.calibratedFeatures(ctx, results, queryVec, blender.w, pageranks, confidences, ti, haveIntent)
	for i := range results {
		results[i].Score = blender.StageAScore(feats[i])
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	logCalibratedFeatures(ctx, feats)
	return results
}

// logCalibratedFeatures is a diagnostic only (DEBUG; it changes no score): the
// pool's mean cosine, lexical and coherence features and how many rows matched
// lexically. A feature whose scale has drifted from the others — as the lexical one
// did at k = 0.1, where nearly every row of the lexical list scored ≥ 0.5 — shows
// up here per query, without a database or a per-row dump.
func logCalibratedFeatures(ctx context.Context, feats []StageASignals) {
	if len(feats) == 0 || !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}
	var cos, lex, coh float64
	matched := 0
	for _, f := range feats {
		cos += clamp01(f.Cosine)
		lex += f.Lexical
		coh += f.GraphCoherence
		if f.Lexical > 0 {
			matched++
		}
	}
	n := float64(len(feats))
	slog.DebugContext(ctx, "ranking v2: calibrated features", "pool", len(feats),
		"lexical_matched", matched, "mean_cosine", cos/n, "mean_lexical", lex/n, "mean_coherence", coh/n)
}

// timeIntentKey carries the query's D3 intent from searchByType (which has the
// query text) to applyStageABlend (which has only the candidates).
type timeIntentKey struct{}

func withTimeIntent(ctx context.Context, ti TimeIntent) context.Context {
	return context.WithValue(ctx, timeIntentKey{}, ti)
}

func timeIntentFromContext(ctx context.Context) (TimeIntent, bool) {
	ti, ok := ctx.Value(timeIntentKey{}).(TimeIntent)
	return ti, ok
}

// stageAWeightsFor returns the Stage-A weights for ONE query. A query-conditioned
// feature the query does not activate is taken out of the convex combination
// entirely — numerator and denominator — so a query with no time phrase ranks as if
// D3 did not exist:
//
//   - D3 on: the temporal-fit weight joins only when the query names a band; the
//     recency weight survives only when the query asks for an order (latest,
//     current, earliest).
//   - D1 on, D3 off: recency is off (ADR-0136 D1: "only when D3 activates it").
//
// ok is false when nothing changed, so the caller keeps the shared Blender and the
// flags-off arithmetic is untouched.
func (q *QueryService) stageAWeightsFor(base BlendWeights, ti TimeIntent, haveIntent bool) (BlendWeights, bool) {
	rv := q.rankingV2
	w := base
	if rv.TemporalIntent && haveIntent {
		if ti.Target != nil {
			w.TemporalFit = rv.TemporalFitWeight
		}
		if ti.RecencyDirection() == 0 {
			w.Recency = 0
		}
	} else if rv.Calibrated {
		w.Recency = 0
	}
	return w, w != base
}

// factWeightsFor adds the ADR-0136 D4/D5 fact features to one query's weights, by the
// same rule as stageAWeightsFor: a feature joins the convex combination only for a
// query that activates it, so every other query ranks exactly as if the fact layer
// did not exist.
//
//   - fact_match joins when the fact lane found at least one fact for the query.
//   - current_fact joins when supersession is on, D3 reads an order of current or
//     latest from the query, and some matched fact is the current value of its chain.
func (q *QueryService) factWeightsFor(w BlendWeights, hits factLaneHits, ti TimeIntent, haveIntent bool) (BlendWeights, bool) {
	rv := q.rankingV2
	if !rv.FactLane || len(hits) == 0 {
		return w, false
	}
	base := w
	w.FactMatch = rv.FactMatchWeight
	if rv.Supersession && rv.TemporalIntent && haveIntent && wantsCurrentValue(ti) && hits.anyCurrent() {
		w.CurrentFact = rv.CurrentFactWeight
	}
	return w, w != base
}
