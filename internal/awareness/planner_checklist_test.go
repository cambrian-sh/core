package awareness

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 phase 2: the checklist arm (planner_cap v7 + planner_input v3) is used
// only when the server put a checklist on ctx; the legacy arms stay pinned to the
// versions they were measured on, byte for byte.
func TestPlanner_ChecklistArmOnlyWithAChecklist(t *testing.T) {
	if plannerCapPrompts.System.Version != 6 || plannerCapPrompts.Input.Version != 2 || plannerPrompts.Input.Version != 2 {
		t.Fatalf("legacy arms must stay pinned: cap v%d input v%d / plain input v%d",
			plannerCapPrompts.System.Version, plannerCapPrompts.Input.Version, plannerPrompts.Input.Version)
	}
	if plannerChecklistPrompts.System.Version != 10 || plannerChecklistPrompts.Input.Version != 3 {
		t.Fatalf("checklist arm = v%d + v%d", plannerChecklistPrompts.System.Version, plannerChecklistPrompts.Input.Version)
	}

	gen := &mockGenerator{response: minimalPlanJSON()}
	p := NewPlanner(gen, &mockAgentProvider{})
	plan, err := p.GetExecutionPlan(context.Background(), "list overdue customers")
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlannerPromptVersion != plannerPromptHash || strings.Contains(gen.capturedPrompts[0], "<requirements>") {
		t.Fatal("without a checklist the legacy arm must be sent unchanged")
	}

	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "List overdue customers", Quote: "vadesi geçen müşteriler"},
		{ID: "R2", Type: domain.RequirementConstraint, Text: "Do not send", Quote: "gönderme", Forbids: []string{"send"}},
	}}
	ctx := domain.WithRequirementChecklist(context.Background(), cl)
	ctx = domain.WithCoverageStage(ctx, &domain.CoverageStage{Number: 2, Settled: []string{"R2"},
		Open:    []domain.OpenItem{{ID: "R1", Reason: "members with no result: CUS-007", Missing: []string{"CUS-007"}}},
		Earlier: "Results of stage 1 (read an entry with ledger_get):\n- step 0 \"list\" -> led_x: CUS-004"})
	gen2 := &mockGenerator{response: minimalPlanJSON()}
	p2 := NewPlanner(gen2, &mockAgentProvider{})
	plan, err = p2.GetExecutionPlan(ctx, "list overdue customers")
	if err != nil {
		t.Fatal(err)
	}
	sent := gen2.capturedPrompts[0]
	for _, want := range []string{"<requirements>", `R1 [answer] List overdue customers (from the request: "vadesi geçen müşteriler")`,
		"forbids: send", "Stage 2.", "cover exactly these: CUS-007", "led_x", "Requirements:"} {
		if !strings.Contains(sent, want) {
			t.Errorf("checklist arm request lacks %q", want)
		}
	}
	if plan.PlannerPromptVersion != plannerPromptHashChecklist {
		t.Fatal("a checklist plan must be stamped with the checklist arm's hash")
	}
}
