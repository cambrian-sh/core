package domain

import (
	"reflect"
	"strings"
	"testing"
)

// proseSource is the step-2 output of o2-incidents-to-issues #3 (run
// 20261107T103843Z_usecases_full, kernel-U2 13:55:59-13:56:15): a thought step's
// markdown answer. Split into lines it made 15 children, every one a write.
const proseSource = `## Comparison Results

# Missing OPS Issues for Unresolved Incidents

Based on the unresolved incidents from the #incidents channel and the existing OPS issues, the following incidents **do not yet have associated OPS issues**:

| Incident ID | Name | Location | Reported By | Date Reported |
|---|---|---|---|---|
| INC-1040 | Temperature excursion during inbound transit | Ashvale | Mara | 2026-10-30 |
| INC-1043 | Label mismatch between carton and pallet | Dunmere | Tobin | 2026-11-01 |
| INC-1046 | Product stored outside designated location | Calderwick | Ines | 2026-11-03 |

These three incidents need OPS issues.`

// writeFanOutPlan is the o2 #3 plan shape: reads, a thought that compares them, and a
// parametric write over the thought.
func writeFanOutPlan(effects []string, thought bool) *ExecutionPlan {
	return &ExecutionPlan{Steps: []Step{
		{Query: "Read the #incidents channel", RequiredEffects: []string{"read"}},
		{Query: "List the open OPS issues", RequiredEffects: []string{"read"}},
		{Query: "Compare and identify incidents without an OPS issue", IsThought: true, DependsOn: []int{0, 1}, RequiredEffects: []string{}},
		{Query: "For the incident {item}, create a new OPS issue", DependsOn: []int{2}, FanOutOver: iptr(2), RequiredEffects: effects, IsThought: thought},
		{Query: "Summarise the created issues", IsThought: true, DependsOn: []int{3}},
	}}
}

// The bug: a side-effecting fan-out over prose must not be split into lines. It runs
// ONCE over the whole source, with its {item} token turned into a reference to the
// source list and the exactly-once instruction.
func TestResolveFanOut_WriteOverProseIsDemotedNotSplit(t *testing.T) {
	t.Parallel()
	plan := writeFanOutPlan([]string{"write"}, false)
	out, items, how, err := ResolveFanOut(plan, 3, proseSource, 50)
	if err != nil {
		t.Fatal(err)
	}
	if how != FanOutDemotedUnstructured {
		t.Fatalf("outcome = %v, want FanOutDemotedUnstructured", how)
	}
	if items != nil {
		t.Errorf("a demoted step uses no items; got %v", items)
	}
	if len(out.Steps) != len(plan.Steps) {
		t.Fatalf("demotion must append nothing: %d steps, want %d", len(out.Steps), len(plan.Steps))
	}
	st := out.Steps[3]
	if st.FanOutOver != nil || st.FanOutVar != "" {
		t.Errorf("demoted step is still parametric: %+v", st)
	}
	if !reflect.DeepEqual(st.DependsOn, []int{2}) {
		t.Errorf("deps = %v, want [2]", st.DependsOn)
	}
	if strings.Contains(st.Query, "{item}") || !strings.Contains(st.Query, "(each item listed in the result of step 2)") ||
		!strings.Contains(st.Query, "never twice for the same item") {
		t.Errorf("demoted query = %q", st.Query)
	}
	if plan.Steps[3].FanOutOver == nil || !strings.Contains(plan.Steps[3].Query, "{item}") {
		t.Error("ResolveFanOut must not mutate its input plan")
	}
	if !reflect.DeepEqual(out.Steps[4].DependsOn, []int{3}) {
		t.Errorf("the summary still depends on the (now concrete) step: %v", out.Steps[4].DependsOn)
	}
}

// A step that declared no effects (legacy / authored plan) is treated as side-effecting.
func TestResolveFanOut_UndeclaredEffectsOverProseIsDemoted(t *testing.T) {
	t.Parallel()
	_, _, how, err := ResolveFanOut(writeFanOutPlan(nil, false), 3, proseSource, 50)
	if err != nil || how != FanOutDemotedUnstructured {
		t.Fatalf("outcome = %v, err %v; want FanOutDemotedUnstructured", how, err)
	}
}

// A structured source still fans a write step out, one child per item — bare JSON or
// one fenced JSON block behind a lead-in sentence.
func TestResolveFanOut_WriteOverStructuredListExpands(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"bare array":     `["INC-1040","INC-1043","INC-1046"]`,
		"wrapper":        `{"missing":[{"id":"INC-1040"},{"id":"INC-1043"},{"id":"INC-1046"}]}`,
		"fenced w/ lead": "These incidents lack an issue:\n```json\n[\"INC-1040\", \"INC-1043\", \"INC-1046\"]\n```\n",
	} {
		t.Run(name, func(t *testing.T) {
			plan := writeFanOutPlan([]string{"write"}, false)
			out, items, how, err := ResolveFanOut(plan, 3, src, 50)
			if err != nil || how != FanOutExpanded {
				t.Fatalf("outcome = %v, err %v", how, err)
			}
			want := []string{"INC-1040", "INC-1043", "INC-1046"}
			if !reflect.DeepEqual(items, want) {
				t.Fatalf("items = %v, want %v", items, want)
			}
			if len(out.Steps) != len(plan.Steps)+3 {
				t.Fatalf("%d steps, want %d", len(out.Steps), len(plan.Steps)+3)
			}
			if q := out.Steps[5].Query; q != "For the incident INC-1040, create a new OPS issue" {
				t.Errorf("child query = %q", q)
			}
		})
	}
}

// An empty source expands to no children (unchanged behaviour), it is not demoted.
func TestResolveFanOut_EmptySourceExpandsToNothing(t *testing.T) {
	t.Parallel()
	plan := writeFanOutPlan([]string{"write"}, false)
	out, items, how, err := ResolveFanOut(plan, 3, "  \n ", 50)
	if err != nil || how != FanOutExpanded || len(items) != 0 || len(out.Steps) != len(plan.Steps) {
		t.Fatalf("outcome = %v items %v steps %d err %v", how, items, len(out.Steps), err)
	}
	if out.Steps[3].FanOutOver == nil {
		t.Error("an empty expansion leaves the node inert (parametric), to be marked completed")
	}
}

// A read-only (or thought) fan-out may still split free text, but only its list lines:
// headings, table header and separator rows and lead-in lines are not items.
func TestResolveFanOut_ReadOnlyOverProseUsesListLinesOnly(t *testing.T) {
	t.Parallel()
	src := "# Files to review\n\nThe following files changed:\n- a/b.go\n- c/d.go\n2. e/f.go\n---\n| Path | Lines |\n|---|---|\n| g/h.go | 12 |"
	want := []string{"a/b.go", "c/d.go", "e/f.go", "| g/h.go | 12 |"}
	for name, plan := range map[string]*ExecutionPlan{
		"read":    writeFanOutPlan([]string{"read"}, false),
		"thought": writeFanOutPlan(nil, true),
	} {
		t.Run(name, func(t *testing.T) {
			_, items, how, err := ResolveFanOut(plan, 3, src, 50)
			if err != nil || how != FanOutExpanded {
				t.Fatalf("outcome = %v, err %v", how, err)
			}
			if !reflect.DeepEqual(items, want) {
				t.Errorf("items = %q, want %q", items, want)
			}
		})
	}
}

func TestResolveFanOut_UntemplatedIsDemoted(t *testing.T) {
	t.Parallel()
	plan := writeFanOutPlan([]string{"write"}, false)
	plan.Steps[3].Query = "Create an OPS issue for each incident"
	out, _, how, err := ResolveFanOut(plan, 3, `["a","b"]`, 50)
	if err != nil || how != FanOutDemotedUntemplated || len(out.Steps) != len(plan.Steps) {
		t.Fatalf("outcome = %v err %v", how, err)
	}
	if out.Steps[3].Query != "Create an OPS issue for each incident" {
		t.Errorf("an untemplated query is run as written; got %q", out.Steps[3].Query)
	}
}

func TestResolveFanOut_OverWidth(t *testing.T) {
	t.Parallel()
	_, _, _, err := ResolveFanOut(writeFanOutPlan([]string{"write"}, false), 3, `["a","b","c"]`, 2)
	if _, ok := err.(*FanOutWidthError); !ok {
		t.Fatalf("err = %v, want *FanOutWidthError", err)
	}
}

func TestExtractStructuredFanOutItems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
		ok   bool
	}{
		{"array", `["x","y"]`, []string{"x", "y"}, true},
		{"fenced", "```json\n[\"x\"]\n```", []string{"x"}, true},
		{"fenced no info string", "Here:\n```\n{\"items\":[\"x\"]}\n```\nDone.", []string{"x"}, true},
		{"two fences, none ending the reply, are ambiguous", "```json\n[\"x\"]\n```\n```json\n[\"y\"]\n```\nDone.", nil, false},
		{"fenced prose", "```\nnot json\n```", nil, false},
		{"markdown list", "- x\n- y", nil, false},
		{"prose", proseSource, nil, false},
		{"empty", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractStructuredFanOutItems(tt.raw)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestStep_FanOutSideEffecting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s    Step
		want bool
	}{
		{Step{RequiredEffects: []string{"read"}}, false},
		{Step{RequiredEffects: []string{}}, false},
		{Step{RequiredEffects: []string{"read", "send"}}, true},
		{Step{RequiredEffects: []string{"write"}}, true},
		{Step{}, true}, // undeclared: unknown is treated as side-effecting
		{Step{IsThought: true}, false},
	} {
		if got := tt.s.FanOutSideEffecting(); got != tt.want {
			t.Errorf("%+v: got %v, want %v", tt.s, got, tt.want)
		}
	}
}
