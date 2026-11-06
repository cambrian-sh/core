package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/hooks"
)

type denyAllHook struct{ name string }

func (h denyAllHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: h.name, Points: []domain.HookPoint{domain.HookPreTool}, Source: "plugin:test"}
}
func (h denyAllHook) Run(context.Context, domain.HookEvent) (domain.HookResult, error) {
	return domain.HookResult{Verdict: domain.HookDeny, Reason: "plugin says no"}, nil
}

type hookRecorder struct{ seen []domain.HookInvocation }

func (r *hookRecorder) ObserveHook(inv domain.HookInvocation) { r.seen = append(r.seen, inv) }

type hookPlugin struct {
	hook domain.Hook
	obs  domain.HookObserver
}

func (p *hookPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: "hooks-test", DisplayName: "hooks-test", Version: "test"}
}

func (p *hookPlugin) Register(r *Registry) error {
	r.AddHook(p.hook)
	r.AddHookObserver(p.obs)
	return nil
}

// ADR-0139 D11: a plugin's hook reaches Options and joins the operator's config
// hooks in one chain; its observer sees the invocation.
func TestHooks_PluginHookReachesTheChain(t *testing.T) {
	rec := &hookRecorder{}
	composed, err := applyPlugins(Options{Plugins: []Plugin{&hookPlugin{hook: denyAllHook{"plugin-deny"}, obs: rec}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.HooksConfig{Entries: []config.HookConfig{
		{Name: "cfg-ask", Kind: config.HookKindRequireApproval, Tools: []string{"*"}, Priority: -1},
	}}
	chain, err := hooks.NewChain(cfg, composed.opts.Hooks, composed.opts.HookObserver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := chain.Names(); len(got) != 2 || got[0] != "cfg-ask" || got[1] != "plugin-deny" {
		t.Fatalf("chain = %v", got)
	}
	out := chain.Run(context.Background(), domain.HookEvent{Point: domain.HookPreTool, Tool: "web"})
	if out.Verdict != domain.HookDeny || out.By != "plugin-deny" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(rec.seen) != 2 {
		t.Fatalf("observer saw %d invocations, want 2", len(rec.seen))
	}
	// A plugin hook may not reuse a config hook's name.
	if _, err := hooks.NewChain(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "plugin-deny", Kind: config.HookKindRequireApproval, Tools: []string{"*"}},
	}}, composed.opts.Hooks, nil, nil); err == nil {
		t.Fatal("duplicate hook name across config and plugin accepted")
	}
}
