package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ROUTE-02: the Gatekeeper records a Declaration->Interview->Merit funnel onto
// the AuctionTask when routing tracing is enabled, so a mis-route is
// explainable from the persisted auction event alone.

func findEntry[T any](items []T, id string, idOf func(T) string) (T, bool) {
	var zero T
	for _, it := range items {
		if idOf(it) == id {
			return it, true
		}
	}
	return zero, false
}

func TestFunnel_RecordsL1AndL3(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Provisional: false}
	sql := domain.AgentDefinition{ID: "sql-agent", Provisional: false}
	reg := newAgentSourceWith(active, sql)
	// sql-agent declares a format the task does not require -> fails Declaration.
	reg.manifests["sql-agent"] = &domain.AgentManifest{SupportedFormats: []string{"xml"}}

	cfg := defaultGatekeeperCfg()
	cfg.RoutingTraceEnabled = true
	gk := NewGatekeeper(reg, cfg)

	task := &domain.AuctionTask{ID: "t-funnel-1", RequiredFormats: []string{"json"}}
	candidates, err := gk.FindCandidates(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Funnel == nil {
		t.Fatal("expected funnel to be recorded when RoutingTraceEnabled")
	}

	// L1: both agents considered; active passes, sql-agent fails with a reason.
	if got := len(task.Funnel.L1); got != 2 {
		t.Fatalf("expected 2 L1 entries, got %d: %+v", got, task.Funnel.L1)
	}
	a1, ok := findEntry(task.Funnel.L1, "active", func(d domain.DeclarationResult) string { return d.AgentID })
	if !ok || !a1.Passed {
		t.Errorf("expected active to pass Declaration, got %+v (found=%v)", a1, ok)
	}
	s1, ok := findEntry(task.Funnel.L1, "sql-agent", func(d domain.DeclarationResult) string { return d.AgentID })
	if !ok || s1.Passed || s1.Reason == "" {
		t.Errorf("expected sql-agent to fail Declaration with a reason, got %+v", s1)
	}

	// L3: only the surviving candidate ranked, with merit components populated.
	if len(task.Funnel.L3) != len(candidates) || len(candidates) != 1 {
		t.Fatalf("expected 1 ranked L3 entry matching candidates, got L3=%d candidates=%d",
			len(task.Funnel.L3), len(candidates))
	}
	m := task.Funnel.L3[0]
	if m.AgentID != "active" {
		t.Errorf("expected 'active' in L3, got %q", m.AgentID)
	}
	// Neutral profile -> SuccessRate/TrustScore default to 0.5 in the merit formula.
	if m.SuccessRate != 0.5 || m.TrustScore != 0.5 {
		t.Errorf("expected neutral merit components 0.5/0.5, got %.3f/%.3f", m.SuccessRate, m.TrustScore)
	}
	if m.LatencyTerm <= 0 {
		t.Errorf("expected a positive latency term for a cognitive agent, got %.4f", m.LatencyTerm)
	}
}

func TestFunnel_RecordsL2SurvivorsAndEliminated(t *testing.T) {
	above := domain.AgentDefinition{ID: "above", Provisional: false}
	below := domain.AgentDefinition{ID: "below", Provisional: false}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{above, below}, nil)
	searcher := &fakeInterviewSearcher{results: map[string]float64{"above": 0.8, "below": 0.1}}

	cfg := defaultGatekeeperCfg()
	cfg.RoutingTraceEnabled = true
	gk := NewGatekeeper(reg, cfg, WithEmbedder(&mockEmbedder{}), WithSearcher(searcher))

	task := &domain.AuctionTask{ID: "t-funnel-2", Description: "find something"}
	if _, err := gk.FindCandidates(context.Background(), task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Funnel == nil {
		t.Fatal("expected funnel")
	}
	if task.Funnel.L2Threshold != DefaultSimilarityThreshold {
		t.Errorf("expected L2 threshold %.2f, got %.2f", DefaultSimilarityThreshold, task.Funnel.L2Threshold)
	}
	if len(task.Funnel.L2) != 2 {
		t.Fatalf("expected 2 L2 entries, got %d: %+v", len(task.Funnel.L2), task.Funnel.L2)
	}
	up, _ := findEntry(task.Funnel.L2, "above", func(v domain.InterviewResult) string { return v.AgentID })
	if !up.Survived || up.Similarity != 0.8 {
		t.Errorf("expected 'above' to survive with sim 0.8, got %+v", up)
	}
	down, _ := findEntry(task.Funnel.L2, "below", func(v domain.InterviewResult) string { return v.AgentID })
	if down.Survived {
		t.Errorf("expected 'below' to be eliminated in L2, got %+v", down)
	}
}

func TestFunnel_NilWhenTracingDisabled(t *testing.T) {
	reg := newAgentSourceWith(domain.AgentDefinition{ID: "active", Provisional: false})
	cfg := defaultGatekeeperCfg() // RoutingTraceEnabled defaults false in the test cfg
	gk := NewGatekeeper(reg, cfg)

	task := &domain.AuctionTask{ID: "t-funnel-3"}
	if _, err := gk.FindCandidates(context.Background(), task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Funnel != nil {
		t.Errorf("expected nil funnel when tracing disabled, got %+v", task.Funnel)
	}
}
