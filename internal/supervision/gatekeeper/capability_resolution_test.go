package gatekeeper

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// resolutionCfg turns on the ADR-0100 D5 ladder plus the ROUTE-03 contract it
// gates, which is the shipped default combination.
func resolutionCfg() config.ExecutionConfig {
	cfg := defaultGatekeeperCfg()
	cfg.Capability.CapabilityResolution = true
	return cfg
}

func resolutionAgents() *mockAgentDeclarationSource {
	return newMockAgentDeclarationSource(
		[]domain.AgentDefinition{
			{ID: "terminal_agent"},
			{ID: "calculator_agent"},
		},
		map[string]*domain.AgentManifest{
			"terminal_agent":   {Capabilities: []string{"shell_execution", "general_purpose"}},
			"calculator_agent": {Capabilities: []string{"calculation", "general_purpose"}},
		},
	)
}

// A requirement the fleet declares gates normally — only the agent declaring it survives.
func TestResolution_ExactRequirementGatesToDeclaringAgent(t *testing.T) {
	g := NewGatekeeper(resolutionAgents(), resolutionCfg())

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t1", Description: "run ls", RequiredCapabilities: []string{"shell_execution"},
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Agent.ID != "terminal_agent" {
		t.Errorf("candidates = %v, want only terminal_agent", ids(got))
	}
}

// D5 rung 3: a requirement NOTHING declares must not gate everyone out. Before
// the ladder this produced a dead step (ADR-0096: 4 measured); now it falls back
// to the generalist tier so the plan survives.
func TestResolution_UnknownCapabilityFallsBackToGeneralists(t *testing.T) {
	g := NewGatekeeper(resolutionAgents(), resolutionCfg())

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t2", Description: "extract a pdf", RequiredCapabilities: []string{"pdf_extract"},
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("unknown capability produced an empty slate — the step would die (the D5 regression)")
	}
}

// D5 rung 4: no match AND no generalist ⇒ a typed, loud failure naming the gap.
func TestResolution_UnsatisfiableReturnsTypedError(t *testing.T) {
	src := newMockAgentDeclarationSource(
		[]domain.AgentDefinition{{ID: "calculator_agent"}},
		map[string]*domain.AgentManifest{
			"calculator_agent": {Capabilities: []string{"calculation"}}, // no general_purpose
		},
	)
	g := NewGatekeeper(src, resolutionCfg())

	_, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t3", Description: "extract a pdf", RequiredCapabilities: []string{"pdf_extract"},
	})
	if err == nil {
		t.Fatal("expected a typed failure when nothing can satisfy the step")
	}
	var noMatch *domain.NoCapabilityMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("error = %T, want *domain.NoCapabilityMatchError", err)
	}
	if len(noMatch.Unmatched) != 1 || noMatch.Unmatched[0] != "pdf_extract" {
		t.Errorf("Unmatched = %v, want [pdf_extract]", noMatch.Unmatched)
	}
	if len(noMatch.Vocabulary) == 0 {
		t.Error("error must carry the live vocabulary so an operator can see the gap")
	}
}

// The authored alias map is the only synonym mechanism — and it works end to end.
func TestResolution_AliasMapRoutesToDeclaringAgent(t *testing.T) {
	cfg := resolutionCfg()
	cfg.Capability.CapabilityAliases = map[string]string{"run_command": "shell_execution"}
	g := NewGatekeeper(resolutionAgents(), cfg)

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t4", Description: "run ls", RequiredCapabilities: []string{"run_command"},
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Agent.ID != "terminal_agent" {
		t.Errorf("candidates = %v, want only terminal_agent via the alias", ids(got))
	}
}

// Steps that declare nothing must be untouched by the ladder — that is the
// pre-ROUTE-03 path and it has to stay byte-identical.
func TestResolution_NoRequirementsIsUnaffected(t *testing.T) {
	g := NewGatekeeper(resolutionAgents(), resolutionCfg())

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t5", Description: "anything"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("candidates = %v, want both agents when no capability is required", ids(got))
	}
}

// With the ladder OFF, an unknown capability gates everyone out again — proving
// the flag actually controls the behaviour.
func TestResolution_DisabledRestoresHardGate(t *testing.T) {
	cfg := resolutionCfg()
	cfg.Capability.CapabilityResolution = false
	g := NewGatekeeper(resolutionAgents(), cfg)

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t6", Description: "extract a pdf", RequiredCapabilities: []string{"pdf_extract"},
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("candidates = %v, want none with resolution disabled", ids(got))
	}
}

func ids(cs []domain.ScoredCandidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Agent.ID)
	}
	return out
}

// REGRESSION (live probe, 2026-07-29): L1 compares VERBATIM unless
// canonical_vocab is on, so a planner tag that differs only in spelling from the
// declared one must be substituted before gating — otherwise every agent is
// filtered and the step dies with an empty slate.
func TestResolution_PlannerSpellingIsSubstitutedBeforeVerbatimGate(t *testing.T) {
	cfg := resolutionCfg()
	cfg.Capability.CanonicalVocab = false // the shipped default: L1 is a verbatim comparison
	g := NewGatekeeper(resolutionAgents(), cfg)

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t7", Description: "run ls",
		RequiredCapabilities: []string{"Shell-Execution"}, // declared as shell_execution
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Agent.ID != "terminal_agent" {
		t.Errorf("candidates = %v, want terminal_agent — a spelling variant must not empty the slate", ids(got))
	}
}

// ADR-0100 D1: L1 decides eligibility, L2 only expresses preference. When the
// capability contract admitted an agent, a low semantic score must not empty the
// slate and kill the step. Measured on the live probe 2026-07-29: L1 admitted one
// eligible agent, L2 eliminated it at similarity 0.0, step died `no_candidate`.
func TestResolution_L2CannotEmptyASlateL1Approved(t *testing.T) {
	cfg := resolutionCfg()
	g := NewGatekeeper(resolutionAgents(), cfg,
		WithEmbedder(&mockEmbedder{}),
		// Nobody clears the semantic threshold.
		WithSearcher(&fakeInterviewSearcher{results: map[string]float64{}}),
	)

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t8", Description: "run ls", RequiredCapabilities: []string{"shell_execution"},
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Agent.ID != "terminal_agent" {
		t.Errorf("candidates = %v, want terminal_agent — L2 must not veto L1 eligibility", ids(got))
	}
}

// The converse: with NO declared requirements, L1 is a free pass, so L2 is the
// only filter and it may legitimately empty the slate (ADR-0023).
func TestResolution_L2MayEmptySlateWhenL1DidNotGate(t *testing.T) {
	g := NewGatekeeper(resolutionAgents(), resolutionCfg(),
		WithEmbedder(&mockEmbedder{}),
		WithSearcher(&fakeInterviewSearcher{results: map[string]float64{}}),
	)

	got, err := g.FindCandidates(context.Background(), &domain.DispatchTask{
		ID: "t9", Description: "anything at all",
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("candidates = %v, want none — without a capability contract L2 is the only gate", ids(got))
	}
}
