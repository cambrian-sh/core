package domain

import "testing"

// T-20261108-system-prompt-skill-list-order-is-not-stable: the full skill menu is
// rendered into a worker's system prompt, so All must return one order, every call.
func TestInMemorySkillRegistryAllIsSortedByName(t *testing.T) {
	r := NewInMemorySkillRegistry()
	for _, n := range []string{"web-research", "summarise", "chart-report", "organise-files", "file-report", "exploratory-analysis"} {
		r.Register(Skill{Name: n})
	}
	want := []string{"chart-report", "exploratory-analysis", "file-report", "organise-files", "summarise", "web-research"}
	for round := 0; round < 50; round++ {
		got := r.All()
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].Name != want[i] {
				t.Fatalf("round %d: All()[%d] = %q, want %q", round, i, got[i].Name, want[i])
			}
		}
	}
}
