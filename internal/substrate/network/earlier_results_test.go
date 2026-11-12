package network

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// T-20261112-staged-planner-binds-earlier-stage-results-with: every finished
// stage's completed steps get e<K> labels that continue across stages, and the
// planner's index shows each result under its label.
func TestAddEarlier_LabelsContinueAcrossStagesAndSkipUnfinishedSteps(t *testing.T) {
	r := &requestRequirements{}
	s1 := r.addEarlier(1, []domain.ExecuteStepRef{
		{Step: 0, Query: "list suppliers", Status: domain.ExecuteStepCompleted, Ref: "led_a", Preview: "Acme, Borealis"},
		{Step: 1, Query: "fetch pages", Status: "failed"},
		{Step: 2, Query: "fetch prices", Status: domain.ExecuteStepCompleted, Ref: "led_b"},
	})
	s2 := r.addEarlier(2, []domain.ExecuteStepRef{{Step: 0, Query: "compare", Status: domain.ExecuteStepCompleted, Ref: "led_c"}})
	if len(s1) != 2 || s1[0].Label != "e1" || s1[1].Label != "e2" || s1[1].Step != 2 || len(s2) != 1 || s2[0].Label != "e3" || s2[0].Stage != 2 {
		t.Fatalf("stage 1 = %+v, stage 2 = %+v", s1, s2)
	}
	if len(r.earlier) != 3 {
		t.Fatalf("request keeps %d earlier results, want 3", len(r.earlier))
	}
	idx := earlierStagesIndex(1, s1)
	for _, want := range []string{`- e1 = stage 1 step 0 "list suppliers" -> led_a: Acme, Borealis`, `- e2 = stage 1 step 2 "fetch prices" -> led_b`, "bind one in inputs by its label"} {
		if !strings.Contains(idx, want) {
			t.Fatalf("index lacks %q:\n%s", want, idx)
		}
	}
	if strings.Contains(idx, "fetch pages") {
		t.Fatal("an unfinished step has no result to bind")
	}
}

// End to end with the D7 validator on: stage 2 binds stage 1's result as e1 —
// accepted on the first planner call (no structure.binding round) — and its step
// is handed stage 1's output reference and values, resolved by code.
func TestStagedPlanner_StageTwoBindsAnEarlierResultByItsLabel(t *testing.T) {
	items := []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "Which customers are overdue", Quote: "overdue customers"},
		{ID: "R2", Type: domain.RequirementAnswer, Text: "A reminder draft for each overdue customer", Quote: "draft a reminder"},
	}
	s, sp, disp, _ := stagedServer(t, items,
		planOf(domain.Step{Query: "Use invoices_search to list the overdue customers", RequiredEffects: []string{"read"}, Answers: []string{"R1"}}),
		planOf(domain.Step{Query: "Draft a reminder for each overdue customer", RequiredEffects: []string{"write"},
			Answers: []string{"R2"}, Inputs: []string{"e1.customer"}}),
	)
	s.ExecCfg.Plan.PlanValidator = true
	disp.answer = func(ctx context.Context, q string, record func(string, string) string) []byte {
		if strings.HasPrefix(q, "Use invoices_search") {
			ref := record("invoices_search", `{"invoices":[{"customer":"CUS-004"},{"customer":"CUS-007"}]}`)
			return envelopeJSON(domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				idValue("customer", "CUS-004", ref), idValue("customer", "CUS-007", ref)}})
		}
		ref := record("mail_draft", `{"drafts":[{"id":"DRF-1","to":"CUS-004"},{"id":"DRF-2","to":"CUS-007"}]}`)
		return envelopeJSON(domain.ItemResult{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
			idValue("draft", "DRF-1", ref), idValue("draft_2", "DRF-2", ref)}})
	}
	if _, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte(
		"list the overdue customers and draft a reminder for each")}}); err != nil {
		t.Fatal(err)
	}
	if len(sp.inputs) != 2 || strings.Contains(sp.inputs[1], "plan_violations") {
		t.Fatalf("stage 2's e1 binding must pass the validator first time: %d planner calls", len(sp.inputs))
	}
	if !strings.Contains(sp.seen[1].EarlierStages, "- e1 = stage 1 step 0") {
		t.Fatalf("stage 2's planner must see the e1 label: %s", sp.seen[1].EarlierStages)
	}
	if cs := sp.stages[1]; cs == nil || len(cs.EarlierResults) != 1 || cs.EarlierResults[0].Label != "e1" {
		t.Fatalf("stage 2's coverage stage = %+v", cs)
	}
	var in []domain.StepInputValue
	raw := disp.stepInputs["Draft a reminder for each overdue customer"]
	if err := json.Unmarshal([]byte(raw), &in); err != nil || len(in) != 1 {
		t.Fatalf("stage 2's step inputs = %q (%v)", raw, err)
	}
	if in[0].Binding != "e1.customer" || in[0].Stage != 1 || !domain.IsLedgerID(in[0].Ref) || len(in[0].Values) != 2 {
		t.Fatalf("e1 resolved to %+v", in[0])
	}
}
