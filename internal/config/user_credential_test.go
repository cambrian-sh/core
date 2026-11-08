package config

import (
	"strings"
	"testing"
)

// ADR-0140 D1: the credential model must fit the server's transport.
func TestValidateMCPServer_UserCredential(t *testing.T) {
	stdio := MCPServerConfig{ID: "m", Transport: "stdio", Endpoint: "npx"}
	http := MCPServerConfig{ID: "g", Transport: "http", Endpoint: "https://x"}
	cases := []struct {
		name string
		s    MCPServerConfig
		uc   *MCPUserCredentialConfig
		want string // "" ⇒ valid
	}{
		{"absent is shared", stdio, nil, ""},
		{"explicit shared ignores the rest", stdio, &MCPUserCredentialConfig{Scope: "shared", Inject: "bogus"}, ""},
		{"stdio env", stdio, &MCPUserCredentialConfig{Scope: "per_user", Provider: "microsoft", Inject: "env", Name: "MS365_MCP_OAUTH_TOKEN"}, ""},
		{"http bearer", http, &MCPUserCredentialConfig{Scope: "either", Provider: "github", Inject: "bearer", SharedFallback: true}, ""},
		{"unknown scope", stdio, &MCPUserCredentialConfig{Scope: "team"}, "not shared, per_user or either"},
		{"no provider", stdio, &MCPUserCredentialConfig{Scope: "per_user", Inject: "env", Name: "T"}, "provider is required"},
		{"env on http", http, &MCPUserCredentialConfig{Scope: "per_user", Provider: "p", Inject: "env", Name: "T"}, "needs a stdio server"},
		{"bearer on stdio", stdio, &MCPUserCredentialConfig{Scope: "per_user", Provider: "p", Inject: "bearer"}, "needs an http/sse server"},
		{"kernel var", stdio, &MCPUserCredentialConfig{Scope: "per_user", Provider: "p", Inject: "env", Name: "CAMBRIAN_SECRET_KEY"}, "kernel variable"},
		{"header needs name", http, &MCPUserCredentialConfig{Scope: "per_user", Provider: "p", Inject: "header"}, "header name"},
		{"fallback only for either", http, &MCPUserCredentialConfig{Scope: "per_user", Provider: "p", Inject: "bearer", SharedFallback: true}, "only meaningful"},
	}
	for _, tc := range cases {
		s := tc.s
		s.UserCredential = tc.uc
		errs := strings.Join(ValidateMCPServer(s), "; ")
		if tc.want == "" && errs != "" {
			t.Errorf("%s: unexpected %s", tc.name, errs)
		}
		if tc.want != "" && !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q, got %q", tc.name, tc.want, errs)
		}
	}
	if (MCPServerConfig{}).CredentialScope() != CredentialShared {
		t.Fatal("absent block must read as shared")
	}
}
