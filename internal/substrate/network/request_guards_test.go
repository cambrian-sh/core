package network

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/requirements"
)

// ADR-0145 D14: the facts a step's guards need are computed at dispatch from the
// kernel's own state (T-20261109-adr-0145-d14-fix-batch-remove-per-step).
func TestStepGuard_FromTheChecklistAndCoverage(t *testing.T) {
	s := &Server{ToolExecutor: &domain.ToolExecutor{Guards: &domain.RequestGuards{}}}
	s.ExecCfg.Tools.RequestGuards = true
	cl := domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer},
		{ID: "R2", Type: domain.RequirementDocument},
		{ID: "R3", Type: domain.RequirementWrite},
		{ID: "R4", Type: domain.RequirementConstraint, Quote: "do not send yet", Forbids: []string{"send"}},
	}}
	cov := requirements.NewCoverage(cl)
	// R3 settles: a step answering it is no longer asked to write.
	cov.ObserveStep(requirements.StepObservation{Step: 0,
		Results: []domain.ItemResult{{Item: "R3", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{{Step: 0, Tool: "crm_note", Status: domain.PlanActionDone}}})
	reqs := &requestRequirements{checklist: &cl, coverage: cov}
	ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{ConversationID: "c1", TurnID: "m7"})

	g := s.stepGuard(ctx, domain.Step{Answers: []string{"R1", "R2", "R3"}}, reqs, "run-1")
	if g == nil || g.Request != "turn:c1/m7" || !g.Checklist || !reflect.DeepEqual(g.WriteItems, []string{"R2"}) {
		t.Fatalf("guard = %+v", g)
	}
	if len(g.Constraints) != 1 || !reflect.DeepEqual(g.Constraints[0].Classes, []domain.ToolEffect{domain.EffectEgress}) {
		t.Fatalf("constraints = %+v", g.Constraints)
	}
	if g := s.stepGuard(domain.WithRepairTurn(context.Background()), domain.Step{}, nil, "run-1"); g.Request != "run:run-1" || !g.Repair || g.Checklist {
		t.Fatalf("no turn, no checklist, repair: %+v", g)
	}
	s.ExecCfg.Tools.RequestGuards = false
	if g := s.stepGuard(ctx, domain.Step{}, reqs, "run-1"); g != nil {
		t.Fatalf("guards off: %+v", g)
	}
}

func TestConstraintTermsDefaults(t *testing.T) {
	terms, err := config.ToolsConfig{}.ConstraintTerms()
	if err != nil || !reflect.DeepEqual(terms["send"], []domain.ToolEffect{domain.EffectEgress}) {
		t.Fatalf("terms = %v err %v", terms, err)
	}
	if _, err := (config.ToolsConfig{ConstraintClasses: map[string][]string{"send": {"teleport"}}}).ConstraintTerms(); err == nil {
		t.Fatal("an unknown class must be refused")
	}
}

func TestPlanRunIDStripsStagesAndGenerations(t *testing.T) {
	for in, want := range map[string]domain.RunID{"abc": "abc", "abc-s2": "abc", "abc-g1": "abc", "abc-s3-g2": "abc"} {
		ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{RunID: in})
		if got := planRunID(ctx); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}
