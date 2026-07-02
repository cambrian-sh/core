package watcher

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/metabolism/agentmgr"

	"google.golang.org/grpc/metadata"
)

func TestWatcher_ValidateSignal_RejectsMissingSignalType(t *testing.T) {
	w := New(nil, nil, nil, WatcherConfig{})

	err := w.ValidateSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Payload:   &domain.Payload{Data: []byte("test")},
	})

	if err == nil {
		t.Error("expected error for missing _signal_type")
	}
}

func TestWatcher_ValidateSignal_AcceptsValidSignal(t *testing.T) {
	w := New(nil, nil, nil, WatcherConfig{})

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
	w := New(nil, nil, nil, WatcherConfig{})

	err := w.ValidateSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Context:   map[string]string{"_signal_type": ""},
	})

	if err == nil {
		t.Error("expected error for empty _signal_type")
	}
}

func TestWatcher_BuildInspirationPrompt(t *testing.T) {
	w := New(nil, nil, nil, WatcherConfig{})

	prompt := w.BuildInspiration("STOCK_LOW", "item:GPU level:5", "Recent purchases: GPU-x500")

	if prompt == "" {
		t.Error("inspiration prompt should not be empty")
	}
	if prompt == "STOCK_LOW" {
		t.Error("inspiration should include signal data, not just the type")
	}
}

type testMemoryAgent struct {
	fetchContextFn func(ctx context.Context, query string) string
}

func (m *testMemoryAgent) FetchContext(ctx context.Context, query string) string {
	if m.fetchContextFn != nil {
		return m.fetchContextFn(ctx, query)
	}
	return ""
}

func TestWatcher_EnrichSignal_CallsMemoryAgent(t *testing.T) {
	called := false
	mem := &testMemoryAgent{
		fetchContextFn: func(ctx context.Context, query string) string {
			called = true
			return "relevant LTM context"
		},
	}
	w := New(nil, mem, nil, WatcherConfig{})

	ltmCtx := w.EnrichSignal(context.Background(), "STOCK_LOW", "GPU level:5")

	if !called {
		t.Error("EnrichSignal should call FetchContext on MemoryAgent")
	}
	if ltmCtx == "" {
		t.Error("EnrichSignal should return LTM context")
	}
}

func TestWatcher_EnrichSignal_NilMemoryAgent(t *testing.T) {
	w := New(nil, nil, nil, WatcherConfig{})

	ltmCtx := w.EnrichSignal(context.Background(), "STOCK_LOW", "data")

	if ltmCtx != "" {
		t.Error("EnrichSignal with nil MemoryAgent should return empty string")
	}
}

func TestWatcher_ValidateToken(t *testing.T) {
	m := &agentmgr.AgentManager{}
	w := New(m, nil, nil, WatcherConfig{})

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
	w := New(nil, nil, planner, WatcherConfig{})

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
	planner := &testPlanner{
		planFn: func(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
			return &domain.ExecutionPlan{Subject: "no action", Steps: nil}, nil
		},
	}
	w := New(nil, nil, planner, WatcherConfig{})

	plan, err := w.ProcessSignal(context.Background(), "STOCK_LOW", "data", "")

	if err != nil {
		t.Fatalf("ProcessSignal: %v", err)
	}
	if plan == nil {
		t.Error("expected a plan (possibly empty), got nil")
	}
}
