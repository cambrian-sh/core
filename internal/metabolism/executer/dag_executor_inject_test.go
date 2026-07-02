package executer

import (
	"context"
	"sync"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// An operator injection mid-run pauses the coordinator, folds the instruction
// into a new forward plan (deterministic default = the instruction as a step),
// and executes it. ADR-0047 A1.1 / 0047-22.
func TestDAGExecutor_InjectFoldsInstructionIntoRun(t *testing.T) {
	d := &DAGExecutor{}

	var mu sync.Mutex
	var executed []string
	injectedOnce := false

	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		query := string(h.Payload.Data)
		mu.Lock()
		executed = append(executed, query)
		first := !injectedOnce
		if query == "a" {
			injectedOnce = true
		}
		mu.Unlock()

		// On the first step, deliver an operator correction into the running plan.
		if query == "a" && first {
			if err := d.Inject("operator correction"); err != nil {
				t.Errorf("Inject: %v", err)
			}
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "a"},
		{Query: "b", DependsOn: []int{0}},
	}}
	if _, err := d.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The injected instruction ran...
	if !contains(executed, "operator correction") {
		t.Fatalf("expected the injected instruction to execute, got %v", executed)
	}
	// ...and the original step "a" ran before the injection replaced the forward plan.
	if !contains(executed, "a") {
		t.Fatalf("expected original step a to have run, got %v", executed)
	}
}

// Inject rejects an empty instruction.
func TestDAGExecutor_InjectRejectsEmpty(t *testing.T) {
	d := &DAGExecutor{}
	if err := d.Inject("   "); err == nil {
		t.Fatal("expected an error for an empty instruction")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
