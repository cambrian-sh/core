package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// poolAnswerer stands in for QueryService.AnswerSystem: it applies the request's
// pool narrowing exactly as the real answer lane does (memory.AnswerEvidenceFrom)
// and returns what it would have synthesized.
type poolAnswerer struct{ pool []domain.SearchResult }

func (p poolAnswerer) AnswerSystem(ctx context.Context, _ string) (string, string, []domain.SearchResult, error) {
	opts, _ := memory.AnswerEvidenceFrom(ctx)
	var out []domain.SearchResult
	for _, r := range p.pool {
		if opts.Keep == nil || opts.Keep(r) {
			out = append(out, r)
		}
	}
	if opts.Max > 0 && len(out) > opts.Max {
		out = out[:opts.Max]
	}
	return "answer", "see [1] and [2]", out, nil
}

func hit(id, source, session string, imp float64) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id, Text: id, Metadata: map[string]interface{}{
		"source": source, "session_id": session, "importance": imp,
	}}}
}

// ENG-223: a filter that drops evidence item 2 must not shift later markers away
// from the answer's [n] labels, and the filters must reach the answer lane (pool
// narrowing before synthesis), not only trim the returned citations.
func TestAnswerMemory_FiltersNarrowThePoolAndMarkersMatchLabels(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	svc.SetMemoryAnswerer(poolAnswerer{pool: []domain.SearchResult{
		hit("c1", "wiki", "s1", 0.9), hit("c2", "chat", "s1", 0.9), hit("c3", "wiki", "s1", 0.9),
	}})

	resp, err := svc.AnswerMemory(opCtx(), &pb.AnswerMemoryRequest{Query: "q", Source: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	cits := resp.GetCitations()
	if len(cits) != 2 || cits[0].GetDocId() != "c1" || cits[1].GetDocId() != "c3" {
		t.Fatalf("citations = %v, want c1, c3", cits)
	}
	for i, c := range cits {
		if c.GetMarker() != int32(i+1) {
			t.Fatalf("citation %s marker %d, want %d (the synthesizer's label)", c.GetDocId(), c.GetMarker(), i+1)
		}
	}

	// top_k caps the pool; session and min_importance filter it.
	resp, err = svc.AnswerMemory(opCtx(), &pb.AnswerMemoryRequest{Query: "q", TopK: 1, Session: "s1", MinImportance: 0.5})
	if err != nil || len(resp.GetCitations()) != 1 || resp.GetCitations()[0].GetDocId() != "c1" {
		t.Fatalf("top_k 1: %v err=%v", resp.GetCitations(), err)
	}
}
