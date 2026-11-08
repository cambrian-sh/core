package config

import (
	"strings"
	"testing"
)

// ADR-0142 D1 + K-2: the identity block validates, and refuses insecure_localhost
// together with an identity provider.
func TestIdentityConfig_Validate(t *testing.T) {
	ok := OIDCIdentityConfig{Issuer: "https://idp.example/realms/cambrian", ClientID: "cambrian-console"}
	cases := []struct {
		name     string
		oidc     OIDCIdentityConfig
		insecure bool
		want     string // substring of an error; "" = valid
	}{
		{"off", OIDCIdentityConfig{}, true, ""},
		{"keycloak realm", ok, false, ""},
		{"loopback http", OIDCIdentityConfig{Issuer: "http://127.0.0.1:8080/realms/x", ClientID: "c"}, false, ""},
		{"routable http", OIDCIdentityConfig{Issuer: "http://idp.example", ClientID: "c"}, false, "must be https"},
		{"no client", OIDCIdentityConfig{Issuer: "https://idp.example"}, false, "client_id"},
		{"bad first login", OIDCIdentityConfig{Issuer: "https://idp.example", ClientID: "c", FirstLogin: "yes"}, false, "first_login"},
		{"insecure_localhost refused", ok, true, "insecure_localhost cannot be combined"},
	}
	for _, c := range cases {
		errs := IdentityConfig{OIDC: c.oidc}.validate(c.insecure)
		joined := strings.Join(errs, "; ")
		if c.want == "" && len(errs) > 0 {
			t.Errorf("%s: unexpected errors %v", c.name, errs)
		}
		if c.want != "" && !strings.Contains(joined, c.want) {
			t.Errorf("%s: want %q in %q", c.name, c.want, joined)
		}
	}
}
