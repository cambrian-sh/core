package executer

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// (d) ADR-0131 D2 (T-20261101-step-taskevents-still-bill-len-4-although-the):
// a step whose lease usage was stamped on its response is billed from it, and
// the keys are consumed; a step without one falls back to len/4, flagged.
func TestDAGExecutor_StepTaskEventPrefersLeaseUsage(t *testing.T) {
	mw := &mockEventWriter{}
	d := &DAGExecutor{EventWriter: mw, DefaultInputCostPer1M: 2.0, DefaultOutputCostPer1M: 8.0}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "with lease usage"},
		{Query: "without lease usage", DependsOn: []int{0}},
	}}
	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		h := &domain.Handoff{FromAgent: "agent", Payload: &domain.Payload{Data: []byte("twelve chars")}, Context: map[string]string{}}
		if i == 0 {
			llm.StampStepUsage(h.Context, llm.TokenUsage{PromptTokens: 1200, CompletionTokens: 300, TotalTokens: 1500})
		}
		return h, nil
	}
	final, err := d.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	{
		if _, leaked := final[domain.HandoffKeyStepPromptTokens]; leaked {
			t.Fatal("step-usage keys must be consumed, not merged into the plan context")
		}
	}

	mw.mu.Lock()
	defer mw.mu.Unlock()
	if len(mw.events) != 2 {
		t.Fatalf("want 2 TaskEvents, got %d", len(mw.events))
	}
	byQuery := map[bool]domain.TaskEvent{}
	for _, e := range mw.events {
		byQuery[e.UsageEstimated] = e
	}
	real, est := byQuery[false], byQuery[true]
	if real.PromptTokens != 1200 || real.CompletionTokens != 300 || real.TotalTokens != 1500 ||
		real.EstimatedCost < 0.0048-1e-9 || real.EstimatedCost > 0.0048+1e-9 {
		t.Fatalf("lease-usage step = %+v, want 1200/300 at $0.0048, not estimated", real)
	}
	wantIn, wantOut := llm.EstimateTokens("without lease usage"), llm.EstimateTokens("twelve chars")
	if !est.UsageEstimated || est.PromptTokens != wantIn || est.CompletionTokens != wantOut {
		t.Fatalf("fallback step = %+v, want len/4 %d/%d flagged estimated", est, wantIn, wantOut)
	}
}
