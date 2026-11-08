package executer_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// A side-effecting fan-out expands only over a STRUCTURED source list
// (domain.ResolveFanOut); over plain lines it is demoted to one step. A worker
// answering "list the overdue invoices" in lines therefore turned a parallel write
// batch into one sequential step. The executor now tells the source step's worker,
// deterministically, to end with the items as one fenced JSON array, naming the
// per-item step they feed (T-20261107-planner-prompt-does-not-steer-fan-out-no-rule).
//
// The fake worker answers like a model: plain lines, plus the fenced array only
// when its task asks for one. Red before the change: the source task carried no
// such instruction, the write was demoted, and it ran once.
func TestDAGExecutor_FanOutSourceIsAskedForAJSONArray(t *testing.T) {
	dag := &executer.DAGExecutor{MaxFanOutWidth: 10}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "List the overdue invoices", RequiredEffects: []string{"read"}},
		{Query: "Send a payment reminder for invoice {item}", FanOutOver: ptr(0), DependsOn: []int{0},
			RequiredEffects: []string{"send"}},
		{Query: "Report what was sent", DependsOn: []int{1}, IsThought: false},
	}}

	var mu sync.Mutex
	var sourceTask string
	var writes []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		out := "ok"
		switch {
		case strings.HasPrefix(q, "List the overdue invoices"):
			mu.Lock()
			sourceTask = q
			mu.Unlock()
			out = "The overdue invoices are:\nINV-1\nINV-2\nINV-3"
			if strings.Contains(q, "fenced json code block") {
				out += "\n\n```json\n[\"INV-1\",\"INV-2\",\"INV-3\"]\n```"
			}
		case strings.HasPrefix(q, "Send a payment reminder"):
			mu.Lock()
			writes = append(writes, q)
			mu.Unlock()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}

	if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(sourceTask, "Send a payment reminder for invoice {item}") {
		t.Errorf("the source task does not name the per-item step it feeds:\n%s", sourceTask)
	}
	if len(writes) != 3 {
		t.Fatalf("want one reminder per invoice (3), got %d: %v", len(writes), writes)
	}
}

// A step that feeds no fan-out is dispatched with its query unchanged, and an
// untemplated parametric step (demoted whatever the source says) asks for nothing.
func TestDAGExecutor_NonSourceStepQueryIsUnchanged(t *testing.T) {
	dag := &executer.DAGExecutor{MaxFanOutWidth: 10}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "scan"},
		{Query: "write every file", FanOutOver: ptr(0), DependsOn: []int{0}},
		{Query: "summarise", DependsOn: []int{1}},
	}}
	var mu sync.Mutex
	var got []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		mu.Lock()
		got = append(got, string(h.Payload.Data))
		mu.Unlock()
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("a\nb")}}, nil
	}
	if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, q := range got {
		if strings.Contains(q, "fenced json") {
			t.Errorf("step query %q carries the fan-out source instruction", q)
		}
	}
}

// A thought step that selects the items reads its task from the ctx step, not the
// handoff payload, so it must receive the instruction there.
func TestDAGExecutor_ThoughtFanOutSourceGetsTheInstruction(t *testing.T) {
	var thoughtTask string
	dag := &executer.DAGExecutor{
		MaxFanOutWidth: 10,
		ThoughtFn: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			st, _ := domain.PlanStepFrom(ctx)
			thoughtTask = st.Query
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("```json\n[\"x\"]\n```")}}, nil
		},
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "Pick the cases to close", IsThought: true},
		{Query: "Close case {item}", FanOutOver: ptr(0), DependsOn: []int{0}},
	}}
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}
	if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(thoughtTask, "Close case {item}") || !strings.Contains(thoughtTask, "JSON array") {
		t.Errorf("thought source task lacks the instruction:\n%s", thoughtTask)
	}
}

// plannerQuery is a dispatched step's query as the planner wrote it: the payload
// without the fan-out source instruction the executor appends to a source step.
func plannerQuery(h *domain.Handoff) string {
	q := string(h.Payload.Data)
	if i := strings.Index(q, "\n\nYour result is also the item list"); i >= 0 {
		return q[:i]
	}
	return q
}
