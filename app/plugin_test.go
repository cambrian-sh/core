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
	name     string
	selector domain.ResourceSelector
	lifecycle *Lifecycle
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
	return nil
}

// A plugin's ResourceSelector folds into the effective Options (Tier-1 replace-one).
func TestApplyPlugins_FoldsResourceSelector(t *testing.T) {
	sel := stubSelector{id: "custom"}
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", selector: sel}}}
	got, _, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if got.ResourceSelector != domain.ResourceSelector(sel) {
		t.Fatalf("ResourceSelector not folded from plugin: %#v", got.ResourceSelector)
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
	got, _, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if got.ResourceSelector != domain.ResourceSelector(direct) {
		t.Fatalf("direct selector should win, got %#v", got.ResourceSelector)
	}
}

// Two plugins owning the same replace-one point is a hard error (fail-closed).
func TestApplyPlugins_SelectorConflictErrors(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "p1", selector: stubSelector{id: "a"}},
		&testPlugin{name: "p2", selector: stubSelector{id: "b"}},
	}}
	if _, _, err := applyPlugins(opts); err == nil {
		t.Fatal("expected a conflict error when two plugins register a ResourceSelector")
	}
}

// Lifecycles registered by plugins are returned in registration order.
func TestApplyPlugins_CollectsLifecycles(t *testing.T) {
	started := ""
	lc := Lifecycle{Name: "lc1", Start: func(context.Context) { started = "yes" }}
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", lifecycle: &lc}}}
	_, lifecycles, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(lifecycles) != 1 || lifecycles[0].Name != "lc1" {
		t.Fatalf("expected 1 lifecycle 'lc1', got %v", lifecycles)
	}
	lifecycles[0].Start(context.Background())
	if started != "yes" {
		t.Fatal("lifecycle Start not wired through")
	}
}

// No plugins ⇒ Options unchanged, no lifecycles (OSS default path).
func TestApplyPlugins_Empty(t *testing.T) {
	got, lifecycles, err := applyPlugins(Options{})
	if err != nil || lifecycles != nil || got.ResourceSelector != nil {
		t.Fatalf("empty plugins should be a no-op, got err=%v lifecycles=%v", err, lifecycles)
	}
}
