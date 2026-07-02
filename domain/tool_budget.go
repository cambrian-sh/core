package domain

import (
	"fmt"
	"sync"
)

// ToolPricing is the operator-configured cost model for one tool (ADR-0043 D6).
// MCP has no pricing channel, so this is supplied by operator config keyed by
// mcp:<server>/<tool>, never read from the server. It is a pure value — the
// estimate/reconcile math has no I/O — mirroring the LLM Pass-1/Pass-2 pattern
// (a reserved estimate, reconciled to actual).
type ToolPricing struct {
	Kind            PricingKind
	UnitCost        float64       // $ per unit (per_unit/token) or per call (flat)
	MaxUnitsPerCall int           // reservation cap for per_unit/token; ignored for flat
	ChargeOnFailure FailureCharge // server-reached failure policy (ADR-0043 D7)
}

// PricingKind is how a tool's cost is computed.
type PricingKind string

const (
	// PricingFlat — a fixed cost per call, known before the call.
	PricingFlat PricingKind = "flat"
	// PricingPerUnit — cost = units × UnitCost; units known only after the call
	// (e.g. Firecrawl credits per page). Reserved at the cap, reconciled to actual.
	PricingPerUnit PricingKind = "per_unit"
	// PricingToken — like per_unit, denominated in tokens.
	PricingToken PricingKind = "token"
)

// FailureCharge is the per-server policy for a server-reached failure (ADR-0043 D7).
type FailureCharge string

const (
	// ChargeNone — a server-reached failure with no reported usage costs 0 (default).
	ChargeNone FailureCharge = "none"
	// ChargeCap — a server-reached failure with no reported usage costs the cap.
	ChargeCap FailureCharge = "cap"
)

// Reserve is the pre-call admission estimate (the hold). Flat is exact;
// per_unit/token reserve MaxUnitsPerCall × UnitCost so the budget can never be
// overspent (ADR-0043 D6).
func (p ToolPricing) Reserve() float64 {
	switch p.Kind {
	case PricingFlat:
		return p.UnitCost
	case PricingPerUnit, PricingToken:
		return float64(max(p.MaxUnitsPerCall, 0)) * p.UnitCost
	default:
		return 0
	}
}

// Reconcile is the post-call actual charge. When usage is unmeasurable
// (measured=false) the charge is the reserved cap — "cap-on-unmeasurable"
// (ADR-0043 D6): work happened, so a poorly-instrumented tool fails safe toward
// the budget. Measured per_unit/token is clamped to the cap (never exceeds the hold).
func (p ToolPricing) Reconcile(actualUnits int, measured bool) float64 {
	if p.Kind == PricingFlat {
		return p.UnitCost
	}
	if !measured {
		return p.Reserve()
	}
	u := min(max(actualUnits, 0), p.MaxUnitsPerCall)
	return float64(u) * p.UnitCost
}

// FailureCost charges for the work the provider actually did (ADR-0043 D7):
// a call that never reached the server costs 0; a server-reached failure costs
// the reported usage when present, else 0 by default (ChargeNone) or the cap
// (ChargeCap). The recurrence gate (ADR-0041 D4) is the retry-storm backstop, so
// failure-cost optimizes for ledger accuracy, not behaviour-shaping.
func (p ToolPricing) FailureCost(reachedServer bool, reportedUnits int, measured bool) float64 {
	if !reachedServer {
		return 0
	}
	if measured {
		return p.Reconcile(reportedUnits, true)
	}
	if p.ChargeOnFailure == ChargeCap {
		return p.Reserve()
	}
	return 0
}

// ToolPricingSource resolves the operator-configured pricing for a tool by its
// identity (e.g. mcp:<server>/<tool>). nil/absent ⇒ the tool is unpriced and the
// budget regime does not meter it (ADR-0043 D3/D6).
type ToolPricingSource interface {
	PricingFor(toolName string) (ToolPricing, bool)
}

// MapPricingSource is a static ToolPricingSource built from operator config
// (tool identity → pricing). The composition root populates it from the MCP
// server config.
type MapPricingSource map[string]ToolPricing

// PricingFor implements ToolPricingSource.
func (m MapPricingSource) PricingFor(toolName string) (ToolPricing, bool) {
	p, ok := m[toolName]
	return p, ok
}

// BudgetExhaustedError is returned by Reserve when a hold would exceed an
// account's remaining budget. The ToolExecutor maps it to a denied tool call.
type BudgetExhaustedError struct {
	Account   string
	Need      float64
	Remaining float64
}

func (e *BudgetExhaustedError) Error() string {
	return fmt.Sprintf("budget_exhausted: account %q needs %.6f but %.6f remaining",
		e.Account, e.Need, e.Remaining)
}

// BudgetLedger is the pure accounting core of the ToolExecutor budget regime
// (ADR-0043 D5): reserve → admit/deny → reconcile, across one or more accounts
// (e.g. a per-step MaxEnergy account and a per-session account). A reservation
// must fit in EVERY named account that has a cap set; accounts without a cap are
// unconstrained (enforcement is opt-in per account). No I/O — the ToolExecutor
// supplies the caps from MaxEnergy / SessionState and attributes by session token.
type BudgetLedger struct {
	// DefaultCap, when > 0, is the cap applied to an account auto-created on its
	// first Reserve — so every managed session is metered with this per-session
	// budget without the operator pre-registering each session. 0 ⇒ accounts must
	// be created explicitly via SetCap (unknown accounts stay unconstrained).
	DefaultCap float64

	mu       sync.Mutex
	accounts map[string]*budgetAccount
	holds    map[string]*hold
	nextID   uint64
}

type budgetAccount struct {
	cap   float64
	spent float64
	held  float64
}

type hold struct {
	amount   float64
	accounts []string
}

// NewBudgetLedger constructs an empty ledger.
func NewBudgetLedger() *BudgetLedger {
	return &BudgetLedger{
		accounts: map[string]*budgetAccount{},
		holds:    map[string]*hold{},
	}
}

// SetCap sets (or resets) an account's spending cap, creating it if absent.
// An account with no cap set is unconstrained.
func (l *BudgetLedger) SetCap(account string, cap float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[account]
	if a == nil {
		a = &budgetAccount{}
		l.accounts[account] = a
	}
	a.cap = cap
}

// Remaining is an account's unspent, unheld budget (0 for an unknown account).
func (l *BudgetLedger) Remaining(account string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[account]
	if a == nil {
		return 0
	}
	return a.cap - a.spent - a.held
}

// Reserve holds `amount` against every named account that has a cap. It is
// all-or-nothing: if ANY capped account lacks room, nothing is held and a
// BudgetExhaustedError names the failing account. Returns a hold id for
// Reconcile/Release. Accounts without a cap impose no constraint.
func (l *BudgetLedger) Reserve(amount float64, accounts ...string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, acc := range accounts {
		a := l.accounts[acc]
		if a == nil {
			if l.DefaultCap > 0 {
				a = &budgetAccount{cap: l.DefaultCap}
				l.accounts[acc] = a
			} else {
				continue // unconstrained
			}
		}
		if rem := a.cap - a.spent - a.held; rem < amount {
			return "", &BudgetExhaustedError{Account: acc, Need: amount, Remaining: rem}
		}
	}
	for _, acc := range accounts {
		if a := l.accounts[acc]; a != nil {
			a.held += amount
		}
	}
	l.nextID++
	id := fmt.Sprintf("hold-%d", l.nextID)
	l.holds[id] = &hold{amount: amount, accounts: accounts}
	return id, nil
}

// Reconcile converts a hold into actual spend (actual may be less than reserved)
// across the hold's accounts, releasing the remainder of the hold.
func (l *BudgetLedger) Reconcile(holdID string, actual float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.holds[holdID]
	if h == nil {
		return fmt.Errorf("unknown hold %q", holdID)
	}
	delete(l.holds, holdID)
	for _, acc := range h.accounts {
		if a := l.accounts[acc]; a != nil {
			a.held -= h.amount
			a.spent += actual
		}
	}
	return nil
}

// Release drops a hold without charging (a never-reached failure, or a 0-cost
// server-reached failure under ChargeNone).
func (l *BudgetLedger) Release(holdID string) error {
	return l.Reconcile(holdID, 0)
}
