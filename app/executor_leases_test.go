package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// buildTestExecutor builds the executor the way bootstrapKernel does: the lease
// controls from config, then the one constructor (AGENTS.md §2.3).
func buildTestExecutor(t *testing.T, cfg config.ExecutionConfig) (*domain.ToolExecutor, executorLeases) {
	t.Helper()
	l := buildExecutorLeases(cfg, nil)
	te, err := domain.NewToolExecutor(l.controls(domain.ToolExecutorControls{
		Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore(), Authz: domain.AllowAllAuthorizer{},
	}), domain.ToolExecutor{Overlay: domain.NewRunGrantOverlay()})
	if err != nil {
		t.Fatal(err)
	}
	return te, l
}

func singleAgentComposed() config.ExecutionConfig {
	var cfg config.ExecutionConfig
	cfg.Workers.Composed = true
	cfg.Lane.Default = config.LaneSingleAgent
	return cfg
}

// T-20261113-domain-toolexecutor-has-no-constructor-its: the composed-lease
// ceiling and the subtask registry exist exactly when config turns their
// consumers on, and the executor is CONSTRUCTED enforcing them — a composition
// bound later is enforced without anything being attached to the executor.
func TestExecutorLeases_BuiltFromConfigAndEnforcedFromConstruction(t *testing.T) {
	if l := buildExecutorLeases(config.ExecutionConfig{}, nil); l.composed != nil || l.subtasks != nil {
		t.Fatalf("composed workers off: no lease sources expected, got %+v", l)
	}
	planner := singleAgentComposed()
	planner.Lane.Default = config.LanePlanner
	if l := buildExecutorLeases(planner, nil); l.composed == nil || l.subtasks != nil {
		t.Fatalf("planner lane: compositions only, got %+v", l)
	}
	off := false
	noSubtasks := singleAgentComposed()
	noSubtasks.Lane.Subtasks.Enabled = &off
	if l := buildExecutorLeases(noSubtasks, nil); l.subtasks != nil {
		t.Fatal("subtasks disabled: no subtask registry expected")
	}

	te, l := buildTestExecutor(t, singleAgentComposed())
	if l.composed == nil || l.subtasks == nil || te.Subtasks() != l.subtasks {
		t.Fatalf("single-agent lane with composed workers: executor not constructed with both lease sources (%+v)", l)
	}
	l.composed.Bind("lease-1", domain.WorkerComposition{Runtime: "rt", Tools: []string{"read_file"}})
	if te.WithinLeaseCeiling("lease-1", "write_file") || !te.WithinLeaseCeiling("lease-1", "read_file") {
		t.Fatal("the composition bound after construction is not the executor's lease ceiling")
	}
}

// wireSubtasks serves run_subtasks only over the registry the executor enforces;
// a registry the executor was not built with (the old attach-later shape) is
// refused rather than served unenforced.
func TestWireSubtasks_OnlyOverTheExecutorsOwnRegistry(t *testing.T) {
	cfg := singleAgentComposed()
	te, l := buildTestExecutor(t, cfg)
	srv := &subnetwork.Server{ToolExecutor: te, Compositions: l.composed, ComposedRuntimeID: "rt"}

	wireSubtasks(cfg, srv, domain.NewSubtasks(0, nil))
	if srv.Subtasks != nil || len(te.KernelTools) != 0 {
		t.Fatal("run_subtasks served over a registry the executor does not enforce")
	}
	wireSubtasks(cfg, srv, l.subtasks)
	if srv.Subtasks != l.subtasks || len(te.KernelTools) != 1 {
		t.Fatalf("run_subtasks not served over the executor's registry (server=%v tools=%d)", srv.Subtasks != nil, len(te.KernelTools))
	}
}
