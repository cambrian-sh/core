package domain

import (
	"context"
	"testing"
)

// denyTool is a decision point that refuses one named tool.
type denyTool struct {
	AllowAllAuthorizer
	deny string
}

func (d denyTool) Authorize(_ context.Context, req AccessRequest) AccessDecision {
	if req.Resource.Kind == KindTool && req.Resource.ID == d.deny {
		return AccessDecision{
			Allowed: false, Reason: ReasonForbiddenTag, Detail: "secrets",
			Resource: req.Resource, Principal: req.Principal,
		}
	}
	return AccessDecision{Allowed: true, Reason: ReasonAllowed, Resource: req.Resource}
}

func clipExecutor(a Authorizer) *ToolExecutor {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "rotate_keys", ClassificationTags: []string{"secrets"}, Dangerous: true})
	reg.Register(SystemTool{Name: "read_runbook", ClassificationTags: []string{"public_kb"}})
	return &ToolExecutor{
		Registry: reg,
		Grants:   NewInMemoryGrantsStore(),
		Overlay:  NewRunGrantOverlay(),
		Authz:    a,
		Handler:  handlerFunc(func(context.Context, ToolCall) ([]byte, error) { return []byte(`{"ok":true}`), nil }),
	}
}

// ADR-0085 §5.4 escalation test, the one the whole phase exists for: a principal
// denied tool T, loading a skill that grants T, must NOT obtain T — the run
// continues and a skill_grant_clipped decision is emitted.
func TestSkillGrants_ClippedNotUnioned(t *testing.T) {
	e := clipExecutor(denyTool{deny: "rotate_keys"})
	ctx := context.Background()

	granted, clipped := e.ConferSkillGrants(ctx, "run1", "support",
		[]string{"read_runbook", "rotate_keys"})

	// The rest of the skill activates — denying the whole skill would make the
	// system feel broken.
	if len(granted) != 1 || granted[0] != "read_runbook" {
		t.Fatalf("the permitted grant must survive, got %v", granted)
	}
	if len(clipped) != 1 {
		t.Fatalf("expected exactly one clip, got %d", len(clipped))
	}
	c := clipped[0]
	if c.Reason != ReasonSkillGrantClipped {
		t.Errorf("Reason = %q, want %q", c.Reason, ReasonSkillGrantClipped)
	}
	if c.Resource.ID != "rotate_keys" {
		t.Errorf("the clip must name the tool, got %+v", c.Resource)
	}
	if c.Detail == "" {
		t.Errorf("the clip must say why")
	}

	// And the security property itself: the tool is NOT callable afterwards.
	if _, ok := e.grantFor(ctx, "support", "rotate_keys", "run1"); ok {
		t.Fatalf("a clipped tool must not be grantable — this is the escalation path")
	}
	if _, ok := e.grantFor(ctx, "support", "read_runbook", "run1"); !ok {
		t.Errorf("the permitted tool should be grantable for the run")
	}
	if resp := e.Execute(ctx, ToolCallRequest{AgentID: "support", ToolName: "rotate_keys", SessionTokenID: "run1"}); !resp.Denied {
		t.Errorf("executing a clipped tool must be denied, got %+v", resp)
	}
}

// D6 is preserved: an operator-authored skill may still confer a tool the agent
// has no STATIC grant for. Policy is the ceiling; the static grant list is not.
func TestSkillGrants_StillWidenBeyondStaticGrants(t *testing.T) {
	e := clipExecutor(AllowAllAuthorizer{}) // no policy ⇒ nothing to clip
	ctx := context.Background()

	if _, ok := e.grantFor(ctx, "support", "rotate_keys", "run1"); ok {
		t.Fatal("precondition: the agent holds no static grant")
	}
	granted, clipped := e.ConferSkillGrants(ctx, "run1", "support", []string{"rotate_keys"})
	if len(clipped) != 0 || len(granted) != 1 {
		t.Fatalf("an unscoped deployment must not clip an operator-authored skill: granted=%v clipped=%v", granted, clipped)
	}
	if _, ok := e.grantFor(ctx, "support", "rotate_keys", "run1"); !ok {
		t.Errorf("ADR-0046 D6 widening must survive: a skill may confer a tool the agent lacks statically")
	}
}

// A skill naming a tool that does not exist is clipped rather than silently
// activating a name the registry will later reject as "unknown tool" — the two
// look identical to a caller otherwise.
func TestSkillGrants_UnknownToolIsClipped(t *testing.T) {
	e := clipExecutor(AllowAllAuthorizer{})
	granted, clipped := e.ConferSkillGrants(context.Background(), "run1", "a", []string{"ghost_tool"})
	if len(granted) != 0 || len(clipped) != 1 {
		t.Fatalf("granted=%v clipped=%v", granted, clipped)
	}
	if clipped[0].Resource.ID != "ghost_tool" {
		t.Errorf("the clip must name the missing tool, got %+v", clipped[0].Resource)
	}
}

// ADR-0051 D6: a restricted principal's hard ceiling outranks a skill. Otherwise
// loading a skill would be the way around the Scout's confinement, which is
// exactly the shape of escalation this phase closes.
func TestSkillGrants_RestrictedCeilingOutranksTheSkill(t *testing.T) {
	e := clipExecutor(AllowAllAuthorizer{})
	e.RestrictedTools = map[string]map[string]bool{
		"scout": {"read_runbook": true},
	}
	granted, clipped := e.ConferSkillGrants(context.Background(), "run1", "scout",
		[]string{"read_runbook", "rotate_keys"})

	if len(granted) != 1 || granted[0] != "read_runbook" {
		t.Fatalf("granted = %v, want only the allowlisted tool", granted)
	}
	if len(clipped) != 1 || clipped[0].Resource.ID != "rotate_keys" {
		t.Fatalf("the out-of-ceiling tool must be clipped, got %+v", clipped)
	}
	if clipped[0].Reason != ReasonSkillGrantClipped {
		t.Errorf("Reason = %q, want %q", clipped[0].Reason, ReasonSkillGrantClipped)
	}
}

// Loading a skill can only ever narrow or maintain privilege. Expressed as a
// property: for any skill grant list, the conferred set is a SUBSET of it, and
// nothing outside the list ever becomes grantable.
func TestSkillGrants_NeverWidenBeyondWhatTheSkillNamed(t *testing.T) {
	e := clipExecutor(denyTool{deny: "rotate_keys"})
	ctx := context.Background()

	granted, _ := e.ConferSkillGrants(ctx, "run1", "support", []string{"read_runbook"})
	inList := map[string]bool{"read_runbook": true}
	for _, g := range granted {
		if !inList[g] {
			t.Fatalf("conferred %q which the skill never named", g)
		}
	}
	// A tool the skill did not mention stays ungranted.
	if _, ok := e.grantFor(ctx, "support", "rotate_keys", "run1"); ok {
		t.Fatalf("loading a skill must not confer anything it did not name")
	}
}
