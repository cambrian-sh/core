package executer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type memRunTracer struct {
	mu    sync.Mutex
	spans []domain.TraceSpan
}

func (m *memRunTracer) Span(_ context.Context, s domain.TraceSpan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
}

// ADR-0145 D10: every executed step leaves an invoke_agent span with the step as
// dispatched and the output it handed on; a failed step carries its error.
func TestRunTrace_StepSpans(t *testing.T) {
	tr := &memRunTracer{}
	ex := &DAGExecutor{RunTracer: tr, MaxReplanAttempts: 0}
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
		{Query: "list invoices", RequiredEffects: []string{"read"}},
		{Query: "sum them", DependsOn: []int{0}, IsThought: true},
	}}
	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		if i == 1 {
			return nil, errors.New("model refused")
		}
		return &domain.Handoff{FromAgent: "worker-1", Payload: &domain.Payload{Data: []byte("INV-1 120.00")}}, nil
	}
	ex.ThoughtFn = stepFn
	_, _ = ex.Execute(t.Context(), plan, nil, stepFn)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.spans) < 2 {
		t.Fatalf("spans = %d, want >= 2", len(tr.spans))
	}
	var ok, failed *domain.TraceSpan
	for i := range tr.spans {
		switch tr.spans[i].Attributes["cambrian.step.index"] {
		case 0:
			ok = &tr.spans[i]
		case 1:
			failed = &tr.spans[i]
		}
	}
	if ok == nil || ok.Name != "invoke_agent worker-1" || ok.Attributes["cambrian.step.query"] != "list invoices" {
		t.Fatalf("step 0 span: %+v", ok)
	}
	out := ok.Attributes[domain.GenAIOutputMessages].([]map[string]any)
	if out[0]["parts"].([]map[string]any)[0]["content"] != "INV-1 120.00" {
		t.Errorf("step 0 output: %v", out)
	}
	if failed == nil || failed.Error == "" {
		t.Errorf("failed step must carry its error: %+v", failed)
	}
}
