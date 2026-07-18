package domain

import (
	"errors"
	"reflect"
	"testing"
)

func iptr(i int) *int { return &i }

func TestExtractFanOutItems(t *testing.T) {
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
	got := ExtractFanOutItems(`{"a":["x"],"b":["y"]}`)
	if len(got) == 1 && (got[0] == "x" || got[0] == "y") {
		t.Errorf("ambiguous object must not be guessed; got %v", got)
	}
}

// The canonical case: scan → parametric write → summarize. Children are APPENDED at fresh
// indices; the parametric node stays inert; the dependent barriers over all children.
func TestExpandFanOut_ExpandsAndRemaps(t *testing.T) {
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan the helicopter folder"},                                    // 0
		{Query: "write the file for {item}", FanOutOver: iptr(0), DependsOn: []int{0}}, // 1 (parametric)
		{Query: "summarize what was written", DependsOn: []int{1}},               // 2 (reduce)
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

// Discovery found nothing: no children are appended and the dependent drops its dependency
// on the (now empty) node, so it proceeds rather than stalling forever.
func TestExpandFanOut_EmptySetRemovesNode(t *testing.T) {
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
	if len(got.Steps[2].DependsOn) != 0 {
		t.Errorf("dependency on the empty node must be dropped, got %v", got.Steps[2].DependsOn)
	}
}

// No silent truncation: over-width is a structured error the caller routes to replan.
func TestExpandFanOut_WidthCap(t *testing.T) {
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

func TestPendingFanOut(t *testing.T) {
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "scan"},
		{Query: "write {item}", FanOutOver: iptr(0)},
	}}
	if got := PendingFanOut(plan, 0); got != 1 {
		t.Errorf("PendingFanOut(src=0) = %d, want 1", got)
	}
	if got := PendingFanOut(plan, 1); got != -1 {
		t.Errorf("PendingFanOut(src=1) = %d, want -1", got)
	}
	if got := PendingFanOut(nil, 0); got != -1 {
		t.Errorf("nil plan must yield -1, got %d", got)
	}
}

// Clone must carry the parametric fields — a replan that dropped them would silently
// turn a fan-out node into a literal one-step "write the file for {item}".
func TestClone_PreservesFanOut(t *testing.T) {
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
