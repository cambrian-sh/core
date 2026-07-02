package executer

import "testing"

// ADR-0049 D10: dependency indices map to per-step TaskIDs for follows edges.
func TestDependsOnTaskIDs(t *testing.T) {
	got := dependsOnTaskIDs([]int{0, 2}, "planX")
	want := []string{"step-0-planX", "step-2-planX"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
	if dependsOnTaskIDs(nil, "planX") != nil {
		t.Error("no deps → nil")
	}
}
