package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// fakeReranker returns a score per document by text lookup (input order
// preserved). A nil scoreByText with err set simulates an unreachable oracle.
type fakeReranker struct {
	scoreByText map[string]float64
	err         error
	gotDocs     []string // records what was sent (to assert only the head is reranked)
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, documents []string) ([]float64, error) {
	f.gotDocs = documents
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float64, len(documents))
	for i, d := range documents {
		out[i] = f.scoreByText[d]
	}
	return out, nil
}

func res(id, text string, stageA float64) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id, Text: text}, Score: stageA}
}

// A candidate the cross-encoder loves but Stage-A ranked low must overtake a
// candidate Stage-A ranked high but the cross-encoder dislikes. FinalScore at
// w_bge=0.5: A=0.5·0.1+0.5·0.8=0.45 ; B=0.5·0.9+0.5·0.6=0.75 ⇒ B first.
func TestStageBRerank_PromotesCrossEncoderWinner(t *testing.T) {
	q := &QueryService{}
	q.EnableReranker(&fakeReranker{scoreByText: map[string]float64{
		"alpha": 0.1, // high Stage-A, low relevance
		"bravo": 0.9, // low Stage-A, high relevance
	}}, 0, 0.5)

	in := []domain.SearchResult{res("a", "alpha", 0.8), res("b", "bravo", 0.6)}
	out := q.applyStageBRerank(context.Background(), "q", in)

	if out[0].Document.ID != "b" {
		t.Fatalf("expected the cross-encoder winner 'b' first, got %q (scores: %v)",
			out[0].Document.ID, []float64{out[0].Score, out[1].Score})
	}
}

// Only the top-K Stage-A candidates are sent to the oracle; the tail is left
// untouched (the reranker is the dominant cost — don't pay it for chunks that
// won't surface).
func TestStageBRerank_OnlyHeadIsReranked(t *testing.T) {
	f := &fakeReranker{scoreByText: map[string]float64{"x1": 0.5, "x2": 0.5}}
	q := &QueryService{}
	q.EnableReranker(f, 2, 0.5) // K=2

	in := []domain.SearchResult{
		res("1", "x1", 0.9), res("2", "x2", 0.8), res("3", "x3", 0.7), res("4", "x4", 0.6),
	}
	_ = q.applyStageBRerank(context.Background(), "q", in)

	if len(f.gotDocs) != 2 {
		t.Fatalf("expected only the top-2 sent to the reranker, got %d: %v", len(f.gotDocs), f.gotDocs)
	}
}

// Fail-soft: an unreachable/erroring oracle leaves the Stage-A order intact —
// retrieval must never fail because the reranker is down (ADR-0054 D1).
func TestStageBRerank_FailSoftKeepsStageAOrder(t *testing.T) {
	q := &QueryService{}
	q.EnableReranker(&fakeReranker{err: errors.New("agent down")}, 0, 0.5)

	in := []domain.SearchResult{res("a", "alpha", 0.8), res("b", "bravo", 0.6)}
	out := q.applyStageBRerank(context.Background(), "q", in)

	if out[0].Document.ID != "a" || out[1].Document.ID != "b" {
		t.Fatalf("fail-soft must preserve Stage-A order [a,b], got [%s,%s]",
			out[0].Document.ID, out[1].Document.ID)
	}
}

// A short score slice (oracle returned the wrong count) is treated as a failure,
// not silently applied — keep the Stage-A order rather than corrupt the ranking.
func TestStageBRerank_ScoreCountMismatchFailSoft(t *testing.T) {
	short := &fakeRerankerShort{}
	q := &QueryService{}
	q.EnableReranker(short, 0, 0.5)

	in := []domain.SearchResult{res("a", "alpha", 0.8), res("b", "bravo", 0.6)}
	out := q.applyStageBRerank(context.Background(), "q", in)

	if out[0].Document.ID != "a" || out[1].Document.ID != "b" {
		t.Fatalf("score-count mismatch must fail-soft to [a,b], got [%s,%s]",
			out[0].Document.ID, out[1].Document.ID)
	}
}

type fakeRerankerShort struct{}

func (fakeRerankerShort) Rerank(_ context.Context, _ string, _ []string) ([]float64, error) {
	return []float64{0.9}, nil // one score for two docs
}

// nil reranker is a no-op (the flag is off): order is whatever Stage-A produced.
func TestStageBRerank_NilRerankerNoop(t *testing.T) {
	q := &QueryService{} // reranker not enabled
	in := []domain.SearchResult{res("a", "alpha", 0.8), res("b", "bravo", 0.6)}
	out := q.applyStageBRerank(context.Background(), "q", in)
	if len(out) != 2 || out[0].Document.ID != "a" {
		t.Fatalf("nil reranker must pass through unchanged, got %v", out)
	}
}
