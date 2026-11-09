package domain

import "testing"

type fixedLeaseCeiling map[string]map[string]bool

func (f fixedLeaseCeiling) AllowsLease(lease, tool string) (bool, bool) {
	set, ok := f[lease]
	if !ok {
		return false, false
	}
	return set[tool], true
}

func TestChainLeaseCeilings_EveryBindingCeilingMustAdmit(t *testing.T) {
	comp := NewComposedLeases()
	comp.Bind("L1", WorkerComposition{Runtime: "rt", Tools: []string{"a", "b"}})
	sub := fixedLeaseCeiling{"L1": {"a": true}, "L2": {"z": true}}
	c := ChainLeaseCeilings(nil, sub, comp, (*ComposedLeases)(nil))
	cases := []struct {
		lease, tool   string
		inside, bound bool
	}{
		{"L1", "a", true, true},   // inside both
		{"L1", "b", false, true},  // the subagent toolset refuses it
		{"L1", "c", false, true},  // neither
		{"L2", "z", true, true},   // only the subagent ceiling binds
		{"L3", "a", false, false}, // nobody binds
	}
	for _, tc := range cases {
		in, b := c.AllowsLease(tc.lease, tc.tool)
		if in != tc.inside || b != tc.bound {
			t.Errorf("%s/%s = (%v,%v), want (%v,%v)", tc.lease, tc.tool, in, b, tc.inside, tc.bound)
		}
	}
	if ChainLeaseCeilings(nil, (*ComposedLeases)(nil)) != nil {
		t.Error("an empty chain must be nil (no ceiling)")
	}
}

func TestWorkerComposition_NormalizedEncodingAndID(t *testing.T) {
	a := WorkerComposition{Runtime: "rt", Tools: []string{"b", "a", "a", ""}, Skills: []string{"s2", "s1"}, StepIndex: 2}
	b := WorkerComposition{Runtime: "rt", Tools: []string{"a", "b"}, Skills: []string{"s1", "s2"}, StepIndex: 2}
	if a.ID() != b.ID() || a.Encode() != b.Encode() {
		t.Fatal("equal compositions must encode and hash equally")
	}
	got, ok := DecodeWorkerComposition(a.Encode())
	if !ok || !got.HasTool("a") || got.HasTool("c") || !got.HasSkill("s1") {
		t.Fatalf("round trip = %+v", got)
	}
	if _, ok := DecodeWorkerComposition(`{"tools":["a"]}`); ok {
		t.Fatal("a composition without a runtime must not decode")
	}
	r := NewComposedLeases()
	r.Bind("L", a)
	if in, bound := r.AllowsSkill("L", "s3"); in || !bound {
		t.Fatal("skill outside the composition admitted")
	}
	r.Forget("L")
	if _, bound := r.AllowsLease("L", "a"); bound {
		t.Fatal("forgotten lease still bound")
	}
	if !ValidLedgerKind(LedgerComposition) {
		t.Fatal("composition is not a valid ledger kind")
	}
}
