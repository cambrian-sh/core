package domain

import "testing"

// ENG-468 (owner decision 2026-11-14): memory_fact is the kernel-reserved kind the
// operator contradictions surfaces exclude; business kinds are not reserved.
func TestIsReservedKnowledgeKind(t *testing.T) {
	if !IsReservedKnowledgeKind(KindMemoryFact) {
		t.Error("memory_fact must be reserved")
	}
	for _, k := range []string{"", "order", "customer_status"} {
		if IsReservedKnowledgeKind(k) {
			t.Errorf("%q must not be reserved", k)
		}
	}
}
