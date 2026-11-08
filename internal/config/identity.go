package config

import (
	"fmt"
	"net/url"
	"strings"
)

// IdentityConfig configures how people sign in (ADR-0142 D1). It is top-level,
// beside `server`, because it governs every door a person comes through, not one
// listener.
//
// Nothing configured is today's behaviour: local accounts only, seeded with the
// CAMBRIAN_OPERATOR_USER / CAMBRIAN_OPERATOR_PASSWORD bootstrap operator.
type IdentityConfig struct {
	// OIDC is the ONE external issuer core verifies (owner acceptance 2026-11-06
	// item 2: single-provider OIDC is in core; multi-issuer federation and SAML
	// brokering are premium). Off while issuer is empty.
	OIDC OIDCIdentityConfig `json:"oidc"`
}

// OIDCIdentityConfig is one issuer and its static claim → role map.
type OIDCIdentityConfig struct {
	// Issuer is the exact issuer URL (e.g. a Keycloak realm,
	// https://idp.example/realms/cambrian). https is required unless the host is
	// loopback. Empty ⇒ OIDC off.
	Issuer string `json:"issuer"`
	// ClientID is the public client the console / desktop runs PKCE with; it is
	// advertised by DescribeAuth and is the default accepted audience.
	ClientID string `json:"client_id"`
	// Audiences accepted in `aud`. Empty ⇒ [client_id].
	Audiences []string `json:"audiences"`
	// RolesClaim is the claim holding the IdP's roles/groups. Default "roles".
	RolesClaim string `json:"roles_claim"`
	// RoleMap maps a claim value to Cambrian roles (`operator`, `viewer`, …).
	// Unmapped values grant nothing (Zero-Hardcode: no group name is special in code).
	RoleMap map[string][]string `json:"role_map"`
	// DefaultRoles are stored on a person created at first login. Empty ⇒ the
	// person is a `member`: their own conversations and accounts, no console.
	DefaultRoles []string `json:"default_roles"`
	// FirstLogin decides an unknown (issuer, subject): `require_provisioned`
	// (default, fail closed — an admin links the identity first) or `auto_create`.
	FirstLogin string `json:"first_login"`
	// ClockSkewSeconds tolerated on exp/nbf/iat. Default 60.
	ClockSkewSeconds int `json:"clock_skew_seconds"`
}

// Enabled reports whether an issuer is configured.
func (o OIDCIdentityConfig) Enabled() bool { return strings.TrimSpace(o.Issuer) != "" }

// validate reports configuration errors. insecureLocalhost is
// server.insecure_localhost: ADR-0142 (feasibility K-2) refuses it together with
// an identity provider, because it makes the bare x-agent-id header the agent
// principal — an IdP deployment is by definition one with many people on it.
func (c IdentityConfig) validate(insecureLocalhost bool) []string {
	o := c.OIDC
	if !o.Enabled() {
		return nil
	}
	var errs []string
	u, err := url.Parse(strings.TrimSpace(o.Issuer))
	if err != nil || u.Host == "" {
		errs = append(errs, fmt.Sprintf("identity.oidc.issuer %q is not an absolute URL", o.Issuer))
	} else if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		errs = append(errs, fmt.Sprintf("identity.oidc.issuer %q must be https (http is allowed only for a loopback issuer)", o.Issuer))
	}
	if strings.TrimSpace(o.ClientID) == "" && len(o.Audiences) == 0 {
		errs = append(errs, "identity.oidc.client_id (or identity.oidc.audiences) is required when identity.oidc.issuer is set")
	}
	switch o.FirstLogin {
	case "", "require_provisioned", "auto_create":
	default:
		errs = append(errs, fmt.Sprintf("identity.oidc.first_login %q must be require_provisioned or auto_create", o.FirstLogin))
	}
	if o.ClockSkewSeconds < 0 || o.ClockSkewSeconds > 600 {
		errs = append(errs, "identity.oidc.clock_skew_seconds must be 0..600")
	}
	if insecureLocalhost {
		errs = append(errs, "server.insecure_localhost cannot be combined with identity.oidc: it trusts the bare "+
			"x-agent-id header as the agent principal, which an identity-provider deployment must not do "+
			"(ADR-0142 K-2). Unset one of them; benchmarks keep insecure_localhost on their own kernel")
	}
	return errs
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
