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
	s := &Server{ToolExecutor: newTestToolExecutor(t, domain.ToolExecutorControls{Guards: &domain.RequestGuards{}}, domain.ToolExecutor{})}
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
	// ADR-0145 D14.6: a document item is not a write item (it is finished inside
	// Cambrian); R3 is settled.
	if g == nil || g.Request != "turn:c1/m7" || !g.Checklist || len(g.WriteItems) != 0 ||
		!reflect.DeepEqual(g.Documents, []string{"R2"}) || len(g.Carries) != 0 {
		t.Fatalf("guard = %+v", g)
	}
	// ENG-1426: "send" is the ADR-0150 D9 class egress_send.
	if len(g.Constraints) != 1 || !reflect.DeepEqual(g.Constraints[0].Classes, []domain.ToolEffect{domain.EffectEgressSend}) {
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
	if err != nil || !reflect.DeepEqual(terms["send"], []domain.ToolEffect{domain.EffectEgressSend}) {
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

// T-20261109-d14-constraint-forbids-change-refuses-the-send (o3x replay, runs
// 2a8623eec5f262dc / f3104e8631ba3491): "Send them now, exactly as drafted" derived
// R1 write "Send the drafts" + R2 constraint forbids [change]. The step answering
// R1 carries the write class (and egress_send, from does: [send]), so the constraint
// does not refuse the send it asks for; a step not answering R1 is still held to it.
func TestStepGuard_ConstraintDoesNotBlockTheRequestsOwnWrite(t *testing.T) {
	s := &Server{ToolExecutor: newTestToolExecutor(t, domain.ToolExecutorControls{Guards: &domain.RequestGuards{}}, domain.ToolExecutor{})}
	s.ExecCfg.Tools.RequestGuards = true
	cl := domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: "Send the drafts", Quote: "Send them now", Does: []string{"send"}},
		{ID: "R2", Type: domain.RequirementConstraint, Quote: "exactly as drafted", Forbids: []string{"change"}},
	}}
	reqs := &requestRequirements{checklist: &cl, coverage: requirements.NewCoverage(cl)}
	send := domain.SystemTool{Name: "mail_send_draft", Effects: []domain.ToolEffect{domain.EffectWrite, domain.EffectEgress}}

	g := s.stepGuard(context.Background(), domain.Step{Answers: []string{"R1"}}, reqs, "run-1")
	if !reflect.DeepEqual(g.Carries, []domain.ToolEffect{domain.EffectEgressSend, domain.EffectWrite}) {
		t.Fatalf("carries = %v", g.Carries)
	}
	if c, cl, hit := domain.ForbiddenBy(send, g.Constraints, g.Carries...); hit {
		t.Fatalf("the requested send was refused by %s (%s)", c.Item, cl)
	}
	other := s.stepGuard(context.Background(), domain.Step{Answers: []string{}}, reqs, "run-1")
	if _, _, hit := domain.ForbiddenBy(send, other.Constraints, other.Carries...); !hit {
		t.Fatal("a step not carrying out R1 must still be held to 'exactly as drafted'")
	}
	// Without does, a write item still carries the write class: the 'change'
	// constraint (write) does not refuse it, but a 'send' constraint (egress_send) would.
	cl.Items[0].Does = nil
	g = s.stepGuard(context.Background(), domain.Step{Answers: []string{"R1"}}, reqs, "run-1")
	if _, _, hit := domain.ForbiddenBy(send, g.Constraints, g.Carries...); hit {
		t.Fatal("a write item always carries the write class")
	}
	draftOnly := domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: "Save the reply as a draft in the mailbox", Quote: "save it as a draft in my mail"},
		{ID: "R2", Type: domain.RequirementConstraint, Quote: "don't send it", Forbids: []string{"send"}},
	}}
	reqs = &requestRequirements{checklist: &draftOnly, coverage: requirements.NewCoverage(draftOnly)}
	g = s.stepGuard(context.Background(), domain.Step{Answers: []string{"R1"}}, reqs, "run-1")
	if _, cls, hit := domain.ForbiddenBy(send, g.Constraints, g.Carries...); !hit || cls != domain.EffectEgressSend {
		t.Fatal("'save as a draft, don't send it': the drafting write must not license a send")
	}
}
