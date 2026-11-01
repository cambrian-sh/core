package domain

import "testing"

// ADR-0046 D6 ephemerality: a grant a system skill conferred for a run is gone
// once the kernel ends that run (T-20261031-skill-conferred-run-grants-are-never-cleared).
func TestToolExecutor_EndRun_ClearsConferredGrants(t *testing.T) {
	e := &ToolExecutor{Overlay: NewRunGrantOverlay()}
	e.Overlay.Activate("lease-1", []string{"web_fetch"})
	e.Overlay.Activate("lease-2", []string{"web_fetch"})
	if !e.Overlay.Granted("lease-1", "web_fetch") {
		t.Fatal("precondition: the grant must be active for the run")
	}

	e.EndRun("lease-1")

	if e.Overlay.Granted("lease-1", "web_fetch") {
		t.Fatal("a conferred grant must not outlive its run")
	}
	if !e.Overlay.Granted("lease-2", "web_fetch") {
		t.Fatal("ending one run must not touch another run's grants")
	}

	// Nil-safe on both the executor and its overlay: the kernel calls it from a
	// defer that must never panic.
	var none *ToolExecutor
	none.EndRun("lease-1")
	(&ToolExecutor{}).EndRun("lease-1")
}
