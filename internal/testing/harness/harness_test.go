package harness_test

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/testing/harness"
)

func TestSystemHarness_ExecutePlan_PassesObserver(t *testing.T) {
	h := harness.New(harness.Config{})

	h.AddResponse("step completed successfully")

	_, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if len(events) == 0 {
		t.Error("expected at least one TaskEvent")
	}
	if len(h.Observer.TaskCompletedEvents) == 0 {
		t.Error("observer not called")
	}
}

func TestSystemHarness_ExecutePlan_Within100ms(t *testing.T) {
	h := harness.New(harness.Config{})
	h.AddResponse("fast")

	start := time.Now()
	_, _, err := h.ExecutePlan(t.Context())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("ExecutePlan took %v, want <100ms", elapsed)
	}
}

func TestCapturingObserver_RecordsAllMethods(t *testing.T) {
	obs := &harness.CapturingTelemetryObserver{}
	obs.OnSessionEvicted("agent-1")
	obs.OnConwipWait(50)
	obs.OnAuctionNoWinner("task-1")
	obs.OnSchemaMismatch("agent-1", "missing-field")

	if len(obs.EvictedAgentIDs) != 1 {
		t.Errorf("EvictedAgentIDs = %d, want 1", len(obs.EvictedAgentIDs))
	}
	if len(obs.ConwipWaits) != 1 || obs.ConwipWaits[0] != 50 {
		t.Errorf("ConwipWaits = %v, want [50]", obs.ConwipWaits)
	}
	if len(obs.AuctionNoWinners) != 1 {
		t.Errorf("AuctionNoWinners = %d, want 1", len(obs.AuctionNoWinners))
	}
	if len(obs.SchemaMismatches) != 1 {
		t.Errorf("SchemaMismatches = %d, want 1", len(obs.SchemaMismatches))
	}
}
