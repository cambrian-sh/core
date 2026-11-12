package planvalidate

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-staged-planner-binds-earlier-stage-results-with: one input reference
// grammar, s<N> | e<K>; a rejected reference is answered with the valid ones and
// the closest, never reinterpreted.

func earlierResults() []domain.EarlierResult {
	return []domain.EarlierResult{
		{Label: "e1", Stage: 1, Step: 0, Ref: "led_aaaaaaaaaaaa1111"},
		{Label: "e2", Stage: 1, Step: 3, Ref: "led_07f59bbbbbbb2222"},
	}
}

func bindingDetails(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		if v.Rule == RuleBinding {
			out = append(out, v.Detail)
		}
	}
	return out
}

func TestValidate_EarlierStageResultIsBoundByItsLabel(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "compare the prices", Inputs: []string{"e2", "e2.prices[*]", "e1.supplier"}},
		{Query: "write the CSV", DependsOn: []int{0}, Inputs: []string{"s0"}},
	}}
	if d := bindingDetails(Validate(Input{Plan: p, Earlier: earlierResults()})); len(d) != 0 {
		t.Fatalf("valid references refused: %v", d)
	}
}

func TestValidate_RejectedReferenceListsValidOnesAndTheClosest(t *testing.T) {
	cases := []struct {
		input, closest string
	}{
		{"s1.step_3", "Closest: e2"},            // stage 1 step 3, written as a step of this plan
		{"led_07f59bbbbbbb2222", "Closest: e2"}, // the ledger id of e2
		{"e7", "Closest: e"},                    // an unknown label
		{"stage1.s0", "Closest: e1"},            // stage 1 step 0 in another syntax
	}
	for _, tc := range cases {
		p := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "one"}, {Query: "two", Inputs: []string{tc.input}}}}
		d := bindingDetails(Validate(Input{Plan: p, Earlier: earlierResults()}))
		if len(d) != 1 {
			t.Fatalf("%s: want one binding violation, got %v", tc.input, d)
		}
		for _, want := range []string{tc.input, "Valid here: s0, e1 (stage 1 step 0), e2 (stage 1 step 3)", tc.closest} {
			if !strings.Contains(d[0], want) {
				t.Fatalf("%s: detail lacks %q:\n%s", tc.input, want, d[0])
			}
		}
	}
}

// s<N> still names a step of THIS plan and needs depends_on; an e<K> with no
// earlier stage (stage 1) is refused.
func TestValidate_OwnStepReferencesKeepTheirRules(t *testing.T) {
	p := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "one"}, {Query: "two", Inputs: []string{"s0"}}}}
	d := bindingDetails(Validate(Input{Plan: p}))
	if len(d) != 1 || !strings.Contains(d[0], "add 0 to depends_on") {
		t.Fatalf("missing depends_on: %v", d)
	}
	p = &domain.ExecutionPlan{Steps: []domain.Step{{Query: "one", Inputs: []string{"e1"}}}}
	d = bindingDetails(Validate(Input{Plan: p}))
	if len(d) != 1 || !strings.Contains(d[0], "not an earlier result") || !strings.Contains(d[0], "none (") {
		t.Fatalf("e1 on the first stage: %v", d)
	}
}

func TestParseInputRef_OneGrammar(t *testing.T) {
	for _, tc := range []struct {
		in      string
		ok      bool
		earlier bool
		n       int
		name    string
		list    bool
	}{
		{"s0", true, false, 0, "", false},
		{"s2.overdue[*]", true, false, 2, "overdue", true},
		{"e3", true, true, 3, "", false},
		{"e3.prices[*]", true, true, 3, "prices", true},
		{"e0", false, false, 0, "", false},
		{"led_07f59", false, false, 0, "", false},
		{"stage1.s3", false, false, 0, "", false},
		{"s1.step 3", false, false, 0, "", false},
	} {
		r, ok := domain.ParseInputRef(tc.in)
		if ok != tc.ok || (ok && (r.Earlier != tc.earlier || r.N != tc.n || r.Name != tc.name || r.List != tc.list)) {
			t.Fatalf("%q: got %+v ok=%v", tc.in, r, ok)
		}
	}
}
