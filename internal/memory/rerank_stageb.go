package memory

import (
	"context"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
)

// defaultRerankTopK is how many top Stage-A candidates the cross-encoder rescores
// (ADR-0054 Stage B). It must be large enough to CONTAIN the gold: on LoCoMo the
// gold lives deep (recall@25≈0.81 but recall@100≈0.94), so reranking only the
// top-25 caps the achievable recall at 0.81 — the reranker cannot promote a chunk
// it never sees. 50 captures the recall@50≈0.91 pool at half the per-query cost of
// 100. K is the dominant CPU-latency driver (K forward passes/query); it is config.
const defaultRerankTopK = 50

// Reranker is the ADR-0054 Stage-B port: a cross-encoder that scores the
// relevance of each document to the query, returning one score per document in
// input order. Implemented by network.RerankerDispatcher, which dispatches to the
// warm `reranker_agent` system organ via the Auctioneer (no auction) — the same
// privileged-organ pattern as the kg_extractor's TripletExtractor.
//
// Errors are the caller's signal to fail-soft: a down/erroring reranker leaves the
// Stage-A ordering intact (retrieval never fails because the oracle is unreachable,
// the same contract as the LLM broker, ADR-0042 / ADR-0054 D1).
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
}

// EnableReranker turns on ADR-0054 Stage B: after the Stage-A blend, the top-K
// candidates are rescored by the bge cross-encoder and blended via
// FinalScore = wBGE·bge + (1-wBGE)·stageA. Flag-gated at the wiring site (default
// off). nil reranker ⇒ no-op (Stage-A order kept). topK ≤ 0 ⇒ defaultRerankTopK;
// wBGE ≤ 0 ⇒ the ADR default 0.5 (an enabled reranker with zero weight would be a
// no-op, so a non-positive weight is read as "use the default", not "ignore bge").
func (q *QueryService) EnableReranker(r Reranker, topK int, wBGE float64) {
	q.reranker = r
	if topK > 0 {
		q.rerankTopK = topK
	}
	if wBGE > 0 {
		q.rerankWeight = wBGE
	} else {
		q.rerankWeight = 0.5
	}
}

// applyStageBRerank rescores the top-K Stage-A candidates with the cross-encoder
// and re-sorts. Only the head (top rerankTopK) is sent to the oracle — the tail is
// below it by Stage-A and reranking it would cost K forward passes for chunks that
// won't surface anyway. The head items take FinalScore; the tail keeps its Stage-A
// score, so the head reorders among itself above the tail (a head item only sinks
// into the tail if the oracle judged it genuinely less relevant).
//
// Fail-soft: a nil reranker, an error, or a score-count mismatch returns the input
// untouched (Stage-A order). The query never fails because the reranker is down.
func (q *QueryService) applyStageBRerank(ctx context.Context, query string, results []domain.SearchResult) []domain.SearchResult {
	if q.reranker == nil || len(results) == 0 {
		return results
	}
	k := q.rerankTopK
	if k <= 0 {
		k = defaultRerankTopK
	}
	if k > len(results) {
		k = len(results)
	}
	head := results[:k]
	docs := make([]string, k)
	for i := range head {
		docs[i] = head[i].Document.Text
	}
	scores, err := q.reranker.Rerank(ctx, query, docs)
	if err != nil || len(scores) != k {
		slog.Warn("rerank: Stage-B unavailable; keeping Stage-A order", "err", err, "got_scores", len(scores), "want", k)
		return results
	}
	wBGE := q.rerankWeight
	if wBGE <= 0 {
		wBGE = 0.5
	}
	// The agent returns RAW cross-encoder logits on the model's own scale (often
	// all-negative, even for relevant passages). Min-max normalize them across
	// THIS head into [0,1] so the bge term is commensurate with the [0,1] Stage-A
	// score before the linear FinalScore blend — the same treatment PageRank gets
	// in applyStageABlend. A sigmoid would crush all-negative logits toward 0 and
	// erase the gaps; min-max preserves them (best→1, worst→0). Zero range (all
	// equal, or a single candidate) ⇒ no discriminative bge signal (all 0), so
	// FinalScore degenerates to (1-wBGE)·stageA and the Stage-A order is kept.
	lo, hi := scores[0], scores[0]
	for _, s := range scores {
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	rng := hi - lo
	for i := range head {
		bge := 0.0
		if rng > 0 {
			bge = (scores[i] - lo) / rng
		}
		head[i].Score = FinalScore(bge, head[i].Score, wBGE)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results
}
