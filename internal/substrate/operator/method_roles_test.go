package operator

import (
	"strings"
	"testing"
)

// The premium plugins declare their methods' classes (accounts, org directory,
// substrate); this fixture is the same table, so the tests below exercise the
// mechanism core owns with realistic declarations. The premium plugins' own tests
// pin that each plugin declares exactly these.
var premiumRolesFixture = MethodRoles{
	"/cambrian.premium.accounts.AccountsAdmin/StartAccountConnect":  MethodSelfService,
	"/cambrian.premium.accounts.AccountsAdmin/DisconnectMyAccount":  MethodSelfService,
	"/cambrian.premium.accounts.AccountsAdmin/ConfirmMyAccount":     MethodSelfService,
	"/cambrian.premium.accounts.AccountsAdmin/ListMyAccounts":       MethodMemberRead,
	"/cambrian.premium.accounts.AccountsAdmin/ListAccounts":         MethodOperatorOnly,
	"/cambrian.premium.accounts.AccountsAdmin/ListOrgAccounts":      MethodOperatorOnly,
	"/cambrian.premium.accounts.AccountsAdmin/ListProviderClients":  MethodOperatorOnly,
	"/cambrian.premium.accounts.AccountsAdmin/ListAccountConsumers": MethodOperatorOnly,
	"/cambrian.premium.org.OrgDirectoryAdmin/GetMyOrgChain":         MethodSelfService,
	"/cambrian.premium.org.OrgDirectoryAdmin/ListScimConnections":   MethodOperatorOnly,
	"/cambrian.premium.org.OrgDirectoryAdmin/GetReconcileStatus":    MethodOperatorOnly,
	"/cambrian.premium.substrate.SubstrateLane/GetOntologyDraft":    MethodOperatorOnly,
}

// Core names no premium method any more
// (T-20261113-core-operator-authz-hardcodes-premium-plane): with no declarations
// a premium method follows the naming rule — the formerly hard-coded self-service
// and member surfaces are commands for a Viewer and unreachable for a Member, and
// the formerly hard-coded operator-only reads are open reads. Declaring restores
// each class.
func TestMethodRoles_UndeclaredPremiumMethodsFollowTheNamingRule(t *testing.T) {
	const acc = "/cambrian.premium.accounts.AccountsAdmin/"
	if !isOperatorOnly(acc+"StartAccountConnect", nil) {
		t.Fatal("undeclared StartAccountConnect must be a command (Operator-only)")
	}
	if isOperatorOnly(acc+"ListAccounts", nil) {
		t.Fatal("undeclared ListAccounts reads like a read and must follow the naming rule")
	}
	if memberMayCall(acc+"ListMyAccounts", nil) {
		t.Fatal("a Member must reach no undeclared premium method")
	}
	// Declared.
	if isOperatorOnly(acc+"StartAccountConnect", premiumRolesFixture) {
		t.Fatal("declared self-service must be open to every role")
	}
	if !isOperatorOnly(acc+"ListAccounts", premiumRolesFixture) {
		t.Fatal("declared operator-only read must refuse Viewers")
	}
	for _, m := range []string{"StartAccountConnect", "ListMyAccounts"} {
		if !memberMayCall(acc+m, premiumRolesFixture) {
			t.Fatalf("declared %s must be reachable by a Member", m)
		}
	}
	if memberMayCall(acc+"ListAccounts", premiumRolesFixture) {
		t.Fatal("an operator-only read must not be reachable by a Member")
	}
	// member_read stays an ordinary read for Viewers.
	if isOperatorOnly(acc+"ListMyAccounts", premiumRolesFixture) {
		t.Fatal("member_read is a read for Viewers")
	}
}

// A declaration can never reclassify an OperatorConsole method: roles are
// consulted for /cambrian.premium. methods only, and FullMethodName refuses a
// non-premium service at registration.
func TestMethodRoles_CannotTouchOperatorConsole(t *testing.T) {
	forged := MethodRoles{operatorMethodPrefix + "QueryLogs": MethodSelfService, operatorMethodPrefix + "Logout": MethodOperatorOnly}
	if !isOperatorOnly(operatorMethodPrefix+"QueryLogs", forged) {
		t.Fatal("a declaration must not open QueryLogs to Viewers")
	}
	if isOperatorOnly(operatorMethodPrefix+"Logout", forged) || memberMayCall(operatorMethodPrefix+"QueryLogs", forged) {
		t.Fatal("a declaration must not change an OperatorConsole method's class")
	}
	for _, svc := range []string{"cambrian.OperatorConsole", "cambrian.Orchestrator", "other.premium.X", "cambrian.premium.a/b", ""} {
		if _, err := FullMethodName(svc, "M"); err == nil {
			t.Errorf("FullMethodName(%q) must be refused", svc)
		}
	}
	if _, err := FullMethodName("cambrian.premium.accounts.AccountsAdmin", "a/b"); err == nil {
		t.Error("a method name with a slash must be refused")
	}
	full, err := FullMethodName("cambrian.premium.accounts.AccountsAdmin", "ListAccounts")
	if err != nil || full != "/cambrian.premium.accounts.AccountsAdmin/ListAccounts" {
		t.Fatalf("FullMethodName = %q, %v", full, err)
	}
	if !strings.HasPrefix(full, premiumMethodPrefix) {
		t.Fatal("full name must carry the premium prefix")
	}
	if MethodRole("admin").Valid() || !MethodOperatorOnly.Valid() {
		t.Fatal("Valid must accept exactly the three classes")
	}
}
