package app

import (
	"strings"
	"testing"
)

// Registry.DeclareOperatorMethodRoles (owner decision 2026-11-14,
// T-20261113-core-operator-authz-hardcodes-premium-plane): each premium plugin
// declares the operator-role class of its own methods; the declarations reach the
// composition keyed by full method, and malformed or conflicting ones fail the boot.

type rolesPlugin struct {
	name    string
	service string
	roles   map[string]OperatorMethodRole
}

func (p *rolesPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: p.name, DisplayName: p.name, Version: "test"}
}

func (p *rolesPlugin) Register(r *Registry) error {
	return r.DeclareOperatorMethodRoles(p.service, p.roles)
}

func TestDeclareOperatorMethodRoles_ReachesTheComposition(t *testing.T) {
	composed, err := applyPlugins(Options{Plugins: []Plugin{
		&rolesPlugin{name: "accounts", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"StartAccountConnect": OperatorMethodSelfService, "ListAccounts": OperatorMethodOperatorOnly}},
		&rolesPlugin{name: "org", service: "cambrian.premium.org.OrgDirectoryAdmin",
			roles: map[string]OperatorMethodRole{"GetMyOrgChain": OperatorMethodSelfService}},
	}})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	got := composed.opts.OperatorMethodRoles
	want := map[string]OperatorMethodRole{
		"/cambrian.premium.accounts.AccountsAdmin/StartAccountConnect": OperatorMethodSelfService,
		"/cambrian.premium.accounts.AccountsAdmin/ListAccounts":        OperatorMethodOperatorOnly,
		"/cambrian.premium.org.OrgDirectoryAdmin/GetMyOrgChain":        OperatorMethodSelfService,
	}
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestDeclareOperatorMethodRoles_RefusesMalformedAndConflicting(t *testing.T) {
	for name, p := range map[string]*rolesPlugin{
		"OperatorConsole is core's": {name: "x", service: "cambrian.OperatorConsole",
			roles: map[string]OperatorMethodRole{"QueryLogs": OperatorMethodSelfService}},
		"agent plane": {name: "x", service: "cambrian.Orchestrator",
			roles: map[string]OperatorMethodRole{"Execute": OperatorMethodSelfService}},
		"unknown class": {name: "x", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"ListAccounts": "admin"}},
		"bad method": {name: "x", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"a/b": OperatorMethodSelfService}},
	} {
		if _, err := applyPlugins(Options{Plugins: []Plugin{p}}); err == nil {
			t.Errorf("%s: registration succeeded", name)
		}
	}
	// Two plugins giving one method different classes.
	_, err := applyPlugins(Options{Plugins: []Plugin{
		&rolesPlugin{name: "a", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"ListAccounts": OperatorMethodOperatorOnly}},
		&rolesPlugin{name: "b", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"ListAccounts": OperatorMethodMemberRead}},
	}})
	if err == nil || !strings.Contains(err.Error(), "declared both") {
		t.Fatalf("conflicting declarations must fail: %v", err)
	}
	// Options and a plugin disagreeing.
	_, err = applyPlugins(Options{
		OperatorMethodRoles: map[string]OperatorMethodRole{"/cambrian.premium.accounts.AccountsAdmin/ListAccounts": OperatorMethodMemberRead},
		Plugins: []Plugin{&rolesPlugin{name: "a", service: "cambrian.premium.accounts.AccountsAdmin",
			roles: map[string]OperatorMethodRole{"ListAccounts": OperatorMethodOperatorOnly}}},
	})
	if err == nil || !strings.Contains(err.Error(), "on Options but a plugin declared") {
		t.Fatalf("Options vs plugin disagreement must fail: %v", err)
	}
}
