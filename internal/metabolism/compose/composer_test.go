package compose

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D12: the composer gives a step exactly the tools inside its professions'
// allowlists AND the person's scope (ADR-0145 D14: a step's declared effects no
// longer narrow them), and the professions' skills; binds that to the step lease;
// and records it in the ledger.

type fakeAgents struct {
	defs      map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func (f fakeAgents) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	d, ok := f.defs[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return &d, nil
}

func (f fakeAgents) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	m, ok := f.manifests[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return m, nil
}

type fakeTools []domain.SystemTool

func (f fakeTools) AllTools() []domain.SystemTool { return f }

type fakeLedger struct{ recs []domain.LedgerRecord }

func (l *fakeLedger) Record(_ context.Context, r domain.LedgerRecord) (domain.LedgerEntry, error) {
	l.recs = append(l.recs, r)
	return domain.LedgerEntry{ID: "led_x", Kind: r.Kind}, nil
}

type fakeSkills map[string]bool

func (f fakeSkills) Get(n string) (domain.Skill, bool) { return domain.Skill{Name: n}, f[n] }

func fixture(t *testing.T) (*Composer, *fakeLedger) {
	t.Helper()
	agents := fakeAgents{
		defs: map[string]domain.AgentDefinition{
			"composed_worker": {ID: "composed_worker", System: true},
		},
		manifests: map[string]*domain.AgentManifest{
			"researcher": {Capabilities: []string{"research"}, Skills: []string{"web-research", "summarise"},
				ToolAllowlist: []string{"mcp:web/*", "mcp:fs/read_*"}},
			"assistant": {Capabilities: []string{"general_purpose", "file_write"}, Skills: []string{"file-report"},
				ToolAllowlist: []string{"mcp:fs/*"}},
			"analyst": {Capabilities: []string{"research", "analysis"}, Skills: []string{"exploratory-analysis", "ghost-skill"},
				ToolAllowlist: []string{"mcp:sandbox/*"}},
		},
	}
	tools := fakeTools{
		{Name: "mcp:web/search", Effects: []domain.ToolEffect{domain.EffectRead}},
		{Name: "mcp:fs/read_file", Effects: []domain.ToolEffect{domain.EffectRead}},
		{Name: "mcp:fs/write_file", Effects: []domain.ToolEffect{domain.EffectWrite}},
		{Name: "mcp:sandbox/run", Effects: []domain.ToolEffect{domain.EffectRead}},
		{Name: "mcp:fs/read_secret", Effects: []domain.ToolEffect{domain.EffectRead}},
		{Name: "mcp:fs/read_with_path", Effects: []domain.ToolEffect{domain.EffectRead}, PathArgs: []string{"path"}},
		{Name: "mcp:mail/send", Effects: []domain.ToolEffect{domain.EffectWrite}},
	}
	led := &fakeLedger{}
	c := &Composer{
		RuntimeID: "composed_worker", Agents: agents, Manifests: agents, Tools: tools,
		Scope: func(_ context.Context, _ string, t domain.SystemTool) bool {
			return t.Name != "mcp:fs/read_secret" // the person may not read this store
		},
		Skills:    fakeSkills{"web-research": true, "summarise": true, "file-report": true, "exploratory-analysis": true},
		MaxSkills: 8,
		Leases:    domain.NewComposedLeases(),
		Overlay:   domain.NewRunGrantOverlay(),
		Ledger:    led,
	}
	return c, led
}

func profileAgent(id string) domain.AgentDefinition {
	return domain.AgentDefinition{ID: id, Profile: id}
}

func stepHandoff(lease string) *domain.Handoff {
	deps, _ := domain.EncodeStepDependencies([]domain.StepDependency{{Step: 0, Status: domain.StepDependencyCompleted, CID: "led_upstream"}})
	return &domain.Handoff{Context: map[string]string{
		HandoffLeaseKey: lease, "_step_index": "1", domain.HandoffKeyStepDependencies: deps,
	}}
}

func TestCompose_StepGetsExactlyTheAllowedToolsAndSelectedSkills(t *testing.T) {
	c, led := fixture(t)
	task := &domain.DispatchTask{ID: "task-1", Description: "find the 2025 filing deadline",
		RequiredCapabilities: []string{"research"}, RequiredEffects: []string{"read"}}
	scored := []domain.ScoredCandidate{{Agent: profileAgent("researcher"), Score: 0.9},
		{Agent: profileAgent("analyst"), Score: 0.5}, {Agent: profileAgent("assistant"), Score: 0.4}}
	in := stepHandoff("lease-1")
	ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{RunID: "run-1", StepIndex: -1})

	rt, comp, ok := c.Compose(ctx, task, scored[0], scored, in)
	if !ok || rt.Agent.ID != "composed_worker" {
		t.Fatalf("not composed onto the runtime: ok=%v rt=%v", ok, rt.Agent.ID)
	}
	// Professions: the profile candidates declaring the step's capability (not assistant).
	if want := []string{"analyst", "researcher"}; !reflect.DeepEqual(comp.Professions, want) {
		t.Fatalf("professions = %v, want %v", comp.Professions, want)
	}
	// Skills: theirs, served by the kernel (ghost-skill dropped), sorted.
	if want := []string{"exploratory-analysis", "summarise", "web-research"}; !reflect.DeepEqual(comp.Skills, want) {
		t.Fatalf("skills = %v, want %v", comp.Skills, want)
	}
	// Tools: allowlists ∩ scope, no resource-arg tool, no tool outside every
	// allowlist (mail/send, fs/write_file).
	if want := []string{"mcp:fs/read_file", "mcp:sandbox/run", "mcp:web/search"}; !reflect.DeepEqual(comp.Tools, want) {
		t.Fatalf("tools = %v, want %v", comp.Tools, want)
	}
	// Bound to the lease (the kernel's enforcement point) and granted run-scoped.
	if got, bound := c.Leases.AllowsLease("lease-1", "mcp:fs/write_file"); !bound || got {
		t.Fatalf("write_file inside the lease ceiling: inside=%v bound=%v", got, bound)
	}
	if !c.Overlay.Granted("lease-1", "mcp:web/search") || c.Overlay.Granted("lease-1", "mcp:fs/write_file") {
		t.Fatal("run-scoped grants are not exactly the composed tools")
	}
	// Stamped on the handoff as data, with the brief and the input ledger reference.
	got, okDec := domain.DecodeWorkerComposition(in.Context[domain.HandoffKeyComposition])
	if !okDec || got.Brief != task.Description || !reflect.DeepEqual(got.LedgerRefs, []string{"led_upstream"}) || got.StepIndex != 1 {
		t.Fatalf("handoff composition = %+v", got)
	}
	if in.Context[HandoffKeyCompositionID] != comp.ID() {
		t.Fatal("composition id not stamped")
	}
	// Recorded in the ledger, derived from its inputs.
	if len(led.recs) != 1 || led.recs[0].Kind != domain.LedgerComposition || led.recs[0].Provenance.StepIndex != 1 ||
		led.recs[0].Provenance.RunID != "run-1" || !reflect.DeepEqual(led.recs[0].DerivedFrom, []string{"led_upstream"}) {
		t.Fatalf("ledger record = %+v", led.recs)
	}
	var body domain.WorkerComposition
	if err := json.Unmarshal(led.recs[0].Body, &body); err != nil || body.ID() != comp.ID() {
		t.Fatalf("recorded body is not the composition: %v", err)
	}
}

// ADR-0145 D14: the declared effects do not narrow the tools; the profession's
// allowlist and the person's scope do.
func TestCompose_WriteStepGetsItsWriteTool(t *testing.T) {
	c, _ := fixture(t)
	task := &domain.DispatchTask{ID: "t", Description: "save the report", RequiredCapabilities: []string{"file_write"},
		RequiredEffects: []string{"write"}}
	w := domain.ScoredCandidate{Agent: profileAgent("assistant")}
	_, comp, ok := c.Compose(context.Background(), task, w, []domain.ScoredCandidate{w}, stepHandoff("lease-w"))
	if want := []string{"mcp:fs/read_file", "mcp:fs/write_file"}; !ok || !reflect.DeepEqual(comp.Tools, want) {
		t.Fatalf("tools = %v (ok=%v), want %v", comp.Tools, ok, want)
	}
}

func TestCompose_NoCapabilities_UsesTheWinnerProfessionOnly(t *testing.T) {
	c, _ := fixture(t)
	task := &domain.DispatchTask{ID: "t", Description: "x"}
	scored := []domain.ScoredCandidate{{Agent: profileAgent("researcher")}, {Agent: profileAgent("assistant")}}
	_, comp, ok := c.Compose(context.Background(), task, scored[0], scored, stepHandoff("lease-n"))
	if !ok || !reflect.DeepEqual(comp.Professions, []string{"researcher"}) {
		t.Fatalf("composition = %+v", comp)
	}
}

func TestCompose_CodeOrServiceAgentIsNotComposed(t *testing.T) {
	c, led := fixture(t)
	w := domain.ScoredCandidate{Agent: domain.AgentDefinition{ID: "docling_agent"}} // no profile
	in := stepHandoff("lease-b")
	if _, _, ok := c.Compose(context.Background(), &domain.DispatchTask{ID: "t"}, w, []domain.ScoredCandidate{w}, in); ok {
		t.Fatal("a code/service agent was composed")
	}
	if _, bound := c.Leases.For("lease-b"); bound || len(led.recs) != 0 || in.Context[domain.HandoffKeyComposition] != "" {
		t.Fatal("a non-composed step left composition state behind")
	}
}

func TestCompose_RefusesWithoutALeaseOrRuntime(t *testing.T) {
	c, _ := fixture(t)
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	if _, _, ok := c.Compose(context.Background(), &domain.DispatchTask{ID: "t"}, w, nil, &domain.Handoff{Context: map[string]string{}}); ok {
		t.Fatal("composed without a lease to bind to")
	}
	c.RuntimeID = "missing"
	if _, _, ok := c.Compose(context.Background(), &domain.DispatchTask{ID: "t"}, w, nil, stepHandoff("lease-r")); ok {
		t.Fatal("composed onto an unregistered runtime")
	}
}

func TestCompose_DoesNotRecomposeALeaseThatAlreadyCarriesOne(t *testing.T) {
	c, led := fixture(t)
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	task := &domain.DispatchTask{ID: "t", Description: "x"}
	if _, _, ok := c.Compose(context.Background(), task, w, nil, stepHandoff("lease-y")); !ok {
		t.Fatal("first composition failed")
	}
	// A second dispatch on the same lease (a heal retry, a yield sub-goal) runs on
	// the composed runtime under the lease's EXISTING composition — never on the
	// selected profile agent (T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent).
	a := domain.ScoredCandidate{Agent: profileAgent("assistant")}
	in := stepHandoff("lease-y")
	rt, comp, ok := c.Compose(context.Background(), task, a, nil, in)
	if !ok || rt.Agent.ID != c.RuntimeID {
		t.Fatalf("a re-dispatch on a composed lease left the runtime: ok=%v agent=%q", ok, rt.Agent.ID)
	}
	if !reflect.DeepEqual(comp.Professions, []string{"researcher"}) || in.Context[domain.HandoffKeyComposition] == "" {
		t.Fatalf("re-dispatch did not reuse the lease's composition: %+v", comp)
	}
	if cmp, _ := c.Leases.For("lease-y"); !reflect.DeepEqual(cmp.Professions, []string{"researcher"}) || len(led.recs) != 1 {
		t.Fatalf("parent composition replaced or re-recorded: %+v (records %d)", cmp, len(led.recs))
	}
}

func TestCompose_SkillCapAndDeterministicID(t *testing.T) {
	c, _ := fixture(t)
	c.MaxSkills = 1
	task := &domain.DispatchTask{ID: "t", Description: "x", RequiredCapabilities: []string{"research"}}
	scored := []domain.ScoredCandidate{{Agent: profileAgent("researcher")}, {Agent: profileAgent("analyst")}}
	_, a, _ := c.Compose(context.Background(), task, scored[0], scored, stepHandoff("l1"))
	// Same step, candidates in another order, another lease: same composition.
	_, b, _ := c.Compose(context.Background(), task, scored[1], []domain.ScoredCandidate{scored[1], scored[0]}, stepHandoff("l2"))
	if len(a.Skills) != 1 || a.Skills[0] != "exploratory-analysis" {
		t.Fatalf("cap not applied in name order: %v", a.Skills)
	}
	if a.ID() != b.ID() {
		t.Fatalf("composition id is not deterministic: %s vs %s", a.ID(), b.ID())
	}
}
