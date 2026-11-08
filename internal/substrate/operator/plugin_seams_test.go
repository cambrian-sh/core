package operator

import (
	"context"
	"testing"
)

// ADR-0141: the premium accounts plane's role table. Connect/disconnect-my-own
// are open to every role; the cross-principal list and revoke are
// Operator-only. Matched on the full method, so an OperatorConsole RPC with
// the same short name gains nothing.
func TestAccountsPlane_RoleGating(t *testing.T) {
	const svc = "/cambrian.premium.accounts.AccountsAdmin/"
	for m, want := range map[string]bool{
		svc + "StartAccountConnect":   false,
		svc + "GetAccountConnectFlow": false,
		svc + "ListMyAccounts":        false,
		svc + "DisconnectMyAccount":   false,
		svc + "ListAccounts":          true,
		svc + "RevokeAccount":         true,
		// ADR-0141 phase 2.
		svc + "ConfirmMyAccount":    false,
		svc + "ListOrgAccounts":     true,
		svc + "ListProviderClients": true,
		svc + "CreateOrgAccount":    true,
		svc + "UpdateOrgAccount":    true,
		svc + "RotateOrgAccount":    true,
		svc + "RevokeOrgAccount":    true,
		svc + "BindOrgAccount":      true,
		svc + "UnbindOrgAccount":    true,
		svc + "SetProviderClient":   true,
		svc + "ClearProviderClient": true,
		// Org OAuth install + system catalog.
		svc + "StartOrgAccountInstall":                           true,
		svc + "ListSystems":                                      false,
		svc + "ListAccountConsumers":                             true,
		"/cambrian.operator.OperatorConsole/ConfirmMyAccount":    true,
		"/cambrian.operator.OperatorConsole/StartAccountConnect": true,
		"/cambrian.operator.OperatorConsole/DisconnectMyAccount": true,
	} {
		if got := isOperatorOnly(m); got != want {
			t.Errorf("%s operator-only = %v, want %v", m, got, want)
		}
	}
}

// RunPluginMutation keeps the OperatorConsole mutation contract for a plugin
// plane: reason required, audited with the caller, deduped on command_id.
func TestRunPluginMutation_AuditsAndDedupes(t *testing.T) {
	s := newWriteService(nil, nil)
	ctx := ContextWithPrincipal(context.Background(), "root", RoleOperator)
	runs := 0
	apply := func() error { runs++; return nil }
	if _, err := s.RunPluginMutation(ctx, "c1", "", "revoke_account", "account", "a/b", apply); err == nil {
		t.Fatal("a mutation without a reason must be refused")
	}
	if d, err := s.RunPluginMutation(ctx, "c2", "left", "revoke_account", "account", "a/b", apply); err != nil || d {
		t.Fatalf("first run: deduped=%v err=%v", d, err)
	}
	if d, err := s.RunPluginMutation(ctx, "c2", "left", "revoke_account", "account", "a/b", apply); err != nil || !d {
		t.Fatalf("replay: deduped=%v err=%v", d, err)
	}
	if runs != 1 {
		t.Fatalf("apply ran %d times, want 1", runs)
	}
	rows, _ := s.audit.Query(context.Background(), AuditFilter{ActionType: "revoke_account"})
	if len(rows) != 1 || rows[0].Actor != "root" || rows[0].Reason != "left" {
		t.Fatalf("audit: %+v", rows)
	}
	if p, op, ok := CallerFromContext(ctx); !ok || p != "root" || !op {
		t.Fatalf("caller = %q %v %v", p, op, ok)
	}
	if _, _, ok := CallerFromContext(context.Background()); ok {
		t.Fatal("no principal must read as not ok")
	}
}
