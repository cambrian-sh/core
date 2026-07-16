package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// stubSelector is a self-contained domain.ResourceSelector for the registry tests.
type stubSelector struct{ id string }

func (s stubSelector) Select(_ context.Context, _ domain.Intent, _ []domain.AgentDefinition) (domain.Selection, error) {
	return domain.Selection{}, nil
}

// testPlugin registers whatever its fields tell it to.
type testPlugin struct {
	name        string
	selector    domain.ResourceSelector
	lifecycle   *Lifecycle
	agent       *domain.AgentDefinition
	systemAgent *domain.AgentDefinition
}

func (p *testPlugin) Name() string { return p.name }
func (p *testPlugin) Register(r *Registry) error {
	if p.selector != nil {
		if err := r.SetResourceSelector(p.name, p.selector); err != nil {
			return err
		}
	}
	if p.lifecycle != nil {
		r.AddLifecycle(*p.lifecycle)
	}
	if p.agent != nil {
		r.AddAgent(*p.agent)
	}
	if p.systemAgent != nil {
		r.AddSystemAgent(*p.systemAgent)
	}
	return nil
}

// A plugin's ResourceSelector folds into the effective Options (Tier-1 replace-one).
func TestApplyPlugins_FoldsResourceSelector(t *testing.T) {
	sel := stubSelector{id: "custom"}
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", selector: sel}}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if c.opts.ResourceSelector != domain.ResourceSelector(sel) {
		t.Fatalf("ResourceSelector not folded from plugin: %#v", c.opts.ResourceSelector)
	}
}

// A directly-set Options.ResourceSelector wins over a plugin's (explicit beats plugin).
func TestApplyPlugins_DirectSelectorWins(t *testing.T) {
	direct := stubSelector{id: "direct"}
	plugin := stubSelector{id: "plugin"}
	opts := Options{
		ResourceSelector: direct,
		Plugins:          []Plugin{&testPlugin{name: "p1", selector: plugin}},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if c.opts.ResourceSelector != domain.ResourceSelector(direct) {
		t.Fatalf("direct selector should win, got %#v", c.opts.ResourceSelector)
	}
}

// Two plugins owning the same replace-one point is a hard error (fail-closed).
func TestApplyPlugins_SelectorConflictErrors(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "p1", selector: stubSelector{id: "a"}},
		&testPlugin{name: "p2", selector: stubSelector{id: "b"}},
	}}
	if _, err := applyPlugins(opts); err == nil {
		t.Fatal("expected a conflict error when two plugins register a ResourceSelector")
	}
}

// Lifecycles registered by plugins are returned in registration order.
func TestApplyPlugins_CollectsLifecycles(t *testing.T) {
	started := ""
	lc := Lifecycle{Name: "lc1", Start: func(context.Context) { started = "yes" }}
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", lifecycle: &lc}}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.lifecycles) != 1 || c.lifecycles[0].Name != "lc1" {
		t.Fatalf("expected 1 lifecycle 'lc1', got %v", c.lifecycles)
	}
	c.lifecycles[0].Start(context.Background())
	if started != "yes" {
		t.Fatal("lifecycle Start not wired through")
	}
}

// AddAgent contributes a regular agent source; AddSystemAgent forces System=true and
// AddAgent forces System=false (the privilege boundary is enforced at the API).
func TestApplyPlugins_AgentSources(t *testing.T) {
	opts := Options{Plugins: []Plugin{&testPlugin{
		name:        "p1",
		agent:       &domain.AgentDefinition{ID: "regular", System: true}, // AddAgent must strip this
		systemAgent: &domain.AgentDefinition{ID: "privileged"},
	}}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.agentSources) != 2 {
		t.Fatalf("expected 2 agent sources, got %d", len(c.agentSources))
	}
	got := map[string]bool{}
	for _, src := range c.agentSources {
		defs, _ := src.DiscoverAgents(context.Background())
		for _, d := range defs {
			got[d.ID] = d.System
		}
	}
	if got["regular"] != false {
		t.Errorf("AddAgent must force System=false, got true for 'regular'")
	}
	if got["privileged"] != true {
		t.Errorf("AddSystemAgent must set System=true, got false for 'privileged'")
	}
}

// No plugins ⇒ Options unchanged, no lifecycles/sources (OSS default path).
func TestApplyPlugins_Empty(t *testing.T) {
	c, err := applyPlugins(Options{})
	if err != nil || c.lifecycles != nil || c.agentSources != nil || c.opts.ResourceSelector != nil {
		t.Fatalf("empty plugins should be a no-op, got err=%v lifecycles=%v sources=%v", err, c.lifecycles, c.agentSources)
	}
}
