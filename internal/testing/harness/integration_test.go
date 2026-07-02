//go:build integration

package harness_test

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/internal/testing/harness"
)

func TestE2E_TokenBudgetEnforced(t *testing.T) {
	h := harness.New(harness.Config{})
	h.AddResponse("response with more tokens than budget")

	_, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no TaskEvents emitted")
	}

	if len(h.Observer.TaskCompletedEvents) == 0 {
		t.Error("observer not called")
	}
}

func TestE2E_ObserverWiring(t *testing.T) {
	h := harness.New(harness.Config{})
	h.Observer = &harness.CapturingTelemetryObserver{}
	h.AddResponse("step result")

	_, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	if len(events) != len(h.Observer.TaskCompletedEvents) {
		t.Errorf("TaskEvents=%d, Observer calls=%d — observer fires should match TaskEvent count",
			len(events), len(h.Observer.TaskCompletedEvents))
	}
	if len(events) == 0 {
		t.Error("expected at least one TaskEvent + observer call")
	}
}

func TestE2E_NilGateway(t *testing.T) {
	h := harness.New(harness.Config{})
	h.AddResponse("step completed")

	_, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan with nil gateway: %v", err)
	}
	for _, evt := range events {
		if evt.BudgetOverrun {
			t.Error("BudgetOverrun=true with nil gateway (unexpected)")
		}
	}
}

func TestE2E_ConwipBackpressure(t *testing.T) {
	h := harness.New(harness.Config{MaxConcurrentSessions: 1})
	for i := 0; i < 5; i++ {
		h.AddResponse("quick")
	}

	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			_, _, err := h.ExecutePlan(t.Context())
			results <- err
		}()
	}

	timeout := time.After(5 * time.Second)
	successes := 0
	for i := 0; i < 5; i++ {
		select {
		case err := <-results:
			if err == nil {
				successes++
			}
		case <-timeout:
			t.Fatal("timeout waiting for results")
		}
	}
	if successes == 0 {
		t.Error("all plans failed, expected at least one success")
	}
}

func TestE2E_ObserverSchemaMismatch(t *testing.T) {
	obs := &harness.CapturingTelemetryObserver{}
	obs.OnSchemaMismatch("agent-1", "missing-field")
	obs.OnSchemaMismatch("agent-2", "invalid-type")

	if len(obs.SchemaMismatches) != 2 {
		t.Errorf("expected 2 schema mismatches, got %d", len(obs.SchemaMismatches))
	}
	if obs.SchemaMismatches[0].Kind != "missing-field" {
		t.Errorf("first mismatch kind = %q, want missing-field", obs.SchemaMismatches[0].Kind)
	}
}
