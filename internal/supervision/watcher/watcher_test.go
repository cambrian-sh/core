package watcher

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"

	"google.golang.org/grpc/metadata"
)

func TestWatcher_ValidateSignal_RejectsMissingSignalType(t *testing.T) {
	t.Parallel()
	w := New(nil, nil, nil, nil, nil, WatcherConfig{})

	err := w.ValidateSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Payload:   &domain.Payload{Data: []byte("test")},
	})

	if err == nil {
		t.Error("expected error for missing _signal_type")
	}
}

func TestWatcher_ValidateSignal_AcceptsValidSignal(t *testing.T) {
	t.Parallel()
	w := New(nil, nil, nil, nil, nil, WatcherConfig{})

	err := w.ValidateSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Payload:   &domain.Payload{Data: []byte("stock alert")},
		Context:   map[string]string{"_signal_type": "STOCK_LOW"},
	})

	if err != nil {
		t.Errorf("expected no error for valid signal, got %v", err)
	}
}

func TestWatcher_ValidateSignal_EmptySignalType(t *testing.T) {
	t.Parallel()
	w := New(nil, nil, nil, nil, nil, WatcherConfig{})

	err := w.ValidateSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Context:   map[string]string{"_signal_type": ""},
	})

	if err == nil {
		t.Error("expected error for empty _signal_type")
	}
}

func TestWatcher_BuildInspirationPrompt(t *testing.T) {
	t.Parallel()
	w := New(nil, nil, nil, nil, nil, WatcherConfig{})

	prompt := w.BuildInspiration("STOCK_LOW", "item:GPU level:5", "Recent purchases: GPU-x500")

	if prompt == "" {
		t.Error("inspiration prompt should not be empty")
	}
	if prompt == "STOCK_LOW" {
		t.Error("inspiration should include signal data, not just the type")
	}
}

// recordingRecall is a SignalRecall double: it records who each recall read as.
type recordingRecall struct {
	calls   []domain.PrincipalRef
	queries []string
	results []domain.SearchResult
}

func (r *recordingRecall) RecallFor(_ context.Context, p domain.PrincipalRef, query, docType string) ([]domain.SearchResult, error) {
	r.calls = append(r.calls, p)
	r.queries = append(r.queries, query)
	return r.results, nil
}

// ENG-1339 (owner decision 2026-11-14): signal enrichment reads AS THE SIGNAL'S
// PRINCIPAL (the authenticated stream principal on ctx) through QueryService.
func TestWatcher_EnrichSignal_ReadsAsTheSignalsPrincipal(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{results: []domain.SearchResult{
		{Document: domain.Document{ID: "m1", Text: "GPU restock takes 3 days"}, Score: 0.4, RawScore: 0.9},
	}}
	w := New(nil, rec, nil, nil, nil, WatcherConfig{RecallMinCosine: 0.7})
	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("inventory_daemon"))

	ltmCtx := w.EnrichSignal(ctx, "STOCK_LOW", "GPU level:5")

	if len(rec.calls) != 1 || rec.calls[0] != domain.AgentPrincipal("inventory_daemon") {
		t.Fatalf("recall read as %v, want agent:inventory_daemon", rec.calls)
	}
	if rec.queries[0] != "STOCK_LOW GPU level:5" {
		t.Errorf("query = %q", rec.queries[0])
	}
	if !strings.Contains(ltmCtx, "GPU restock takes 3 days") {
		t.Errorf("context = %q, want the recalled memory", ltmCtx)
	}
}

// No principal ⇒ no priming: nothing is read.
func TestWatcher_EnrichSignal_NoPrincipalNoPriming(t *testing.T) {
	t.Parallel()
	rec := &recordingRecall{results: []domain.SearchResult{{Document: domain.Document{ID: "m1", Text: "x"}, RawScore: 0.9}}}
	w := New(nil, rec, nil, nil, nil, WatcherConfig{})
	if got := w.EnrichSignal(context.Background(), "STOCK_LOW", "data"); got != "" || len(rec.calls) != 0 {
		t.Fatalf("enriched %q with %d reads and no principal", got, len(rec.calls))
	}
}

func TestWatcher_EnrichSignal_NilRecall(t *testing.T) {
	t.Parallel()
	w := New(nil, nil, nil, nil, nil, WatcherConfig{})
	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("d"))
	if ltmCtx := w.EnrichSignal(ctx, "STOCK_LOW", "data"); ltmCtx != "" {
		t.Error("EnrichSignal with no recall should return empty string")
	}
}

// The formatting keeps poisoned memories as failures to avoid, drops dense hits
// at or below the cosine floor, keeps rows with no cosine, and caps the list.
func TestFormatSignalRecall(t *testing.T) {
	t.Parallel()
	res := []domain.SearchResult{
		{Document: domain.Document{ID: "p", Text: "bad route", Metadata: map[string]interface{}{"is_poisoned": true, "correction": "use route B"}}},
		{Document: domain.Document{ID: "low", Text: "weak"}, RawScore: 0.5},
		{Document: domain.Document{ID: "a", Text: "first"}, RawScore: 0.9},
		{Document: domain.Document{ID: "lex", Text: "lexical"}, RawScore: 0},
		{Document: domain.Document{ID: "c", Text: "over the cap"}, RawScore: 0.95},
	}
	out := formatSignalRecall(res, 0.7, 2)
	for _, want := range []string{"ERROR: bad route", "CORRECTION: use route B", "1. (Rel: 0.00) first", "2. (Rel: 0.00) lexical"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:%s", want, out)
		}
	}
	for _, not := range []string{"weak", "over the cap"} {
		if strings.Contains(out, not) {
			t.Errorf("%q must be dropped:%s", not, out)
		}
	}
}

func TestWatcher_ValidateToken(t *testing.T) {
	t.Parallel()
	m := &agentmgr.AgentManager{}
	w := New(m, nil, nil, nil, nil, WatcherConfig{})

	t.Run("missing token metadata", func(t *testing.T) {
		ctx := context.Background()
		_, err := w.ValidateToken(ctx)
		if err == nil {
			t.Error("expected error for missing token")
		}
	})

	t.Run("empty token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", ""))
		_, err := w.ValidateToken(ctx)
		if err == nil {
			t.Error("expected error for empty token")
		}
	})
}

type testPlanner struct {
	planFn func(ctx context.Context, userInput string) (*domain.ExecutionPlan, error)
}

func (p *testPlanner) GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
	if p.planFn != nil {
		return p.planFn(ctx, userInput)
	}
	return &domain.ExecutionPlan{Subject: "empty", Steps: nil}, nil
}

func TestWatcher_ProcessSignal_CallsPlanner(t *testing.T) {
	t.Parallel()
	var receivedPrompt string
	planner := &testPlanner{
		planFn: func(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
			receivedPrompt = userInput
			return &domain.ExecutionPlan{
				Subject: "restock GPU",
				Steps:   []domain.Step{{Query: "order GPUs"}},
			}, nil
		},
	}
	w := New(nil, nil, planner, nil, nil, WatcherConfig{})

	plan, err := w.ProcessSignal(context.Background(), "STOCK_LOW", "GPU level:5", "recent orders: GPU-x10")

	if err != nil {
		t.Fatalf("ProcessSignal: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a plan, got nil")
	}
	if receivedPrompt == "" {
		t.Error("planner was not called")
	}
	if plan.Subject != "restock GPU" {
		t.Errorf("expected subject 'restock GPU', got %q", plan.Subject)
	}
}

func TestWatcher_ProcessSignal_EmptyPlanNoOp(t *testing.T) {
	t.Parallel()
	planner := &testPlanner{
		planFn: func(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
			return &domain.ExecutionPlan{Subject: "no action", Steps: nil}, nil
		},
	}
	w := New(nil, nil, planner, nil, nil, WatcherConfig{})

	plan, err := w.ProcessSignal(context.Background(), "STOCK_LOW", "data", "")

	if err != nil {
		t.Fatalf("ProcessSignal: %v", err)
	}
	if plan == nil {
		t.Error("expected a plan (possibly empty), got nil")
	}
}
