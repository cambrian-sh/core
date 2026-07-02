package domain

import (
	"errors"
	"math"
	"testing"
)

// With a DefaultCap, an unknown account is auto-created and enforced on first
// reserve — so every managed session is metered without pre-registering each one
// (ADR-0043 D5 wiring).
func TestBudgetLedger_DefaultCapAutoCreatesAndEnforces(t *testing.T) {
	l := NewBudgetLedger()
	l.DefaultCap = 0.05

	id, err := l.Reserve(0.04, "sess:new")
	if err != nil {
		t.Fatalf("0.04 fits the default cap 0.05: %v", err)
	}
	if got := l.Remaining("sess:new"); math.Abs(got-0.01) > 1e-9 {
		t.Errorf("after auto-create + 0.04 hold, remaining = %v, want ~0.01", got)
	}
	if _, err := l.Reserve(0.02, "sess:new"); err == nil {
		t.Error("0.02 over the remaining 0.01 must be denied")
	}
	_ = id
}

// Tracer: a reservation must fit in EVERY capped account (step AND session). If
// either lacks room, nothing is held and a BudgetExhaustedError names the failing
// account. This is the core admission guarantee of the budget regime (ADR-0043 D5).
func TestBudgetLedger_ReserveDeniesWhenOverEitherAccount(t *testing.T) {
	l := NewBudgetLedger()
	l.SetCap("step:s1", 0.10)
	l.SetCap("session:S", 1.00)

	// Fits in both → admitted.
	if _, err := l.Reserve(0.05, "step:s1", "session:S"); err != nil {
		t.Fatalf("0.05 fits both budgets, got error: %v", err)
	}

	// Would exceed the step account (0.05 held + 0.07 > 0.10), though the session
	// has room — must be denied, naming the step account.
	_, err := l.Reserve(0.07, "step:s1", "session:S")
	var be *BudgetExhaustedError
	if !errors.As(err, &be) {
		t.Fatalf("over-step reservation must return BudgetExhaustedError, got %v", err)
	}
	if be.Account != "step:s1" {
		t.Errorf("BudgetExhaustedError.Account = %q, want the failing step account", be.Account)
	}

	// All-or-nothing: the denied reservation must NOT have held anything on the
	// session account (no partial hold).
	if got := l.Remaining("session:S"); got != 0.95 {
		t.Errorf("denied reservation must not touch the session account; remaining = %v, want 0.95", got)
	}
}

// Reconcile converts a hold into actual spend that may be LESS than reserved,
// releasing the unused remainder of the hold back to the budget (ADR-0043 D6).
func TestBudgetLedger_ReconcileChargesActualAndReleasesHold(t *testing.T) {
	l := NewBudgetLedger()
	l.SetCap("session:S", 1.00)

	id, err := l.Reserve(0.50, "session:S")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// While held, the full reservation is unavailable.
	if got := l.Remaining("session:S"); got != 0.50 {
		t.Fatalf("held reservation: remaining = %v, want 0.50", got)
	}
	// Actual came in under the reservation → only the actual is spent, the rest freed.
	if err := l.Reconcile(id, 0.20); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := l.Remaining("session:S"); got != 0.80 {
		t.Errorf("after reconcile(0.20): remaining = %v, want 0.80 (0.30 hold released)", got)
	}
}

// Release drops a hold without charging — a never-reached failure or a 0-cost
// server-reached failure (ADR-0043 D7).
func TestBudgetLedger_ReleaseChargesNothing(t *testing.T) {
	l := NewBudgetLedger()
	l.SetCap("session:S", 1.00)
	id, _ := l.Reserve(0.40, "session:S")
	if err := l.Release(id); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := l.Remaining("session:S"); got != 1.00 {
		t.Errorf("after release: remaining = %v, want full 1.00 (no charge)", got)
	}
}

// Pricing estimate (the reservation) and reconcile (the actual charge) across the
// pricing kinds — including cap-on-unmeasurable and the max_units clamp (ADR-0043 D6).
func TestToolPricing_EstimateAndReconcile(t *testing.T) {
	// Flat: reserve and reconcile are both the fixed per-call cost, regardless of usage.
	flat := ToolPricing{Kind: PricingFlat, UnitCost: 0.005}
	if got := flat.Reserve(); got != 0.005 {
		t.Errorf("flat Reserve = %v, want 0.005", got)
	}
	if got := flat.Reconcile(999, true); got != 0.005 {
		t.Errorf("flat Reconcile = %v, want 0.005 (usage ignored)", got)
	}

	// Per-unit: reserve at the cap (max_units × unit_cost); reconcile to actual units.
	perUnit := ToolPricing{Kind: PricingPerUnit, UnitCost: 0.01, MaxUnitsPerCall: 10}
	if got := perUnit.Reserve(); got != 0.10 {
		t.Errorf("per_unit Reserve = %v, want 0.10 (10 × 0.01)", got)
	}
	if got := perUnit.Reconcile(3, true); got != 0.03 {
		t.Errorf("per_unit Reconcile(3 pages) = %v, want 0.03", got)
	}
	// Actual usage above the cap is clamped to the cap — never exceeds the hold.
	if got := perUnit.Reconcile(50, true); got != 0.10 {
		t.Errorf("per_unit Reconcile(50) = %v, want 0.10 (clamped to max_units)", got)
	}
	// Unmeasurable usage ⇒ cap-on-unmeasurable (charge the reserved cap).
	if got := perUnit.Reconcile(0, false); got != 0.10 {
		t.Errorf("per_unit Reconcile(unmeasured) = %v, want 0.10 (cap-on-unmeasurable)", got)
	}
}

// Failure-cost charges for work the provider did (ADR-0043 D7): never-reached is
// free; a server-reached failure uses reported usage, else 0 by default and the
// cap only under the per-server ChargeCap override.
func TestToolPricing_FailureCost(t *testing.T) {
	none := ToolPricing{Kind: PricingPerUnit, UnitCost: 0.01, MaxUnitsPerCall: 10, ChargeOnFailure: ChargeNone}
	cap := ToolPricing{Kind: PricingPerUnit, UnitCost: 0.01, MaxUnitsPerCall: 10, ChargeOnFailure: ChargeCap}

	// Never reached the server → free, regardless of the per-server policy.
	if got := none.FailureCost(false, 0, false); got != 0 {
		t.Errorf("never-reached FailureCost = %v, want 0", got)
	}
	if got := cap.FailureCost(false, 0, false); got != 0 {
		t.Errorf("never-reached FailureCost (ChargeCap) = %v, want 0", got)
	}

	// Server-reached with reported usage → charge the partial actual (both policies).
	if got := none.FailureCost(true, 3, true); got != 0.03 {
		t.Errorf("server-reached measured FailureCost = %v, want 0.03 (reported partial)", got)
	}

	// Server-reached, unmeasurable → 0 by default, the cap under the override.
	if got := none.FailureCost(true, 0, false); got != 0 {
		t.Errorf("server-reached unmeasured ChargeNone = %v, want 0 (default)", got)
	}
	if got := cap.FailureCost(true, 0, false); got != 0.10 {
		t.Errorf("server-reached unmeasured ChargeCap = %v, want 0.10 (cap override)", got)
	}
}
