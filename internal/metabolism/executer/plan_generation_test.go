package executer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261108-ledger-step-indices-collide-across-replans-in: a replan restarts
// step indices under the same run, so every plan generation after the first
// records to the ledger under its own run id (domain.LedgerRunID) and its steps
// carry the generation on ctx (the server binds it to the step lease, so the
// tool executor records the step's tool results under the same id). A replanned
// step 0 therefore never sees the swapped-out plan's step 0 entries.
func TestReplan_NewPlanGenerationHasItsOwnLedgerRun(t *testing.T) {
	var mu sync.Mutex
	var gens []int
	calls := 0
	stepFn := func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		mu.Lock()
		defer mu.Unlock()
		gens = append(gens, domain.PlanGenerationFrom(ctx))
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return &domain.Handoff{FromAgent: "a", Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}
	d := &DAGExecutor{ReplanHandler: &fixedReplanHandler{query: "a different step"}, MaxReplanAttempts: 1, CurrentRunID: "run-1"}
	ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{RunID: "run-1", StepIndex: -1})
	if got := d.ledgerProvenance(ctx).RunID; got != "run-1" {
		t.Fatalf("generation 0 records under the run itself, got %q", got)
	}
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "the first step"}}}
	if _, err := d.ExecuteFrom(ctx, plan, map[string]string{}, stepFn, 0); err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 || gens[0] != 0 || gens[1] != 1 {
		t.Fatalf("step generations = %v, want [0 1]", gens)
	}
	if got := d.ledgerProvenance(ctx).RunID; got != "run-1-g1" {
		t.Fatalf("the replanned plan records under %q, want run-1-g1", got)
	}
	if domain.LedgerRunID("run-1", 0) != "run-1" || domain.LedgerRunID("", 3) != "" {
		t.Fatal("LedgerRunID must leave generation 0 and an empty run unchanged")
	}
}
