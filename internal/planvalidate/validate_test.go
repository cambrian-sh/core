package planvalidate

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func vocab(t *testing.T) *domain.StepEffectVocabulary {
	t.Helper()
	v, err := domain.NewStepEffectVocabulary([]domain.StepEffectTerm{
		{Name: "read", Classes: []domain.ToolEffect{domain.EffectRead}},
		{Name: "write", Classes: []domain.ToolEffect{domain.EffectWrite}},
		{Name: "send", Refines: domain.EffectEgress, Tools: []string{"*send*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func checklist() *domain.RequirementChecklist {
	return &domain.RequirementChecklist{Request: "list overdue customers due before 2026-11-01, draft reminders, do not send",
		Items: []domain.RequirementItem{
			{ID: "R1", Type: domain.RequirementAnswer, Text: "list overdue customers"},
			{ID: "R2", Type: domain.RequirementWrite, Text: "draft a reminder for each"},
			{ID: "R3", Type: domain.RequirementConstraint, Text: "do not send", Quote: "do not send", Forbids: []string{"send"}},
		}}
}

func rules(vs []Violation) map[string]int {
	m := map[string]int{}
	for _, v := range vs {
		m[v.Rule]++
	}
	return m
}

func src(i int) *int { return &i }

// An o3x-shaped plan that passes every rule.
func goodPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "use ar_aging to list customers overdue before 2026-11-01 as a JSON array", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
		{Query: "draft a reminder for {item}", DependsOn: []int{0}, FanOutOver: src(0), ForEach: "s0", Inputs: []string{"s0[*]"},
			RequiredEffects: []string{"write"}, Answers: []string{"R2"}},
	}}
}

func TestValidate_AValidPlanPasses(t *testing.T) {
	vs := Validate(Input{Plan: goodPlan(), Checklist: checklist(), Vocabulary: vocab(t),
		DateSources: []string{checklist().Request}})
	if len(vs) != 0 {
		t.Fatalf("violations: %+v", vs)
	}
}

func TestValidate_EveryRuleGroupReportsStructuredViolations(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list customers due 2026-10-15", RequiredEffects: []string{}, Answers: []string{"R9"}},                  // date not sourced, unknown item
		{Query: "send the reminders", DependsOn: []int{0}, RequiredEffects: []string{"send"}, Inputs: []string{"s3.x"}}, // binding not a dep (and out of range)
		{Query: "for each customer", FanOutOver: src(7), ForEach: "s0.list", RequiredEffects: []string{"read"}},         // fan-out out of range, untemplated, for_each not a dep
	}}
	vs := Validate(Input{Plan: p, Checklist: checklist(), Vocabulary: vocab(t), DateSources: []string{"today 2026-11-08"}})
	got := rules(vs)
	for _, want := range []string{RuleDate, RuleUnknownItem, RuleUncovered, RuleBinding, RuleFanOut, RuleForEach} {
		if got[want] == 0 {
			t.Errorf("no %s violation (all: %+v)", want, vs)
		}
	}
	if got[RuleUncovered] != 2 { // R1 and R2 are answered by no step (R9 is not an item)
		t.Errorf("uncovered = %d", got[RuleUncovered])
	}
	r := Render(vs)
	if !strings.Contains(r, `"rule": "dates.unsourced_date"`) || strings.Count(r, "\n") != len(vs)-1 {
		t.Fatalf("render = %s", r)
	}
}

// ADR-0145 D14: no effect rules. A step's declared effects (even one a
// constraint forbids, or one outside the vocabulary) are not validated: the
// constraint is enforced at call time by tool class.
func TestValidate_NoEffectRules(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
		{Query: "draft reminders", RequiredEffects: []string{"write", "send", "teleport"}, Answers: []string{"R2"}},
	}}
	if vs := Validate(Input{Plan: p, Checklist: checklist(), Vocabulary: vocab(t)}); len(vs) != 0 {
		t.Fatalf("effect rules must be gone: %+v", vs)
	}
}

// ADR-0145 D14 (o3x #2): stage 2 planned drafts to recipients stage 1 had written,
// labelled answers:[R1]. A writing step naming a written target is refused
// whatever its answers labels; a read step naming it is fine.
func TestValidate_WrittenTargetsAcrossStages(t *testing.T) {
	written := []domain.WrittenTarget{{Target: "mail_draft|to=brisa.ashcombe@wychwood.example", Tool: "mail_draft", IDs: []string{"id=W-000002"}}}
	stage2 := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "draft a payment reminder to brisa.ashcombe@wychwood.example", RequiredEffects: []string{"write", "read"}, Answers: []string{"R1"}},
		{Query: "look up the account of brisa.ashcombe@wychwood.example", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
	}}
	r := rules(Validate(Input{Plan: stage2, Checklist: checklist(), Vocabulary: vocab(t), Staged: true, Settled: []string{"R2"}, Written: written}))
	if r[RuleWrittenTarget] != 1 {
		t.Fatalf("written-target violations = %v", r)
	}
}

func TestValidate_ForEachMustFanOut(t *testing.T) {
	p := goodPlan()
	p.Steps[1].FanOutOver = nil
	p.Steps[1].Query = "draft a reminder for each overdue customer"
	if got := rules(Validate(Input{Plan: p})); got[RuleForEach] != 1 {
		t.Fatalf("for_each without fan_out_over: %v", got)
	}
}

func TestValidate_CycleAndSelfDependency(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a", DependsOn: []int{1}}, {Query: "b", DependsOn: []int{0}}}}
	if got := rules(Validate(Input{Plan: p})); got[RuleCycle] == 0 && got[RuleDependency] == 0 {
		t.Fatalf("a cycle must be refused: %v", got)
	}
}

// Without a checklist only structure and dates apply (the D7 flag can be on with
// the checklist off).
func TestValidate_RuleGroupsFollowTheirInputs(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "send it", RequiredEffects: []string{"send"}}}}
	if vs := Validate(Input{Plan: p}); len(vs) != 0 {
		t.Fatalf("no checklist and no vocabulary: %+v", vs)
	}
}

// ADR-0145 D11: one stage of the staged planner need not answer every item, but
// it answers at least one open item and never a settled one.
func TestValidate_StagedCoverage(t *testing.T) {
	stage1 := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
	}}
	if vs := Validate(Input{Plan: stage1, Checklist: checklist(), Staged: true}); len(vs) != 0 {
		t.Fatalf("a first stage answering R1 only is valid: %v", vs)
	}
	if r := rules(Validate(Input{Plan: stage1, Checklist: checklist()})); r[RuleUncovered] != 1 {
		t.Fatalf("the single-plan arm still requires every item: %v", r)
	}
	redo := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers again", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
	}}
	r := rules(Validate(Input{Plan: redo, Checklist: checklist(), Staged: true, Settled: []string{"R1"}}))
	if r[RuleSettledItem] != 1 || r[RuleNoOpenItem] != 1 {
		t.Fatalf("a stage that only redoes a settled item: %v", r)
	}
}

// ADR-0145 D13 (T-20261108-plan-validator-enforce-the-d13-decomposition).
func TestValidate_DecompositionRules(t *testing.T) {
	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "which customers to escalate"},
		{ID: "R2", Type: domain.RequirementAnswer, Text: "a reminder per overdue customer"},
	}}
	// J1: two siblings each decide R1 on part of the picture.
	split := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "assess customers A-M", Answers: []string{"R1"}},
		{Query: "assess customers N-Z", Answers: []string{"R1"}},
		{Query: "list overdue", Answers: []string{"R2"}},
	}}
	if r := rules(Validate(Input{Plan: split, Checklist: cl})); r[RuleSplitJudgement] != 1 {
		t.Fatalf("split judgement: %v", r)
	}
	// Joined: one answering step depends on the other, so it decides with both.
	joined := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "gather customer facts", Answers: []string{"R1"}},
		{Query: "decide which customers to escalate", DependsOn: []int{0}, Answers: []string{"R1", "R2"}},
	}}
	if r := rules(Validate(Input{Plan: joined, Checklist: cl})); r[RuleSplitJudgement] != 0 {
		t.Fatalf("a joining step is the one judgement: %v", r)
	}
	// J2: a fan-out step answers a set-level item without for_each.
	fan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers as a JSON array", Answers: []string{"R2"}},
		{Query: "decide whether to escalate {item}", DependsOn: []int{0}, FanOutOver: src(0), Answers: []string{"R1"}},
	}}
	if r := rules(Validate(Input{Plan: fan, Checklist: cl})); r[RuleFanOutJudgement] != 1 {
		t.Fatalf("fan-out judgement: %v", r)
	}
	// J3: R2 is per member of step 0's set; another step answers R2 without it.
	partial := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers as a JSON array", Answers: []string{"R1"}},
		{Query: "draft a reminder for {item}", DependsOn: []int{0}, FanOutOver: src(0), ForEach: "s0.customers", Answers: []string{"R2"}},
		{Query: "draft reminders for the usual customers", Answers: []string{"R2"}},
	}}
	if r := rules(Validate(Input{Plan: partial, Checklist: cl})); r[RulePartialSetInputs] != 1 || r[RuleFanOutJudgement] != 0 {
		t.Fatalf("partial set inputs: %v", r)
	}
}
