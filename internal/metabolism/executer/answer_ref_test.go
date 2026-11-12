package executer

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-summarise-a-document-step-gets-only-the-first: p12's "extract the full
// content" step re-typed a 38 kB tool output into its answer. With answer_ref the
// step's output IS its own tool result, validated by the kernel.
func TestAnswerRef_StepOutputIsItsOwnToolResult(t *testing.T) {
	led := newRunLedger()
	run := domain.LedgerRunID("run-1", 0)
	doc := strings.Repeat("report text ", 3000) + "Leipzig"
	body := `[{"type":"text","text":"` + doc + `"}]`
	ref := led.addToolResult(run, 0, "mcp:docs/read", body)
	d := &DAGExecutor{Ledger: led, LedgerReader: led, CurrentRunID: "run-1",
		DependencyFit: domain.DependencyFit{BudgetBytes: 64000, PageBytes: 16000}}
	var seen string
	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			return &domain.Handoff{FromAgent: "w", Payload: &domain.Payload{Data: []byte("the extracted document")},
				Context: map[string]string{domain.HandoffKeyAnswerRef: ref}}, nil
		}
		seen = h.Context[domain.HandoffKeyStepDependencies]
		return &domain.Handoff{FromAgent: "w", Payload: &domain.Payload{Data: []byte("summary")}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "extract"}, {Query: "summarise", DependsOn: []int{0}}}}
	if _, err := d.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	deps, err := domain.DecodeStepDependencies(seen)
	if err != nil || len(deps) != 1 {
		t.Fatalf("deps = %v %v", deps, err)
	}
	if deps[0].Content != doc {
		t.Fatalf("the dependant did not receive the tool result's text (got %d bytes)", len(deps[0].Content))
	}
}

func TestAnswerRef_Validation(t *testing.T) {
	led := newRunLedger()
	run := domain.LedgerRunID("run-1", 0)
	own := led.addToolResult(run, 2, "t", `"mine"`)
	otherStep := led.addToolResult(run, 1, "t", `"theirs"`)
	otherRun := led.addToolResult(domain.LedgerRunID("run-9", 0), 2, "t", `"elsewhere"`)
	stepOut := led.put(domain.LedgerEntry{Kind: domain.LedgerStepOutput, RunID: run, StepIndex: 2, Body: []byte("x")})
	d := &DAGExecutor{Ledger: led, LedgerReader: led, CurrentRunID: "run-1"}
	resp := func(ref string) *domain.Handoff {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("typed")}, Context: map[string]string{domain.HandoffKeyAnswerRef: ref}}
	}
	got, err := d.applyAnswerRef(context.Background(), 2, resp(own))
	if err != nil || string(got.Payload.Data) != "mine" {
		t.Fatalf("own tool result: %v %v", got, err)
	}
	if _, left := got.Context[domain.HandoffKeyAnswerRef]; left {
		t.Fatal("answer_ref is kernel-consumed: it must not reach the plan context")
	}
	for name, ref := range map[string]string{
		"another step's result": otherStep, "another run's result": otherRun,
		"a step output": stepOut, "unknown": "led_nope", "not a ledger id": "cid-123",
	} {
		if _, err := d.applyAnswerRef(context.Background(), 2, resp(ref)); err == nil ||
			domain.ClassOfStepError(err) != domain.StepFailureDeterministic {
			t.Errorf("%s: err = %v, want a deterministic refusal", name, err)
		}
	}
	plain := &domain.Handoff{Payload: &domain.Payload{Data: []byte("typed")}}
	if got, err := d.applyAnswerRef(context.Background(), 2, plain); err != nil || got != plain {
		t.Fatal("a response without answer_ref must pass through untouched")
	}
}
