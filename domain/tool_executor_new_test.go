package domain

import (
	"context"
	"errors"
	"testing"
)

// AGENTS.md §2.3 (T-20261113-domain-toolexecutor-has-no-constructor-its): the
// executor cannot be constructed without its required controls, and a control
// smuggled in through the features value is refused — one way in per control.
func TestNewToolExecutor_RefusesMissingOrMisplacedControls(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	grants := NewInMemoryGrantsStore()
	full := ToolExecutorControls{Registry: reg, Grants: grants, Authz: AllowAllAuthorizer{}}

	for name, c := range map[string]ToolExecutorControls{
		"no registry": {Grants: grants, Authz: AllowAllAuthorizer{}},
		"no grants":   {Registry: reg, Authz: AllowAllAuthorizer{}},
		"no authz":    {Registry: reg, Grants: grants},
	} {
		if _, err := NewToolExecutor(c, ToolExecutor{}); !errors.Is(err, ErrToolExecutorControl) {
			t.Errorf("%s: err = %v, want ErrToolExecutorControl", name, err)
		}
	}
	for name, f := range map[string]ToolExecutor{
		"authz on features":        {authz: AllowAllAuthorizer{}},
		"unrestricted on features": {unrestricted: true},
		"guards on features":       {guards: &RequestGuards{}},
	} {
		if _, err := NewToolExecutor(full, f); !errors.Is(err, ErrToolExecutorControl) {
			t.Errorf("%s: err = %v, want ErrToolExecutorControl", name, err)
		}
	}
}

// The constructed executor carries the controls and the features, and enforces:
// with no grant and no bypass a registered tool is denied.
func TestNewToolExecutor_EnforcesFromTheFirstCall(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file", Effects: []ToolEffect{EffectRead}})
	te, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}},
		ToolExecutor{InlineThreshold: 1 << 20, PreviewChars: 600})
	if err != nil {
		t.Fatal(err)
	}
	if te.InlineThreshold != 1<<20 || te.PreviewChars != 600 || te.authz == nil {
		t.Fatalf("executor lost its wiring: %+v", te)
	}
	res := te.Execute(context.Background(), ToolCallRequest{AgentID: "w", ToolName: "read_file", ArgsJSON: []byte(`{}`)})
	if !res.Denied {
		t.Fatalf("ungranted call ran: %+v", res)
	}
}

// The ADR-0139 D12 lease ceiling and the ADR-0150 subtask registry are
// constructor controls too (they used to be chained in after construction by
// app/composed_workers.go and app/subtasks.go): placed on features they are
// refused, passed as controls they bind from the first call — a composed lease
// cannot call a tool outside its composition even under tools_unrestricted.
func TestNewToolExecutor_LeaseControlsAreConstructorInjected(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file", Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: "write_file", Effects: []ToolEffect{EffectWrite}})
	base := ToolExecutorControls{Registry: reg, Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}}

	leases := NewComposedLeases()
	subs := NewSubtasks(0, nil)
	for name, f := range map[string]ToolExecutor{
		"lease ceiling on features": {leaseCeiling: leases},
		"subtasks on features":      {subtasks: subs},
	} {
		if _, err := NewToolExecutor(base, f); !errors.Is(err, ErrToolExecutorControl) {
			t.Errorf("%s: err = %v, want ErrToolExecutorControl", name, err)
		}
	}

	c := base
	c.Unrestricted = true
	c.LeaseCeiling = ChainLeaseCeilings(leases, subs)
	c.Subtasks = subs
	te, err := NewToolExecutor(c, ToolExecutor{InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if te.Subtasks() != subs {
		t.Fatal("the executor does not enforce the subtask registry it was constructed with")
	}
	leases.Bind("lease-1", WorkerComposition{Runtime: "rt", Tools: []string{"read_file"}})
	if !te.WithinLeaseCeiling("lease-1", "read_file") {
		t.Error("tool inside the composition refused")
	}
	if te.WithinLeaseCeiling("lease-1", "write_file") {
		t.Error("composed lease reached a tool outside its composition (ceiling not enforced)")
	}
	if !te.WithinLeaseCeiling("lease-unbound", "write_file") {
		t.Error("an unbound lease must be unaffected by the ceiling")
	}
}
