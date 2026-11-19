package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingRecall is a PrincipalRecall double: it answers per document type and
// records who each lane read as and with what query.
type recordingRecall struct {
	mu       sync.Mutex
	byType   map[string][]domain.SearchResult
	calls    []recallCall
	failType string
}

type recallCall struct {
	p       domain.PrincipalRef
	query   string
	docType string
}

func (r *recordingRecall) RecallFor(_ context.Context, p domain.PrincipalRef, query, docType string) ([]domain.SearchResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recallCall{p, query, docType})
	r.mu.Unlock()
	if docType == r.failType {
		return nil, fmt.Errorf("boom")
	}
	return append([]domain.SearchResult(nil), r.byType[docType]...), nil
}

func (r *recordingRecall) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, c.docType)
	}
	return out
}

func factDocs(n int) []domain.SearchResult {
	docs := make([]domain.SearchResult, n)
	for i := range docs {
		docs[i] = domain.SearchResult{
			Document: domain.Document{
				ID:           fmt.Sprintf("fact-%d", i),
				Text:         fmt.Sprintf("fact %d", i),
				DocumentType: domain.DocTypeMnemonicFact,
				Metadata:     map[string]interface{}{"source_agent": "test_agent"},
			},
			Score:    float64(n-i) * 0.1,
			RawScore: 0.9,
		}
	}
	return docs
}

// planningCtx is a context carrying the planning principal (the authenticated
// caller of the Execute that plans).
func planningCtx() context.Context {
	return domain.WithPrincipal(context.Background(), domain.PrincipalRef{ID: "ayse", Kind: domain.PrincipalUser})
}

// Facts returned in LTMEnrichment.Facts.
func TestWorkspaceStage_LTMEnrichmentFacts(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(2)}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)

	result, err := ws.PrimeForPlanning(planningCtx(), "auth deployment")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if len(result.Facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(result.Facts))
	}
}

// ENG-465 + ENG-540 (owner decisions 2026-11-14/15): the FACT lane reads as the
// planning principal; the PROCEDURE lane is global company knowledge and reads as
// domain.SystemPrincipal — both through the recall (QueryService.RecallFor), both
// with the task as the query. (The negatives lane was removed with ENG-1645.)
func TestWorkspaceStage_LanesReadUnderTheDecidedScopes(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(1)}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)
	if _, err := ws.PrimeForPlanning(planningCtx(), "auth deployment"); err != nil {
		t.Fatal(err)
	}
	want := map[string]domain.PrincipalRef{
		domain.DocTypeMnemonicFact:      {ID: "ayse", Kind: domain.PrincipalUser},
		domain.DocTypeMnemonicProcedure: domain.SystemPrincipal,
	}
	seen := map[string]bool{}
	for _, c := range rec.calls {
		if c.p != want[c.docType] || c.query != "auth deployment" {
			t.Errorf("lane %s read as %v with %q; want %v with the task", c.docType, c.p, c.query, want[c.docType])
		}
		seen[c.docType] = true
	}
	for ty := range want {
		if !seen[ty] {
			t.Errorf("lane %s never ran (ran %v)", ty, rec.types())
		}
	}
}

// No principal ⇒ no priming: nothing is read, the enrichment is empty, the plan
// is not failed.
func TestWorkspaceStage_NoPrincipalNoPriming(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(3)}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)
	got, err := ws.PrimeForPlanning(context.Background(), "auth deployment")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts)+len(got.Procedures) != 0 || len(rec.calls) != 0 {
		t.Fatalf("primed without a principal: %+v, calls %v", got, rec.calls)
	}
	m, err := ws.PrimeForExecution(context.Background(), &domain.ExecutionPlan{Subject: "s"}, nil)
	if err != nil || len(m) != 0 {
		t.Fatalf("PrimeForExecution without a principal = %v, %v", m, err)
	}
}

// ENG-1645: the planner no longer recalls negative_edge failure records (nothing
// writes them since ENG-1595), so legacy rows never reach the enrichment.
func TestWorkspaceStage_NoNegativesLane(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{
		domain.DocTypeMnemonicFact: factDocs(1),
		domain.DocTypeNegativeEdge: {{Document: domain.Document{ID: "neg-0", Text: "BLOCKED", DocumentType: domain.DocTypeNegativeEdge}}},
	}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)
	result, err := ws.PrimeForPlanning(planningCtx(), "some query")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	for _, f := range result.Facts {
		if f.Document.DocumentType == domain.DocTypeNegativeEdge {
			t.Error("negative_edge doc must not appear in Facts")
		}
	}
	for _, c := range rec.calls {
		if c.docType == domain.DocTypeNegativeEdge {
			t.Fatalf("PrimeForPlanning still recalls the retired negatives lane: %v", rec.calls)
		}
	}
}

// RetrievalFloor and MinFactCosine drop DENSE hits below them; rows with no
// cosine (lexical or fact-lane hits) are kept, as QueryService's floor keeps them.
func TestWorkspaceStage_FloorsTestRawCosineOnly(t *testing.T) {
	t.Parallel()
	hits := []domain.SearchResult{
		{Document: domain.Document{ID: "dense-hi"}, RawScore: 0.9},
		{Document: domain.Document{ID: "dense-mid"}, RawScore: 0.5},  // below MinFactCosine
		{Document: domain.Document{ID: "dense-low"}, RawScore: 0.25}, // below RetrievalFloor
		{Document: domain.Document{ID: "lexical"}, RawScore: 0},
	}
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: hits}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.3)
	ws.MinFactCosine = 0.6
	got, err := ws.PrimeForPlanning(planningCtx(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range got.Facts {
		ids = append(ids, h.Document.ID)
	}
	if fmt.Sprint(ids) != "[dense-hi lexical]" {
		t.Fatalf("facts = %v, want [dense-hi lexical]", ids)
	}
}

// PrimeForExecution slot limit enforced (map format for DAGExecutor).
func TestWorkspaceStage_SlotLimit(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(8)}}
	ws := NewWorkspaceStage(rec, nil, 10, 3, 0.2)

	result, err := ws.PrimeForExecution(planningCtx(), &domain.ExecutionPlan{Subject: "test"}, nil)
	if err != nil {
		t.Fatalf("PrimeForExecution failed: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d entries, want exactly 3 (slot limit)", len(result))
	}
}

// Empty FACT results → empty LTMEnrichment, no panic; a failing lane costs only
// enrichment.
func TestWorkspaceStage_EmptyAndFailingLanes(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{failType: domain.DocTypeNegativeEdge}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)

	result, err := ws.PrimeForPlanning(planningCtx(), "unknown query")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if len(result.Facts) != 0 {
		t.Errorf("expected empty enrichment, got facts=%d", len(result.Facts))
	}
}

// Execution slots used for PrimeForExecution.
func TestWorkspaceStage_ExecutionSlots(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(10)}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)

	result, err := ws.PrimeForExecution(planningCtx(), &domain.ExecutionPlan{Subject: "test"}, nil)
	if err != nil {
		t.Fatalf("PrimeForExecution failed: %v", err)
	}
	if len(result) != 5 {
		t.Errorf("got %d entries, want exactly 5 (execution slots)", len(result))
	}
}

// 2026-11-05 (decisions/scenes-cancelled.md): enrichment never queries scenes.
func TestWorkspaceStage_NoScenePriming(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{byType: map[string][]domain.SearchResult{domain.DocTypeMnemonicFact: factDocs(2)}}
	ws := NewWorkspaceStage(rec, nil, 10, 5, 0.2)
	if _, err := ws.PrimeForPlanning(planningCtx(), "auth deployment"); err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	for _, ty := range rec.types() {
		if ty == "mnemonic_scene" {
			t.Errorf("enrichment must not search mnemonic_scene; searched types %v", rec.types())
		}
	}
}
