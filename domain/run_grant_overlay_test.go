package domain

import (
	"context"
	"testing"
)

// The run-scoped overlay confers tools per session, scopes them to that session,
// and drops them on Clear (ephemeral). Nil/empty are safe no-ops.
func TestRunGrantOverlay(t *testing.T) {
	var nilO *RunGrantOverlay
	if nilO.Granted("s", "t") {
		t.Error("nil overlay must grant nothing")
	}
	nilO.Activate("s", []string{"t"}) // must not panic
	nilO.Clear("s")

	o := NewRunGrantOverlay()
	o.Activate("run1", []string{"execute_command", "read_file"})
	if !o.Granted("run1", "execute_command") {
		t.Error("conferred tool should be granted within the run")
	}
	if o.Granted("run2", "execute_command") {
		t.Error("a conferred grant must be scoped to its own run/session")
	}
	o.Activate("", []string{"x"})
	if o.Granted("", "x") {
		t.Error("empty session must grant nothing")
	}
	o.Clear("run1")
	if o.Granted("run1", "execute_command") {
		t.Error("Clear must drop conferred grants (ephemeral)")
	}
}

// grantFor honors a system-skill-conferred overlay grant (ADR-0046 D6): a tool the
// agent lacks statically becomes grantable once a skill confers it for the run,
// scoped to that run, and gone after Clear.
func TestGrantFor_SkillConferred(t *testing.T) {
	reg := toolRegFixture()
	grants := NewInMemoryGrantsStore() // agent1 has NO static grants
	overlay := NewRunGrantOverlay()
	exec := &ToolExecutor{Registry: reg, Grants: grants, Overlay: overlay}
	ctx := context.Background()

	if _, ok := exec.grantFor(ctx, "agent1", "execute_command", "run1"); ok {
		t.Fatal("ungranted tool with no overlay must be denied")
	}

	exec.ConferSkillGrants("run1", []string{"execute_command"})
	if _, ok := exec.grantFor(ctx, "agent1", "execute_command", "run1"); !ok {
		t.Error("a system skill should be able to confer a tool run-scoped")
	}
	if _, ok := exec.grantFor(ctx, "agent1", "execute_command", "other-run"); ok {
		t.Error("the conferred grant must not leak to another run/session")
	}

	overlay.Clear("run1")
	if _, ok := exec.grantFor(ctx, "agent1", "execute_command", "run1"); ok {
		t.Error("the conferred grant must vanish after Clear (ephemeral)")
	}
}
