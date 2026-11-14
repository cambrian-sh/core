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
		"authz on features":        {Authz: AllowAllAuthorizer{}},
		"unrestricted on features": {Unrestricted: true},
		"guards on features":       {Guards: &RequestGuards{}},
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
	if te.InlineThreshold != 1<<20 || te.PreviewChars != 600 || te.Authz == nil {
		t.Fatalf("executor lost its wiring: %+v", te)
	}
	res := te.Execute(context.Background(), ToolCallRequest{AgentID: "w", ToolName: "read_file", ArgsJSON: []byte(`{}`)})
	if !res.Denied {
		t.Fatalf("ungranted call ran: %+v", res)
	}
}
