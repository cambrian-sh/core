package app

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/hooks"
)

// T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or: the plugin API carries
// a hook's static scope (HookSpec.Agents / Purposes) into the one chain, and the
// boot warning about suppressed live deltas follows the scope.

type scopedPluginHook struct {
	spec  domain.HookSpec
	calls *int
}

func (h scopedPluginHook) Spec() domain.HookSpec { return h.spec }
func (h scopedPluginHook) Run(context.Context, domain.HookEvent) (domain.HookResult, error) {
	*h.calls++
	return domain.HookResult{Verdict: domain.HookDeny, Reason: "scoped"}, nil
}

func TestHooks_PluginHookScopeReachesTheChain(t *testing.T) {
	calls := 0
	h := scopedPluginHook{spec: domain.HookSpec{Name: "plugin-scoped", Points: []domain.HookPoint{domain.HookPostModel},
		Agents: []string{"research*"}, Purposes: []string{"agent_step"}, Source: "plugin:test"}, calls: &calls}
	composed, err := applyPlugins(Options{Plugins: []Plugin{&hookPlugin{hook: h, obs: &hookRecorder{}}}})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := hooks.NewChain(config.HooksConfig{}, composed.opts.Hooks, composed.opts.HookObserver, nil)
	if err != nil {
		t.Fatal(err)
	}
	planner := domain.HookEvent{Point: domain.HookPostModel, Model: &domain.HookModelCall{Purpose: "planner"}}
	if out := chain.Run(context.Background(), planner); out.Verdict != domain.HookAllow || calls != 0 {
		t.Fatalf("planner call: %+v calls=%d", out, calls)
	}
	step := domain.HookEvent{Point: domain.HookPostModel, AgentID: "researcher", Model: &domain.HookModelCall{Purpose: "agent_step"}}
	if out := chain.Run(context.Background(), step); out.Verdict != domain.HookDeny || calls != 1 {
		t.Fatalf("researcher step: %+v calls=%d", out, calls)
	}
	// A misdeclared plugin scope fails the boot like any misdeclared hook.
	bad := scopedPluginHook{spec: domain.HookSpec{Name: "bad", Points: []domain.HookPoint{domain.HookPreTool},
		Purposes: []string{"planner"}}, calls: &calls}
	if _, err := hooks.NewChain(config.HooksConfig{}, []domain.Hook{bad}, nil, nil); err == nil {
		t.Fatal("purposes on a tool-only plugin hook accepted")
	}
}

func TestStepOutputHookWarning_FollowsScope(t *testing.T) {
	chain := func(hs ...domain.HookSpec) *domain.HookChain {
		t.Helper()
		calls := 0
		var list []domain.Hook
		for _, s := range hs {
			list = append(list, scopedPluginHook{spec: s, calls: &calls})
		}
		c, err := domain.NewHookChain(list, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	pm := []domain.HookPoint{domain.HookPostModel}
	if msg, _ := stepOutputHookWarning(nil); msg != "" {
		t.Errorf("no hooks: %q", msg)
	}
	if msg, _ := stepOutputHookWarning(chain(domain.HookSpec{Name: "p", Points: pm, Purposes: []string{"planner"}})); msg != "" {
		t.Errorf("a planner-only hook suppresses nothing: %q", msg)
	}
	msg, attrs := stepOutputHookWarning(chain(domain.HookSpec{Name: "w", Points: pm, Agents: []string{"writer"}}))
	if !strings.Contains(msg, "scoped by agent") || len(attrs) != 4 {
		t.Errorf("agent-scoped: %q %v", msg, attrs)
	}
	msg, _ = stepOutputHookWarning(chain(domain.HookSpec{Name: "u", Points: pm},
		domain.HookSpec{Name: "w", Points: pm, Agents: []string{"writer"}}))
	if !strings.Contains(msg, "every agent step") {
		t.Errorf("unscoped: %q", msg)
	}
}
