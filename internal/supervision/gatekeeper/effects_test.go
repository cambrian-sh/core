package gatekeeper

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared): L1
// admits only agents whose granted tools' policies cover the step's required effects.

// fakeToolLister is a live, mutable grant set: AvailableTools per agent.
type fakeToolLister struct {
	mu    sync.Mutex
	tools map[string][]domain.SystemTool
	calls int
}

func (f *fakeToolLister) AvailableTools(_ context.Context, agentID string) []domain.SystemTool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return append([]domain.SystemTool(nil), f.tools[agentID]...)
}

func (f *fakeToolLister) set(agentID string, tools ...domain.SystemTool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools[agentID] = tools
}

func tool(name string, effs ...domain.ToolEffect) domain.SystemTool {
	return domain.SystemTool{Name: name, Effects: effs}
}

var (
	readFile  = tool("mcp:filesystem/read_text_file", domain.EffectRead)
	writeFile = tool("mcp:filesystem/write_file", domain.EffectWrite)
	moveFile  = tool("mcp:filesystem/move_file", domain.EffectWrite)
	webSearch = tool("mcp:brave-search/web_search", domain.EffectRead, domain.EffectEgress)
)

func defaultVocab(t *testing.T) *domain.StepEffectVocabulary {
	t.Helper()
	v, err := config.CapabilityConfig{EffectVocabulary: config.DefaultEffectVocabulary()}.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// p05 fleet: assistant can write but not move (the R-20261106T033413Z misroute),
// organiser can move, researcher only reads the web.
func p05Fleet(t *testing.T) (*mockAgentDeclarationSource, *fakeToolLister, *ToolEffectSource) {
	t.Helper()
	src := newMockAgentDeclarationSource(
		[]domain.AgentDefinition{{ID: "assistant"}, {ID: "organiser"}, {ID: "researcher"}}, nil)
	lister := &fakeToolLister{tools: map[string][]domain.SystemTool{
		"assistant":  {readFile, writeFile},
		"organiser":  {readFile, writeFile, moveFile},
		"researcher": {webSearch},
	}}
	return src, lister, NewToolEffectSource(defaultVocab(t), lister)
}

func TestEffectCheck_EligibleAgentIsChosen(t *testing.T) {
	src, _, effects := p05Fleet(t)
	g := NewGatekeeper(src, defaultGatekeeperCfg(), WithEffects(effects))
	task := &domain.DispatchTask{ID: "task-5", Description: "move receipt(2).pdf to Documents/Receipts", RequiredEffects: []string{"move"}}
	got, err := g.FindCandidates(context.Background(), task)
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if ids := candidateIDs(got); !reflect.DeepEqual(ids, []string{"organiser"}) {
		t.Fatalf("candidates = %v, want only the agent with a move tool", ids)
	}
	ec := task.EffectCheck
	if ec == nil || ec.Outcome != domain.EffectCheckEligible || !reflect.DeepEqual(ec.Eligible, []string{"organiser"}) {
		t.Fatalf("effect check record = %+v", ec)
	}
	if !reflect.DeepEqual(ec.AgentEffects["assistant"], []string{"read", "write"}) {
		t.Errorf("assistant's derived effects = %v", ec.AgentEffects["assistant"])
	}
}

func TestEffectCheck_NoEligibleAgentIsRefusedNeverArbitrary(t *testing.T) {
	src, lister, effects := p05Fleet(t)
	lister.set("organiser", readFile, writeFile) // nobody can move any more
	g := NewGatekeeper(src, defaultGatekeeperCfg(), WithEffects(effects))
	task := &domain.DispatchTask{ID: "task-5", RequiredEffects: []string{"move"}}
	got, err := g.FindCandidates(context.Background(), task)
	var nem *domain.NoEffectMatchError
	if !errors.As(err, &nem) {
		t.Fatalf("want NoEffectMatchError, got %v (candidates %v)", err, candidateIDs(got))
	}
	if len(got) != 0 {
		t.Fatalf("a refused step must hand back no candidate, got %v", candidateIDs(got))
	}
	if !reflect.DeepEqual(nem.Missing, []string{"move"}) || !strings.Contains(err.Error(), "no agent can move") {
		t.Errorf("refusal = %v", err)
	}
	if task.EffectCheck.Outcome != domain.EffectCheckRefused {
		t.Errorf("outcome = %q", task.EffectCheck.Outcome)
	}
}

func TestEffectCheck_UnknownEffectIsUnmatched(t *testing.T) {
	src, _, effects := p05Fleet(t)
	g := NewGatekeeper(src, defaultGatekeeperCfg(), WithEffects(effects))
	task := &domain.DispatchTask{ID: "task-1", RequiredEffects: []string{"teleport"}}
	_, err := g.FindCandidates(context.Background(), task)
	var nem *domain.NoEffectMatchError
	if !errors.As(err, &nem) || !reflect.DeepEqual(nem.Missing, []string{"teleport"}) {
		t.Fatalf("an effect outside the vocabulary must refuse the step, got %v", err)
	}
	if !reflect.DeepEqual(task.EffectCheck.Unknown, []string{"teleport"}) {
		t.Errorf("unknown not recorded: %+v", task.EffectCheck)
	}
}

func TestEffectCheck_HardPinnedAgentThatFailsIsRefused(t *testing.T) {
	src, _, effects := p05Fleet(t)
	cfg := defaultGatekeeperCfg()
	cfg.Routing.AgentPinning = true
	g := NewGatekeeper(src, cfg, WithEffects(effects))
	task := &domain.DispatchTask{ID: "task-2", RequiredEffects: []string{"move"},
		PreferredAgent: "assistant", AgentPin: domain.PinHard}
	got, err := g.FindCandidates(context.Background(), task)
	var nem *domain.NoEffectMatchError
	if !errors.As(err, &nem) || len(got) != 0 {
		t.Fatalf("a hard-pinned agent without the effect must be refused, got %v / %v", candidateIDs(got), err)
	}
	if task.EffectCheck.PinRefused != "assistant" {
		t.Errorf("pin refusal not recorded: %+v", task.EffectCheck)
	}
	// A hard pin on an agent that HAS the effect still binds it.
	ok := &domain.DispatchTask{ID: "task-3", RequiredEffects: []string{"move"}, PreferredAgent: "organiser", AgentPin: domain.PinHard}
	got, err = g.FindCandidates(context.Background(), ok)
	if err != nil || !reflect.DeepEqual(candidateIDs(got), []string{"organiser"}) {
		t.Fatalf("capable hard pin: %v / %v", candidateIDs(got), err)
	}
}

func TestEffectCheck_SoftPinnedAgentThatFailsIsRefused(t *testing.T) {
	src, _, effects := p05Fleet(t)
	cfg := defaultGatekeeperCfg()
	cfg.Routing.AgentPinning = true
	g := NewGatekeeper(src, cfg, WithEffects(effects))
	task := &domain.DispatchTask{ID: "task-2", RequiredEffects: []string{"move"},
		PreferredAgent: "assistant", AgentPin: domain.PinSoft}
	got, err := g.FindCandidates(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if ids := candidateIDs(got); !reflect.DeepEqual(ids, []string{"organiser"}) {
		t.Fatalf("the soft-pinned agent lacks move and must not be a candidate: %v", ids)
	}
	if task.EffectCheck.PinRefused != "assistant" {
		t.Errorf("pin refusal not recorded: %+v", task.EffectCheck)
	}
}

// Fail closed: a gatekeeper with no effect source admits nobody for an
// effect-declaring step.
func TestEffectCheck_NoSourceFailsClosed(t *testing.T) {
	src, _, _ := p05Fleet(t)
	g := NewGatekeeper(src, defaultGatekeeperCfg())
	_, err := g.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t", RequiredEffects: []string{"read"}})
	if !domain.IsDispatchRefusal(err) {
		t.Fatalf("want a refusal without an effect source, got %v", err)
	}
}

// Back-compat: a step that declares nothing dispatches as before (unverified); an
// empty declared list needs nothing.
func TestEffectCheck_LegacyStepIsUnverifiedNotRefused(t *testing.T) {
	src, _, effects := p05Fleet(t)
	g := NewGatekeeper(src, defaultGatekeeperCfg(), WithEffects(effects))
	legacy := &domain.DispatchTask{ID: "t"}
	got, err := g.FindCandidates(context.Background(), legacy)
	if err != nil || len(got) != 3 {
		t.Fatalf("legacy step: %v / %v", candidateIDs(got), err)
	}
	if legacy.EffectCheck == nil || legacy.EffectCheck.Outcome != domain.EffectCheckUnverified {
		t.Errorf("legacy outcome = %+v", legacy.EffectCheck)
	}
	none := &domain.DispatchTask{ID: "t", RequiredEffects: []string{}}
	if _, err := g.FindCandidates(context.Background(), none); err != nil || none.EffectCheck.Outcome != domain.EffectCheckNone {
		t.Errorf("declared-empty step: %+v / %v", none.EffectCheck, err)
	}
}

// Live grant changes: an integration adding a move tool to an agent makes it
// eligible on the next dispatch, and removing it takes it away — no restart, no
// stale cache.
func TestEffectCheck_LiveGrantChangesUpdateEligibility(t *testing.T) {
	src, lister, effects := p05Fleet(t)
	g := NewGatekeeper(src, defaultGatekeeperCfg(), WithEffects(effects))
	find := func() []string {
		got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t", RequiredEffects: []string{"move"}})
		if err != nil {
			return nil
		}
		return candidateIDs(got)
	}
	if got := find(); !reflect.DeepEqual(got, []string{"organiser"}) {
		t.Fatalf("before: %v", got)
	}
	lister.set("assistant", readFile, writeFile, moveFile) // integration installed
	if got := find(); !reflect.DeepEqual(got, []string{"assistant", "organiser"}) {
		t.Fatalf("after the grant: %v", got)
	}
	lister.set("organiser", readFile) // grant re-sync dropped the tool
	lister.set("assistant", readFile)
	if got := find(); got != nil {
		t.Fatalf("after the removal every agent lacks move, got %v", got)
	}
	// A tool whose POLICY changes (re-registered without one) changes the key too.
	lister.set("organiser", domain.SystemTool{Name: moveFile.Name, Effects: moveFile.Effects, EffectsDefaulted: true})
	if got := find(); got != nil {
		t.Fatalf("a tool on the fail-closed default policy must not count, got %v", got)
	}
}
