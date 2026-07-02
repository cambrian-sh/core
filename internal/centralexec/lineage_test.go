package centralexec

import "testing"

// Credit attribution over the intent-lineage graph (ADR-0037 D12/D13): the
// worker that executed a sub-goal earns execution credit; the parent that
// framed it earns decomposition credit scored counterfactually as information
// added. A parent never earns execution credit for a child's work (0037-09).
func TestAttributeOutcome_TwoChannels(t *testing.T) {
	c := NewYieldCoordinator(0.1)
	root := c.OpenRoot([]float32{1, 0, 0}) // parent intent
	if err := c.BindResource(root, "manager"); err != nil {
		t.Fatalf("bind root: %v", err)
	}
	// A genuinely narrower carve-out (distinct from the parent).
	child, err := c.Yield(root, SubGoal{Intent: "narrow distinct subtask"}, []float32{0, 1, 0})
	if err != nil {
		t.Fatalf("yield: %v", err)
	}
	if err := c.BindResource(child, "worker"); err != nil {
		t.Fatalf("bind child: %v", err)
	}

	cr := c.AttributeOutcome(child, 1.0)

	if cr.Execution.ResourceID != "worker" || cr.Execution.Amount != 1.0 {
		t.Errorf("execution credit = %+v, want worker=1.0", cr.Execution)
	}
	if cr.Decomposition.ResourceID != "manager" {
		t.Errorf("decomposition credit goes to %q, want manager", cr.Decomposition.ResourceID)
	}
	if cr.Decomposition.Amount <= 0 {
		t.Errorf("decomposition credit = %v, want > 0 for an informative carve-out", cr.Decomposition.Amount)
	}
	if cr.Execution.ResourceID == "manager" {
		t.Error("manager (parent) must NEVER earn execution credit for the child's work")
	}
}

// A pass-through parent — yields a sub-goal that barely differs from its own
// task — adds ~no information and earns ~0 decomposition credit, blocking the
// laundering vector (ADR-0037 D12, metric 8).
func TestAttributeOutcome_PassThroughEarnsNoDecompositionCredit(t *testing.T) {
	c := NewYieldCoordinator(0.1)
	root := c.OpenRoot([]float32{1, 0, 0})
	_ = c.BindResource(root, "lazy")
	// Slips past the livelock guard (cosine 0.866 < 0.9) but is near-identical:
	// information added is tiny.
	child, err := c.Yield(root, SubGoal{Intent: "basically the same task"}, []float32{0.866, 0.5, 0})
	if err != nil {
		t.Fatalf("yield: %v", err)
	}
	_ = c.BindResource(child, "worker")

	cr := c.AttributeOutcome(child, 1.0)

	if cr.Execution.ResourceID != "worker" {
		t.Errorf("execution credit = %q, want worker", cr.Execution.ResourceID)
	}
	if cr.Decomposition.Amount > 0.2 {
		t.Errorf("pass-through decomposition credit = %v, want ~0 (laundering blocked)", cr.Decomposition.Amount)
	}
}
