package config

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// execution.hooks scope fields (T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or)
// and guarded arguments (T-20261106-post-model-hooks-do-not-guard-control-verb).

func TestHooksConfig_ValidatesScopeFields(t *testing.T) {
	t.Parallel()
	redact := func(mut func(*HookConfig)) HooksConfig {
		e := HookConfig{Name: "r", Kind: HookKindRedactOutput, Pattern: "x", MayRewrite: true, Points: []string{"post_model"}}
		mut(&e)
		return HooksConfig{Entries: []HookConfig{e}}
	}
	ok := redact(func(e *HookConfig) { e.Agents = []string{"research*"}; e.Purposes = []string{"agent_step", "plan*"} })
	if errs := ok.validate(); len(errs) != 0 {
		t.Fatalf("valid scope refused: %v", errs)
	}
	for name, c := range map[string]struct {
		cfg  HooksConfig
		want string
	}{
		"empty agent":   {redact(func(e *HookConfig) { e.Agents = []string{" "} }), ".agents must not contain an empty glob"},
		"empty purpose": {redact(func(e *HookConfig) { e.Purposes = []string{""} }), ".purposes must not contain an empty glob"},
		// redact_output's natural point is post_tool: purposes there scope nothing.
		"purposes on natural post_tool": {redact(func(e *HookConfig) { e.Points = nil; e.Purposes = []string{"planner"} }),
			"must attach to pre_model or post_model"},
		"purposes on a tool hook": {HooksConfig{Entries: []HookConfig{{Name: "d", Kind: HookKindDenyArgsRegex, Pattern: "x",
			Purposes: []string{"planner"}}}}, "must attach to pre_model or post_model"},
	} {
		errs := c.cfg.validate()
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: errs = %v (want %q)", name, errs, c.want)
		}
	}
	// agents is valid on a tool hook (it applies at every point).
	tool := HooksConfig{Entries: []HookConfig{{Name: "a", Kind: HookKindRequireApproval, Tools: []string{"*"}, Agents: []string{"coder"}}}}
	if errs := tool.validate(); len(errs) != 0 {
		t.Fatalf("agents on a tool hook refused: %v", errs)
	}
}

func TestHooksConfig_GuardedArguments(t *testing.T) {
	t.Parallel()
	def := HooksConfig{}.GuardedArgumentList()
	want := map[domain.GuardedArg]bool{
		{Tool: "yield_subgoal", Arg: "intent"}: true, {Tool: "delegate_to_planner", Arg: "task"}: true,
		{Tool: "propose_memory_note", Arg: "content"}: true, {Tool: "propose_skill", Arg: "skill_md"}: true,
	}
	if len(def) != len(want) {
		t.Fatalf("defaults = %+v", def)
	}
	for _, g := range def {
		if !want[g] {
			t.Errorf("unexpected default %+v", g)
		}
	}
	custom := HooksConfig{GuardedArguments: []GuardedArgumentConfig{{Tool: "respond", Arg: "body"}}}
	if got := custom.GuardedArgumentList(); len(got) != 1 || got[0] != (domain.GuardedArg{Tool: "respond", Arg: "body"}) {
		t.Fatalf("a configured list replaces the defaults: %+v", got)
	}
	if errs := custom.validate(); len(errs) != 0 {
		t.Fatalf("valid list refused: %v", errs)
	}
	for name, c := range map[string]struct {
		cfg  HooksConfig
		want string
	}{
		"missing arg":  {HooksConfig{GuardedArguments: []GuardedArgumentConfig{{Tool: "x"}}}, "tool and arg are both required"},
		"missing tool": {HooksConfig{GuardedArguments: []GuardedArgumentConfig{{Arg: "x"}}}, "tool and arg are both required"},
		"duplicate": {HooksConfig{GuardedArguments: []GuardedArgumentConfig{{Tool: "a", Arg: "b"}, {Tool: "a", Arg: "b"}}},
			"listed twice"},
	} {
		errs := c.cfg.validate()
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: errs = %v (want %q)", name, errs, c.want)
		}
	}
}

// "thought" (in-kernel thought steps) is a purpose label like any other; a
// deliverables scope is [agent_step, thought]
// (T-20261106-thought-steps-call-the-model-under-the-planner).
func TestHooksConfig_AcceptsTheThoughtPurpose(t *testing.T) {
	t.Parallel()
	c := HooksConfig{Entries: []HookConfig{{Name: "e2e-redact-answer", Kind: HookKindRedactOutput, Pattern: "x",
		MayRewrite: true, Points: []string{"post_model"}, Purposes: []string{"agent_step", "thought"}}}}
	if errs := c.validate(); len(errs) != 0 {
		t.Fatalf("purposes [agent_step, thought] refused: %v", errs)
	}
}
