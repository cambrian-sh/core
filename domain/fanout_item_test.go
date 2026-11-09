package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ADR-0145 D6.3 / T-20261108-execute-returns-a-structured-result-the-sdk: a fan-out
// child depends on the SOURCE step and receives its whole item object. Before, a
// child inherited only the parametric node's own DependsOn (domain/fanout.go
// ExpandFanOut), so a node the planner did not wire to its source produced children
// that never saw the record their item names.
func TestExpandFanOut_ChildrenDependOnTheSource(t *testing.T) {
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "list overdue customers"},                                  // 0 source
		{Query: "load the reminder template"},                              // 1
		{Query: "remind {item}", FanOutOver: iptr(0), DependsOn: []int{1}}, // 2: no dep on 0
	}}
	got, err := ExpandFanOut(plan, 2, []string{"C-4", "C-7"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{3, 4} {
		if !reflect.DeepEqual(got.Steps[idx].DependsOn, []int{1, 0}) {
			t.Errorf("child %d deps = %v, want [1 0] (own deps + the source)", idx, got.Steps[idx].DependsOn)
		}
	}
	// A node that already depends on its source does not list it twice.
	plan.Steps[2].DependsOn = []int{0, 1}
	got, _ = ExpandFanOut(plan, 2, []string{"C-4"}, 0)
	if !reflect.DeepEqual(got.Steps[3].DependsOn, []int{0, 1}) {
		t.Errorf("deps = %v, want [0 1]", got.Steps[3].DependsOn)
	}
	// A scalar item is its own object.
	if got.Steps[3].FanOutItemJSON != `"C-4"` {
		t.Errorf("scalar item json = %q", got.Steps[3].FanOutItemJSON)
	}
}

func TestResolveFanOut_ChildReceivesItsWholeObject(t *testing.T) {
	src := "Found three.\n```json\n" + `[{"customer_id":"C-4","amount":1250.5,"due":"2026-10-01"},` +
		`{"customer_id":"C-7","amount":80,"due":"2026-10-12"}]` + "\n```"
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "list overdue"},
		{Query: "remind {item}", FanOutOver: iptr(0), DependsOn: []int{0}, RequiredEffects: []string{"read"}},
	}}
	out, items, outcome, err := ResolveFanOut(plan, 1, src, 0)
	if err != nil || outcome != FanOutExpanded {
		t.Fatalf("outcome %v err %v", outcome, err)
	}
	if !reflect.DeepEqual(items, []string{"C-4", "C-7"}) {
		t.Fatalf("items = %v", items)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out.Steps[2].FanOutItemJSON), &obj); err != nil {
		t.Fatalf("child item json %q: %v", out.Steps[2].FanOutItemJSON, err)
	}
	if obj["customer_id"] != "C-4" || obj["amount"] != 1250.5 || obj["due"] != "2026-10-01" {
		t.Fatalf("child 0 item = %v, want the whole C-4 record", obj)
	}
	if out.Steps[3].FanOutItem != "C-7" || out.Steps[3].Query != "remind C-7" {
		t.Fatalf("child 1 = %+v", out.Steps[3])
	}
	// The item survives a plan clone (replan / freeze).
	if c := out.Clone(); c.Steps[2].FanOutItemJSON != out.Steps[2].FanOutItemJSON {
		t.Fatal("Clone dropped FanOutItemJSON")
	}
}

func TestFanOutItemObjects(t *testing.T) {
	objs := FanOutItemObjects(`{"customers":[{"name":"Acme","city":"Ankara"},{"name":"Acme","city":"Izmir"},"plain"]}`)
	if objs["Acme"] != `{"city":"Ankara","name":"Acme"}` {
		t.Errorf("first element of a repeated label wins: %q", objs["Acme"])
	}
	if objs["plain"] != `"plain"` {
		t.Errorf("scalar: %q", objs["plain"])
	}
	if FanOutItemObjects("no list here") != nil {
		t.Error("prose has no objects")
	}
}
