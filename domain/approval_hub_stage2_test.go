package domain

import (
	"context"
	"strings"
	"testing"
	"time"
)

// relMap is a fixed ApprovalRelationships (relation → beneficiary → approvers).
type relMap map[string]map[string][]string

func (r relMap) Approvers(_ context.Context, b PrincipalRef, rel string) ([]string, error) {
	return r[rel][b.ID], nil
}

// limitPolicy routes payments above 100 (read from Args) to the supervisor —
// the shape of the premium hierarchy policy, which decorates the base.
type limitPolicy struct{ base ApprovalPolicy }

func (l limitPolicy) Route(ctx context.Context, it ApprovalItem) (ApprovalRoute, error) {
	r, err := l.base.Route(ctx, it)
	if strings.Contains(string(it.Args), `"amount":500`) {
		r.Rules = []ApprovalRule{{Kind: ApprovalRuleRelationship, Value: "supervisor"}}
		r.AllowStanding = false
	}
	return r, err
}

func stage2Hub(t *testing.T) *AddressedHub {
	t.Helper()
	p, err := NewConfigApprovalPolicy(ApprovalPolicyConfig{Rules: []ApprovalClassRule{
		{Class: "expense", Approvers: []string{"relationship:supervisor"}},
		{Class: "payment", Approvers: []string{"self"}},
		{Class: "*", Approvers: []string{"operator"}},
	}, ToolClasses: map[string]string{"expense_submit": "expense", "pay": "payment"}})
	if err != nil {
		t.Fatal(err)
	}
	return NewAddressedHub(AddressedHubConfig{Policy: p, DefaultTimeout: time.Minute})
}

// With a hierarchy installed, a relationship that resolves to nobody fails the
// request closed AT ONCE, naming the missing edge — not "not configured", and
// not a silent wait.
func TestHub_RelationshipResolvingToNobodyFailsClosedVisibly(t *testing.T) {
	ctx := context.Background()
	h := stage2Hub(t)
	h.SetRelationships(relMap{"supervisor": {"emp": {"boss"}, "self-managed": {"self-managed"}}})
	it, err := h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: UserPrincipal("loner")})
	if err != nil || it.State != ApprovalNeedsHierarchy || !strings.Contains(it.Detail, "no supervisor is recorded for user:loner") {
		t.Fatalf("no manager: %s %q %v", it.State, it.Detail, err)
	}
	// A relationship that names only the beneficiary is nobody, too.
	it, _ = h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: UserPrincipal("self-managed")})
	if it.State != ApprovalNeedsHierarchy {
		t.Fatalf("self as supervisor: %s", it.State)
	}
	it, _ = h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: UserPrincipal("emp")})
	if it.State != ApprovalPending {
		t.Fatalf("with a manager: %s %s", it.State, it.Detail)
	}
}

// SetPolicy refines routing; the limit-escalated request must not join the
// pending self-approvable one (the natural key covers rules and full args).
func TestHub_SetPolicyAndNaturalKeyCoversTheRoute(t *testing.T) {
	ctx := context.Background()
	h := stage2Hub(t)
	h.SetRelationships(relMap{"supervisor": {"emp": {"boss"}}})
	h.SetPolicy(nil) // ignored
	h.SetPolicy(limitPolicy{base: h.Policy()})
	small, _ := h.Raise(ctx, ApprovalRequest{ToolName: "pay", ArgsPreview: "same", Args: []byte(`{"amount":50}`), Beneficiary: UserPrincipal("emp")})
	big, _ := h.Raise(ctx, ApprovalRequest{ToolName: "pay", ArgsPreview: "same", Args: []byte(`{"amount":500}`), Beneficiary: UserPrincipal("emp")})
	if small.ID == big.ID {
		t.Fatal("an above-limit request joined the self-approvable pending one")
	}
	if small.RuleSummary() != "self" || big.RuleSummary() != "relationship:supervisor" {
		t.Fatalf("routes: %s / %s", small.RuleSummary(), big.RuleSummary())
	}
	// The same request again still joins (dedupe of identical requests holds).
	again, _ := h.Raise(ctx, ApprovalRequest{ToolName: "pay", ArgsPreview: "same", Args: []byte(`{"amount":50}`), Beneficiary: UserPrincipal("emp")})
	if again.ID != small.ID {
		t.Fatal("an identical request must join the pending item")
	}
	got, _ := h.Get(ctx, small.ID)
	if got.Args != nil {
		t.Fatal("Args must never be persisted")
	}
}

func TestHub_WithdrawForBeneficiary(t *testing.T) {
	ctx := context.Background()
	h := stage2Hub(t)
	h.SetRelationships(relMap{"supervisor": {"emp": {"boss"}, "other": {"boss"}}})
	mine, _ := h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: UserPrincipal("emp")})
	legacy, _ := h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: AgentPrincipal("emp"), ArgsPreview: "legacy"})
	theirs, _ := h.Raise(ctx, ApprovalRequest{ToolName: "expense_submit", Beneficiary: UserPrincipal("other")})
	n, err := h.WithdrawForBeneficiary(ctx, "emp", "deprovisioned")
	if err != nil || n != 2 {
		t.Fatalf("withdrew %d (%v), want 2 (user and legacy agent kind)", n, err)
	}
	for _, id := range []string{mine.ID, legacy.ID} {
		if it, _ := h.Get(ctx, id); it.State != ApprovalWithdrawn || it.Detail != "deprovisioned" {
			t.Fatalf("%s: %s %q", id, it.State, it.Detail)
		}
	}
	if it, _ := h.Get(ctx, theirs.ID); it.State != ApprovalPending {
		t.Fatalf("someone else's approval was withdrawn: %s", it.State)
	}
	if n, _ := h.WithdrawForBeneficiary(ctx, "", "x"); n != 0 {
		t.Fatal("an empty person withdraws nothing")
	}
}
