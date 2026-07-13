package executer

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func collectPlanState(bus *domain.InMemoryEventBus) *[]domain.PlanStateChanged {
	var events []domain.PlanStateChanged
	bus.Subscribe(domain.EventTypePlanState, func(e domain.DomainEvent) {
		events = append(events, e.(domain.PlanStateChanged))
	})
	return &events
}

// A successful multi-step run enters "running" and ends with a single terminal
// "completed" event, all for one PlanID/SessionID. ADR-0047 0047-17.
func TestDAGExecutor_PublishesPlanState_Success(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	events := collectPlanState(bus)

	d := &DAGExecutor{EventBus: bus, CurrentSessionID: "s1"}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "a"},
		{Query: "b", DependsOn: []int{0}},
	}}
	if _, err := d.Execute(context.Background(), plan, nil, okStep("ok", nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ev := *events
	if len(ev) < 2 {
		t.Fatalf("expected multiple plan-state events, got %d", len(ev))
	}
	if ev[0].Status != "running" || ev[0].Terminal {
		t.Fatalf("first event should be non-terminal running, got %+v", ev[0])
	}
	last := ev[len(ev)-1]
	if !last.Terminal || last.Status != "completed" {
		t.Fatalf("last event should be terminal completed, got %+v", last)
	}
	planID := ev[0].PlanID
	for _, e := range ev {
		if e.PlanID != planID || e.SessionID != "s1" {
			t.Fatalf("inconsistent PlanID/SessionID: %+v (want %s/s1)", e, planID)
		}
	}
}

// A failing plan (no replan) ends with a terminal "failed" event.
func TestDAGExecutor_PublishesPlanState_Failed(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	events := collectPlanState(bus)

	d := &DAGExecutor{EventBus: bus, CurrentSessionID: "s1"}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "boom"}}}
	if _, err := d.Execute(context.Background(), plan, nil, failStep("kaboom")); err == nil {
		t.Fatal("expected the plan to fail")
	}

	ev := *events
	if len(ev) == 0 {
		t.Fatal("expected at least the terminal event")
	}
	last := ev[len(ev)-1]
	if !last.Terminal || last.Status != "failed" {
		t.Fatalf("last event should be terminal failed, got %+v", last)
	}
}

// A nil EventBus is a no-op (executor runs normally).
func TestDAGExecutor_NilEventBusIsNoOp(t *testing.T) {
	d := &DAGExecutor{} // no EventBus
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a"}}}
	if _, err := d.Execute(context.Background(), plan, nil, okStep("ok", nil)); err != nil {
		t.Fatalf("Execute with nil bus: %v", err)
	}
}
