package awareness

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func routine() domain.Procedure {
	return domain.Procedure{
		ID: "proc-1", Trigger: "goal: ship the billing service",
		Confidence: 0.87, SampleCount: 4,
		Steps: []domain.ProcedureStep{
			{RequiredCapabilities: []string{"build"}, Intent: "compile the artefact"},
			{RequiredCapabilities: []string{"deploy"}, Intent: "roll it out"},
		},
	}
}

// The block must read as EVIDENCE, not instruction. A numbered list of steps reads as a
// directive unless the prompt says otherwise, and a planner treating a routine as
// authority turns induced memory into a control channel (ADR-0094 D6).
func TestProcedureBlock_IsFramedAsAdvisory(t *testing.T) {
	block := buildProcedureBlock([]domain.Procedure{routine()})
	lower := strings.ToLower(block)
	if !strings.Contains(lower, "advisory") {
		t.Error("the block must tell the planner it may adapt or ignore the routine")
	}
	if !strings.Contains(lower, "auction") {
		t.Error("the block must say selection remains the auction's, or a routine reads " +
			"as a pre-decided assignment")
	}
}

// ADR-0094 D2, at the prompt boundary: whatever else the block says, it must not hand
// the planner an agent to use. That is the Zero-Hardcode rule at the last place it
// could leak.
func TestProcedureBlock_NamesCapabilitiesNotAgents(t *testing.T) {
	p := routine()
	// Even if attribution is present on the record, it must not reach the prompt.
	p.ContributingAgents = []string{"code_generator_agent", "deployer_agent"}
	block := buildProcedureBlock([]domain.Procedure{p})

	for _, agent := range p.ContributingAgents {
		if strings.Contains(block, agent) {
			t.Errorf("agent %q leaked into the planner prompt — that is a learned "+
				"routing table:\n%s", agent, block)
		}
	}
	if !strings.Contains(block, `capabilities="build"`) {
		t.Errorf("steps must be presented by capability:\n%s", block)
	}
}

// Corroboration must be visible, so a barely-seen routine can be weighed differently
// from an established one — the same reason a precedent carries its similarity.
func TestProcedureBlock_SurfacesCorroboration(t *testing.T) {
	block := buildProcedureBlock([]domain.Procedure{routine()})
	if !strings.Contains(block, "0.87") || !strings.Contains(block, `observed="4"`) {
		t.Errorf("confidence and observation count must be visible to the planner:\n%s", block)
	}
}

// No routines ⇒ no block. An empty section is prompt budget spent on nothing, and the
// planner should not be told there is procedural memory when there is none.
func TestProcedureBlock_EmptyWhenNoRoutines(t *testing.T) {
	if got := buildProcedureBlock(nil); got != "" {
		t.Errorf("expected no block, got %q", got)
	}
}
