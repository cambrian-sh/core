package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// stubConsultant is a self-contained domain.SubstrateConsultant used purely as a
// VEHICLE for the Tier-1 replace-one registry tests below. These used to ride on
// domain.ResourceSelector; that seam was removed with the auction, and the registry
// semantics they cover (fold, direct-beats-plugin, two-owners-is-an-error) are
// general to every Tier-1 point, so they were re-pointed rather than deleted.
type stubConsultant struct{ id string }

func (s stubConsultant) Consult(_ context.Context, _, _ string) ([]domain.SubstrateCitation, error) {
	return nil, nil
}

// testPlugin registers whatever its fields tell it to.
type testPlugin struct {
	name        string
	caps        []string
	requires    []string
	consultant  domain.SubstrateConsultant
	lifecycle   *Lifecycle
	agent       *domain.AgentDefinition
	systemAgent *domain.AgentDefinition
	// order, when set, records Register/Build call order across a plugin set.
	order *[]string
}

// Build makes testPlugin a Builder so the D12 phase can be exercised.
func (p *testPlugin) Build(KernelServices) error {
	if p.order != nil {
		*p.order = append(*p.order, "build:"+p.name)
	}
	return nil
}

func (p *testPlugin) Manifest() PluginManifest {
	return PluginManifest{
		ID:           p.name,
		DisplayName:  p.name,
		Version:      "test",
		Requires:     p.requires,
		Capabilities: p.caps,
	}
}
func (p *testPlugin) Register(r *Registry) error {
	if p.order != nil {
		*p.order = append(*p.order, "register:"+p.name)
	}
	if p.consultant != nil {
		if err := r.SetSubstrateConsultant(p.name, p.consultant); err != nil {
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
func TestApplyPlugins_FoldsTier1Seam(t *testing.T) {
	c1 := stubConsultant{id: "custom"}
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", consultant: c1}}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if c.opts.SubstrateConsultant != domain.SubstrateConsultant(c1) {
		t.Fatalf("Tier-1 seam not folded from plugin: %#v", c.opts.SubstrateConsultant)
	}
}

// A directly-set Option wins over a plugin's (explicit beats plugin).
func TestApplyPlugins_DirectTier1Wins(t *testing.T) {
	direct := stubConsultant{id: "direct"}
	plugin := stubConsultant{id: "plugin"}
	opts := Options{
		SubstrateConsultant: direct,
		Plugins:             []Plugin{&testPlugin{name: "p1", consultant: plugin}},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if c.opts.SubstrateConsultant != domain.SubstrateConsultant(direct) {
		t.Fatalf("direct value should win, got %#v", c.opts.SubstrateConsultant)
	}
}

// Two plugins owning the same replace-one point is a hard error (fail-closed).
func TestApplyPlugins_Tier1ConflictErrors(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "p1", consultant: stubConsultant{id: "a"}},
		&testPlugin{name: "p2", consultant: stubConsultant{id: "b"}},
	}}
	if _, err := applyPlugins(opts); err == nil {
		t.Fatal("expected a conflict error when two plugins claim the same Tier-1 seam")
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
			got[d.Definition.ID] = d.Definition.System
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
	if err != nil || c.lifecycles != nil || c.agentSources != nil || c.opts.SubstrateConsultant != nil {
		t.Fatalf("empty plugins should be a no-op, got err=%v lifecycles=%v sources=%v", err, c.lifecycles, c.agentSources)
	}
}

// ── ADR-0082 ─────────────────────────────────────────────────────────────────

// D2: a plugin's manifest capabilities are collected for the handshake, deduped,
// and order-stable. The kernel never interprets them.
func TestApplyPlugins_CollectsManifestCapabilities(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "reactive", caps: []string{"watches-crud", "watch-schedule"}},
		&testPlugin{name: "other", caps: []string{"watch-schedule", "extra"}}, // dup + new
	}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	want := []string{"watches-crud", "watch-schedule", "extra"}
	if len(c.capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v", c.capabilities, want)
	}
	for i, w := range want {
		if c.capabilities[i] != w {
			t.Errorf("capability[%d] = %q, want %q (order must be stable)", i, c.capabilities[i], w)
		}
	}
}

// D10: dependencies register before their dependents, regardless of declaration order.
func TestApplyPlugins_RegistersInDependencyOrder(t *testing.T) {
	var order []string
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "chat", requires: []string{"reactive"}, order: &order},
		&testPlugin{name: "reactive", order: &order},
	}}
	if _, err := applyPlugins(opts); err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(order) != 2 || order[0] != "register:reactive" || order[1] != "register:chat" {
		t.Fatalf("register order = %v, want reactive before chat", order)
	}
}

// D10: an unmet dependency SKIPS the plugin rather than failing the boot — a paying
// customer must never get a kernel that refuses to start over a billing combination.
func TestApplyPlugins_UnmetDependencyIsNonFatal(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "chat", requires: []string{"reactive"}, caps: []string{"chat-crud"}},
	}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("unmet dependency must not be fatal, got: %v", err)
	}
	if len(c.capabilities) != 0 {
		t.Errorf("skipped plugin must contribute no capabilities, got %v", c.capabilities)
	}
	if len(c.statuses) != 1 || c.statuses[0].State != PluginStateDepsUnmet {
		t.Fatalf("statuses = %+v, want one deps_unmet", c.statuses)
	}
	if len(c.statuses[0].Missing) != 1 || c.statuses[0].Missing[0] != "reactive" {
		t.Errorf("Missing = %v, want [reactive]", c.statuses[0].Missing)
	}
}

// D10: skipping cascades — a dependent of a skipped plugin is itself unmet.
func TestApplyPlugins_UnmetDependencyCascades(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "chat", requires: []string{"reactive"}},
		&testPlugin{name: "chatui", requires: []string{"chat"}},
	}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	for _, st := range c.statuses {
		if st.State != PluginStateDepsUnmet {
			t.Errorf("plugin %q state = %q, want deps_unmet (cascade)", st.Manifest.ID, st.State)
		}
	}
	if len(c.built) != 0 {
		t.Errorf("no plugin should have registered, got %d", len(c.built))
	}
}

// A dependency cycle is a packaging bug, not a customer state — it must be an error.
func TestApplyPlugins_DependencyCycleIsError(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "a", requires: []string{"b"}},
		&testPlugin{name: "b", requires: []string{"a"}},
	}}
	if _, err := applyPlugins(opts); err == nil {
		t.Fatal("expected an error for a dependency cycle")
	}
}

// Duplicate plugin ids are a composition error.
func TestApplyPlugins_DuplicateIDIsError(t *testing.T) {
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "dup"},
		&testPlugin{name: "dup"},
	}}
	if _, err := applyPlugins(opts); err == nil {
		t.Fatal("expected an error for duplicate plugin ids")
	}
}

// D12: the Build phase runs in dependency order, after Register.
func TestBuildPlugins_RunsInDependencyOrder(t *testing.T) {
	var order []string
	opts := Options{Plugins: []Plugin{
		&testPlugin{name: "chat", requires: []string{"reactive"}, order: &order},
		&testPlugin{name: "reactive", order: &order},
	}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if err := buildPlugins(c.built, KernelServices{}); err != nil {
		t.Fatalf("buildPlugins: %v", err)
	}
	want := []string{"register:reactive", "register:chat", "build:reactive", "build:chat"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// ── ADR-0082 D3: entitlement chokepoint ──────────────────────────────────────

// stubEntitlements allows only the listed plugin ids; errPlugin forces a provider error.
type stubEntitlements struct {
	allow     map[string]bool
	expired   map[string]bool
	errPlugin string
}

func (s stubEntitlements) Entitled(m PluginManifest) (Entitlement, error) {
	if m.ID == s.errPlugin {
		return Entitlement{}, context.DeadlineExceeded
	}
	if s.expired[m.ID] {
		// Expired but inside grace: still allowed, flagged for operators.
		return Entitlement{Allowed: true, State: PluginStateExpired, Reason: "grace"}, nil
	}
	return Entitlement{Allowed: s.allow[m.ID]}, nil
}

// The governing property: an unentitled plugin contributes NOTHING — no capabilities, no
// lifecycles, no agent sources. "Not entitled" must behave exactly like "not installed".
func TestApplyPlugins_UnentitledPluginContributesNothing(t *testing.T) {
	lc := Lifecycle{Name: "should-not-appear"}
	opts := Options{
		Entitlements: stubEntitlements{allow: map[string]bool{"paid": false}},
		Plugins: []Plugin{&testPlugin{
			name:      "paid",
			caps:      []string{"paid-surface"},
			lifecycle: &lc,
			agent:     &domain.AgentDefinition{ID: "paid-agent"},
		}},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.capabilities) != 0 {
		t.Errorf("capabilities = %v, want none", c.capabilities)
	}
	if len(c.lifecycles) != 0 {
		t.Errorf("lifecycles = %v, want none", c.lifecycles)
	}
	if len(c.agentSources) != 0 {
		t.Errorf("agentSources = %d, want 0", len(c.agentSources))
	}
	if len(c.built) != 0 {
		t.Errorf("built = %d, want 0 (Register must never have run)", len(c.built))
	}
	if len(c.statuses) != 1 || c.statuses[0].State != PluginStateNotEntitled {
		t.Fatalf("statuses = %+v, want one not_entitled", c.statuses)
	}
}

// A nil provider is the OSS default: everything activates.
func TestApplyPlugins_NilEntitlementsAllowsAll(t *testing.T) {
	opts := Options{Plugins: []Plugin{&testPlugin{name: "p1", caps: []string{"c1"}}}}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.capabilities) != 1 || c.statuses[0].State != PluginStateActive {
		t.Fatalf("nil provider must allow all, got caps=%v statuses=%+v", c.capabilities, c.statuses)
	}
}

// A provider error fails CLOSED — a check that cannot answer must not accidentally grant.
func TestApplyPlugins_EntitlementErrorFailsClosed(t *testing.T) {
	opts := Options{
		Entitlements: stubEntitlements{allow: map[string]bool{"p1": true}, errPlugin: "p1"},
		Plugins:      []Plugin{&testPlugin{name: "p1", caps: []string{"c1"}}},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("a provider error must not be fatal, got: %v", err)
	}
	if len(c.capabilities) != 0 {
		t.Errorf("failed check must not grant, got caps %v", c.capabilities)
	}
	if c.statuses[0].State != PluginStateNotEntitled || c.statuses[0].Reason == "" {
		t.Errorf("want not_entitled with a reason, got %+v", c.statuses[0])
	}
}

// Entitlement interacts with dependencies: a plugin requiring an UNENTITLED plugin is
// deps_unmet — absence and non-payment are indistinguishable downstream.
func TestApplyPlugins_UnentitledDependencyCascades(t *testing.T) {
	opts := Options{
		Entitlements: stubEntitlements{allow: map[string]bool{"reactive": false, "chat": true}},
		Plugins: []Plugin{
			&testPlugin{name: "reactive", caps: []string{"watches-crud"}},
			&testPlugin{name: "chat", requires: []string{"reactive"}, caps: []string{"chat"}},
		},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.capabilities) != 0 {
		t.Errorf("capabilities = %v, want none", c.capabilities)
	}
	byID := map[string]PluginStatus{}
	for _, st := range c.statuses {
		byID[st.Manifest.ID] = st
	}
	if byID["reactive"].State != PluginStateNotEntitled {
		t.Errorf("reactive state = %q, want not_entitled", byID["reactive"].State)
	}
	if byID["chat"].State != PluginStateDepsUnmet {
		t.Errorf("chat state = %q, want deps_unmet", byID["chat"].State)
	}
}

// An expired-but-in-grace plugin still runs, and is reported as expired so a UI can prompt
// renewal. An air-gapped kernel must not lose a paid capability the instant a licence lapses.
func TestApplyPlugins_ExpiredInGraceStillActivates(t *testing.T) {
	opts := Options{
		Entitlements: stubEntitlements{expired: map[string]bool{"p1": true}},
		Plugins:      []Plugin{&testPlugin{name: "p1", caps: []string{"c1"}}},
	}
	c, err := applyPlugins(opts)
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(c.capabilities) != 1 {
		t.Errorf("expired-in-grace must still contribute, got caps %v", c.capabilities)
	}
	if c.statuses[0].State != PluginStateExpired {
		t.Errorf("state = %q, want expired", c.statuses[0].State)
	}
}
