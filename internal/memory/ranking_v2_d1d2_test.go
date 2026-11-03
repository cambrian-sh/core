package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261103-adr-0136-d1-stage-a-calibrated-lowers-evidence: the real cause. The
// adapter's tsquery ORs the query's lexemes, so ts_rank_cd ≈ 0.1 per occurrence of
// any query word. At k = 0.1 a chunk with one or two occurrences of a COMMON query
// word took half or more of the lexical credit and outranked a chunk that answers the
// question by meaning (higher cosine) but shares no word with it — the shape of the
// evidence turns that went missing on the gate. With the shipped weights, a 0.1
// cosine lead must beat a weak lexical hit, and a strong lexical match (the scale of
// a typical top lexical hit, s ≈ 1.3) must still beat the same 0.1 cosine lead.
//
// (This file also held the ADR-0136 D2 per-document-cap tests; D2 was rejected by
// the owner on 2026-11-03 and removed with them.)
func TestCalibratedLexical_WeakHitDoesNotOutweighCosine(t *testing.T) {
	shipped := BlendWeights{Cosine: 0.4, Lexical: 0.2, GraphCoherence: 0.05, Confidence: 0.1}
	rank := func(k, distractorTS float64) []string {
		evidence := domain.Document{ID: "evidence", Metadata: map[string]any{"document_id": "S1"}}
		distractor := domain.Document{ID: "distractor", Metadata: map[string]any{"document_id": "S2"}}
		dense := []domain.SearchResult{
			{Document: evidence, Score: 0.72, RawScore: 0.72},
			{Document: distractor, Score: 0.62, RawScore: 0.62},
		}
		lexical := []domain.SearchResult{{Document: distractor, Score: distractorTS}}
		pool := rrfFuse(dense, lexical, 60, 100, 1)
		q := &QueryService{}
		bl := NewBlender(shipped)
		q.EnableBlend(&bl, fixedSignals{})
		q.SetRankingV2(RankingV2{Calibrated: true, LexicalSaturationK: k})
		var ids []string
		for _, r := range q.applyStageABlend(context.Background(), pool, nil) {
			ids = append(ids, r.Document.ID)
		}
		return ids
	}
	// Teeth: the old constant shows the defect on this pool.
	if got := rank(0.1, 0.2); got[0] != "distractor" {
		t.Fatalf("k=0.1 should reproduce the gate defect (two common-word covers beat a 0.1 cosine lead), got %v", got)
	}
	for _, ts := range []float64{0.1, 0.2} {
		if got := rank(DefaultLexicalSaturationK, ts); got[0] != "evidence" {
			t.Errorf("default k: a weak lexical hit (ts_rank_cd %.1f) outranked a 0.1 cosine lead: %v", ts, got)
		}
	}
	if got := rank(DefaultLexicalSaturationK, 1.3); got[0] != "distractor" {
		t.Errorf("default k: a strong lexical match (ts_rank_cd 1.3) must still beat a 0.1 cosine lead, got %v", got)
	}
	if DefaultLexicalSaturationK != 1.5 {
		t.Fatalf("DefaultLexicalSaturationK = %v; its value is derived in its comment, change both together", DefaultLexicalSaturationK)
	}
	if got := saturatedLexical(0.1, DefaultLexicalSaturationK); got > 0.07 {
		t.Errorf("one occurrence of one query word must earn little credit, got %v", got)
	}
}
