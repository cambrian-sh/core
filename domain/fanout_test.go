package domain

import (
	"errors"
	"reflect"
	"testing"
)

func iptr(i int) *int { return &i }

func TestExtractFanOutItems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"json array of strings", `["intro","rotor","tail"]`, []string{"intro", "rotor", "tail"}},
		{"json array of objects (id)", `[{"id":"a.md","exists":false},{"id":"b.md"}]`, []string{"a.md", "b.md"}},
		{"json array of objects (path fallback)", `[{"path":"x/y.go"}]`, []string{"x/y.go"}},
		{"wrapper object known key", `{"missing":["s1","s2"],"count":2}`, []string{"s1", "s2"}},
		{"wrapper object single array field", `{"whatever":["only"]}`, []string{"only"}},
		{"newline fallback", "alpha\n  beta \n\ngamma", []string{"alpha", "beta", "gamma"}},
		{"dedupe preserves order", `["a","b","a"]`, []string{"a", "b"}},
		{"empty", "   ", nil},
		{"numbers", `[1,2]`, []string{"1", "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFanOutItems(tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractFanOutItems(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// An object with two array fields is ambiguous — we must refuse to guess and fall back to
// lines rather than silently pick one.
func TestExtractFanOutItems_AmbiguousObjectFallsBack(t *testing.T) {
	t.Parallel()
	got := ExtractFanOutItems(`{"a":["x"],"b":["y"]}`)
	if len(got) == 1 && (got[0] == "x" || got[0] == "y") {
		t.Errorf("ambiguous object must not be guessed; got %v", got)
	}
}

// The canonical case: scan → parametric write → summarize. Children are APPENDED at fresh
// indices; the parametric node stays inert; the dependent barriers over all children.
func TestExpandFanOut_ExpandsAndRemaps(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan the helicopter folder"},                                          // 0
		{Query: "write the file for {item}", FanOutOver: iptr(0), DependsOn: []int{0}}, // 1 (parametric)
		{Query: "summarize what was written", DependsOn: []int{1}},                     // 2 (reduce)
	}}
	got, err := ExpandFanOut(plan, 1, []string{"intro", "rotor", "tail"}, 0)
	if err != nil {
		t.Fatalf("ExpandFanOut: %v", err)
	}
	if len(got.Steps) != 6 { // 3 original + 3 appended children
		t.Fatalf("want 6 steps, got %d", len(got.Steps))
	}
	// Original steps keep their indices; the parametric node stays inert.
	if got.Steps[0].Query != "scan the helicopter folder" || got.Steps[2].Query != "summarize what was written" {
		t.Errorf("original step indices not preserved")
	}
	if got.Steps[1].FanOutOver == nil {
		t.Errorf("the parametric node must remain inert (FanOutOver tagged) so the executor never dispatches it")
	}
	// Children appended at 3,4,5, each concrete and depending on the source (0).
	wantChildren := []string{"write the file for intro", "write the file for rotor", "write the file for tail"}
	for c, w := range wantChildren {
		idx := 3 + c
		if got.Steps[idx].Query != w {
			t.Errorf("child %d query = %q, want %q", idx, got.Steps[idx].Query, w)
		}
		if got.Steps[idx].FanOutOver != nil {
			t.Errorf("child %d must be concrete", idx)
		}
		if want := []string{"intro", "rotor", "tail"}[c]; got.Steps[idx].FanOutItem != want {
			t.Errorf("child %d FanOutItem = %q, want %q", idx, got.Steps[idx].FanOutItem, want)
		}
		if !reflect.DeepEqual(got.Steps[idx].DependsOn, []int{0}) {
			t.Errorf("child %d deps = %v, want [0]", idx, got.Steps[idx].DependsOn)
		}
	}
	// The reduce step now barriers over every child instead of the parametric node.
	if !reflect.DeepEqual(got.Steps[2].DependsOn, []int{3, 4, 5}) {
		t.Errorf("reduce deps = %v, want [3 4 5]", got.Steps[2].DependsOn)
	}
	if len(plan.Steps) != 3 {
		t.Errorf("ExpandFanOut mutated the input plan")
	}
}

func TestExpandFanOut_CustomVar(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "list"},
		{Query: "patch {file} carefully", FanOutOver: iptr(0), FanOutVar: "file", DependsOn: []int{0}},
	}}
	got, err := ExpandFanOut(plan, 1, []string{"a.go"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[2].Query != "patch a.go carefully" { // appended child at index 2
		t.Errorf("custom var not substituted: %q", got.Steps[2].Query)
	}
}

// Discovery found nothing: no children are appended and the dependent KEEPS its dependency
// on the inert node — the executor marks it completed (no stall) and reports it as an empty
// upstream result rather than dropping it from the dependent's inputs.
func TestExpandFanOut_EmptySetRemovesNode(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan"},
		{Query: "write {item}", FanOutOver: iptr(0), DependsOn: []int{0}},
		{Query: "report", DependsOn: []int{1}},
	}}
	got, err := ExpandFanOut(plan, 1, nil, 0)
	if err != nil {
		t.Fatalf("empty set must not error: %v", err)
	}
	if len(got.Steps) != 3 { // no children appended; inert node lingers
		t.Fatalf("want 3 steps, got %d", len(got.Steps))
	}
	if got.Steps[2].Query != "report" {
		t.Errorf("step 2 = %q, want report", got.Steps[2].Query)
	}
	if d := got.Steps[2].DependsOn; len(d) != 1 || d[0] != 1 {
		t.Errorf("dependency on the empty node must be kept, got %v", d)
	}
}

// No silent truncation: over-width is a structured error the caller routes to replan.
func TestExpandFanOut_WidthCap(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan"},
		{Query: "write {item}", FanOutOver: iptr(0), DependsOn: []int{0}},
	}}
	_, err := ExpandFanOut(plan, 1, []string{"a", "b", "c"}, 2)
	var we *FanOutWidthError
	if !errors.As(err, &we) {
		t.Fatalf("want *FanOutWidthError, got %v", err)
	}
	if we.Width != 3 || we.MaxWidth != 2 || we.StepIndex != 1 {
		t.Errorf("unexpected width error: %+v", we)
	}
}

func TestExpandFanOut_Rejects(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{{Query: "plain"}}}
	if _, err := ExpandFanOut(plan, 0, []string{"a"}, 0); err == nil {
		t.Error("expanding a non-parametric step must error")
	}
	if _, err := ExpandFanOut(plan, 9, []string{"a"}, 0); err == nil {
		t.Error("out-of-range index must error")
	}
	if _, err := ExpandFanOut(nil, 0, nil, 0); err == nil {
		t.Error("nil plan must error")
	}
}

func TestPendingFanOuts(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan"},
		{Query: "write {item}", FanOutOver: iptr(0)},
		{Query: "other"},
		{Query: "check {item}", FanOutOver: iptr(0)}, // a SIBLING fan-out over the same source
	}}
	if got := PendingFanOuts(plan, 0); !reflect.DeepEqual(got, []int{1, 3}) {
		t.Errorf("PendingFanOuts(src=0) = %v, want [1 3] — every fan-out over the source", got)
	}
	if got := PendingFanOuts(plan, 1); got != nil {
		t.Errorf("PendingFanOuts(src=1) = %v, want nil", got)
	}
	if got := PendingFanOuts(nil, 0); got != nil {
		t.Errorf("nil plan must yield nil, got %v", got)
	}
}

// Two parametric steps over ONE source, and a step depending on both: expanding them in
// turn (as the executor does) leaves the dependent barriering over BOTH child sets
// (T-20261107-only-the-first-fan-out-step-over-a-source).
func TestExpandFanOut_SiblingsOverOneSource(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "aging report"}, // 0
		{Query: "contact for {item}", FanOutOver: iptr(0), DependsOn: []int{0}},  // 1
		{Query: "invoices for {item}", FanOutOver: iptr(0), DependsOn: []int{0}}, // 2
		{Query: "draft reminders", DependsOn: []int{1, 2}},                       // 3
	}}
	items := []string{"acme", "bolt"}
	cur := plan
	for _, fi := range PendingFanOuts(plan, 0) {
		next, err := ExpandFanOut(cur, fi, items, 0)
		if err != nil {
			t.Fatalf("ExpandFanOut(%d): %v", fi, err)
		}
		cur = next
	}
	if len(cur.Steps) != 8 {
		t.Fatalf("want 4 + 2 + 2 steps, got %d", len(cur.Steps))
	}
	wantQ := []string{"contact for acme", "contact for bolt", "invoices for acme", "invoices for bolt"}
	for c, q := range wantQ {
		if got := cur.Steps[4+c].Query; got != q {
			t.Errorf("child %d query = %q, want %q", 4+c, got, q)
		}
	}
	if got := cur.Steps[3].DependsOn; !reflect.DeepEqual(got, []int{4, 5, 6, 7}) {
		t.Errorf("the dependent must barrier over both child sets, deps = %v", got)
	}
}

// A parametric step whose query lacks the {item} token must NEVER become N identical
// children: substitution is a no-op, so each child would carry the whole-set instruction
// and a side-effecting step would repeat every write N times
// (T-20261107-fan-out-step-without-an-item-placeholder). It is demoted to ONE concrete
// step over the whole set instead.
func TestExpandFanOut_UntemplatedStepIsDemotedNotMultiplied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		step Step
	}{
		{"default var", Step{Query: "For each incident lacking an OPS issue, create one", FanOutOver: iptr(0), DependsOn: []int{0}}},
		{"custom var, item token used", Step{Query: "create an issue for {item}", FanOutOver: iptr(0), FanOutVar: "inc", DependsOn: []int{0}}},
		{"no dependency declared", Step{Query: "create the issues", FanOutOver: iptr(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &ExecutionPlan{Steps: []Step{
				{Query: "find incidents"},
				tc.step,
				{Query: "summarise", DependsOn: []int{1}},
			}}
			got, err := ExpandFanOut(plan, 1, []string{"INC-1040", "INC-1043", "INC-1046"}, 2) // width cap below 3 is moot
			if err != nil {
				t.Fatalf("ExpandFanOut: %v", err)
			}
			if len(got.Steps) != 3 {
				t.Fatalf("an untemplated step must not expand: %d steps, want 3", len(got.Steps))
			}
			queries := map[string]int{}
			for _, s := range got.Steps {
				queries[s.Query]++
			}
			if queries[tc.step.Query] != 1 {
				t.Errorf("the whole-set query must appear exactly once, got %d", queries[tc.step.Query])
			}
			d := got.Steps[1]
			if d.FanOutOver != nil || d.FanOutVar != "" || d.FanOutItem != "" {
				t.Errorf("demoted step must be concrete and not a child: %+v", d)
			}
			if !reflect.DeepEqual(d.DependsOn, []int{0}) {
				t.Errorf("demoted step must depend on its source exactly once, deps = %v", d.DependsOn)
			}
			if !reflect.DeepEqual(got.Steps[2].DependsOn, []int{1}) {
				t.Errorf("a dependent keeps depending on the demoted step, deps = %v", got.Steps[2].DependsOn)
			}
			if plan.Steps[1].FanOutOver == nil {
				t.Error("ExpandFanOut mutated the input plan")
			}
		})
	}
}

func TestStep_FanOutTemplated(t *testing.T) {
	t.Parallel()
	if !(Step{Query: "write {item}"}).FanOutTemplated() {
		t.Error("{item} is the default token")
	}
	if !(Step{Query: "patch {file}", FanOutVar: "file"}).FanOutTemplated() {
		t.Error("{file} with FanOutVar=file is templated")
	}
	if (Step{Query: "patch {item}", FanOutVar: "file"}).FanOutTemplated() {
		t.Error("{item} is not the token when FanOutVar=file")
	}
	if (Step{Query: "write all of them"}).FanOutTemplated() {
		t.Error("no token ⇒ not templated")
	}
}

// Clone must carry the parametric fields — a replan that dropped them would silently
// turn a fan-out node into a literal one-step "write the file for {item}".
func TestClone_PreservesFanOut(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{{Query: "write {x}", FanOutOver: iptr(3), FanOutVar: "x"}}}
	c := plan.Clone()
	if c.Steps[0].FanOutOver == nil || *c.Steps[0].FanOutOver != 3 || c.Steps[0].FanOutVar != "x" {
		t.Fatalf("Clone dropped fan-out fields: %+v", c.Steps[0])
	}
	*c.Steps[0].FanOutOver = 9 // must not alias the original
	if *plan.Steps[0].FanOutOver != 3 {
		t.Error("Clone aliased the FanOutOver pointer")
	}
}
