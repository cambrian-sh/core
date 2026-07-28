package gatekeeper

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// pinCfg is the pinning arm ON, which is the shipped default.
func pinCfg() config.ExecutionConfig {
	cfg := defaultGatekeeperCfg()
	cfg.AgentPinning = true
	return cfg
}

func pinAgents() *mockAgentDeclarationSource {
	return newMockAgentDeclarationSource(
		[]domain.AgentDefinition{
			{ID: "terminal_agent"},
			{ID: "research_agent"},
			{ID: "calculator_agent"},
		},
		map[string]*domain.AgentManifest{
			"terminal_agent":   {Capabilities: []string{"file_read", "general_purpose"}},
			"research_agent":   {Capabilities: []string{"file_read", "general_purpose"}},
			"calculator_agent": {Capabilities: []string{"general_purpose"}},
		},
	)
}

// A hard pin is the whole slate: the user named the executor, so nothing is
// selected and no other agent may be offered as an alternative.
func TestHardPin_BindsOnlyNamedAgent(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "terminal_agent", AgentPin: domain.PinHard,
	})
	if err != nil {
		t.Fatalf("hard pin on a registered agent must succeed, got %v", err)
	}
	if ids := candidateIDs(got); len(ids) != 1 || ids[0] != "terminal_agent" {
		t.Fatalf("hard pin must return exactly the pinned agent, got %v", ids)
	}
}

// The pin strength arrives from an LLM, so casing must not silently downgrade a
// hard pin to soft.
func TestHardPin_StrengthIsCaseInsensitive(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "terminal_agent", AgentPin: "HARD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids := candidateIDs(got); len(ids) != 1 {
		t.Fatalf("\"HARD\" must be honoured as a hard pin, got %v", ids)
	}
}

// A hard pin fails loudly rather than routing the step somewhere the user did
// not ask for. This is the deliberate difference from a soft pin.
func TestHardPin_UnknownAgentIsAnError(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	_, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "no_such_agent", AgentPin: domain.PinHard,
	})
	if !errors.Is(err, domain.ErrPinnedAgentUnavailable) {
		t.Fatalf("want ErrPinnedAgentUnavailable, got %v", err)
	}
}

// Daemons and privileged system organs never serve task steps; a pin must not be
// a way to reach them.
func TestHardPin_UndispatchableAgentIsAnError(t *testing.T) {
	src := newMockAgentDeclarationSource(
		[]domain.AgentDefinition{{ID: "watcher_agent", Trait: domain.TraitDaemon}}, nil)
	g := NewGatekeeper(src, pinCfg())

	_, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "watcher_agent", AgentPin: domain.PinHard,
	})
	if !errors.Is(err, domain.ErrPinnedAgentUnavailable) {
		t.Fatalf("a daemon must not be bindable by pin, got %v", err)
	}
}

// The load-bearing property of a soft pin: it prioritises without excluding, so
// the step still has somewhere to go if the pinned agent is a poor fit.
func TestSoftPin_RanksFirstButKeepsOthers(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "research_agent", AgentPin: domain.PinSoft,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := candidateIDs(got)
	if len(ids) < 2 {
		t.Fatalf("a soft pin must not narrow the slate to one, got %v", ids)
	}
	if ids[0] != "research_agent" {
		t.Fatalf("soft-pinned agent must rank first, got %v", ids)
	}
}

// A soft pin buys priority, never capabilities: the L1 contract still decides who
// is eligible at all. Pinning calculator_agent (general_purpose only) at a step
// requiring file_read must leave it filtered.
func TestSoftPin_DoesNotBypassCapabilityContract(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything",
		RequiredCapabilities: []string{"file_read"},
		PreferredAgent:       "calculator_agent", AgentPin: domain.PinSoft,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range candidateIDs(got) {
		if id == "calculator_agent" {
			t.Fatal("a soft pin must not carry an agent past the L1 capability gate")
		}
	}
}

// An unspecified strength is the weaker one, so a malformed or missing pin can
// never strand a step.
func TestPin_EmptyStrengthDegradesToSoft(t *testing.T) {
	g := NewGatekeeper(pinAgents(), pinCfg())

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "no_such_agent", AgentPin: "",
	})
	if err != nil {
		t.Fatalf("an unknown SOFT pin must not error, got %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unknown soft pin must leave the slate intact, got %v", candidateIDs(got))
	}
}

// The arm off must restore unpinned selection exactly — including refusing to
// honour a hard pin.
func TestPin_ArmOffIgnoresPinEntirely(t *testing.T) {
	cfg := defaultGatekeeperCfg()
	cfg.AgentPinning = false
	g := NewGatekeeper(pinAgents(), cfg)

	got, err := g.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t1", Description: "anything", PreferredAgent: "terminal_agent", AgentPin: domain.PinHard,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("arm off must ignore the pin and score everyone, got %v", candidateIDs(got))
	}
}

func TestSoftPinBoost_ClampsAtOne(t *testing.T) {
	if got := softPinned(0.9); got != 1.0 {
		t.Fatalf("boost must clamp to 1.0, got %v", got)
	}
	if got := softPinned(0.5); abs64(got-0.75) > 1e-9 {
		t.Fatalf("want 0.5+%v, got %v", SoftPinBoost, got)
	}
}
