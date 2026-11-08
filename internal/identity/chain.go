package identity

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// Chain is the OSS default domain.Authenticator (ADR-0142 D1): local accounts,
// plus at most ONE OIDC issuer when one is configured (owner acceptance item 2:
// single-provider OIDC is in core; multi-issuer federation and SAML brokering are
// premium and replace this whole value through app.Options.NewAuthenticator).
//
// Routing is by token SHAPE, not by trying each backend in turn: a JWT goes to
// the OIDC verifier, anything else to the local session table. Trying both would
// hand an attacker's JWT to the local table (harmless) and a local token to the
// JWT parser (harmless) — but it would also make every failure report the LAST
// backend's reason, which is the kind of ambiguity that hides misconfiguration.
type Chain struct {
	Local *LocalAccounts
	// OIDC is nil when no issuer is configured.
	OIDC domain.Authenticator
	// meta is the OIDC verifier's advertised issuer/client id.
	oidcMeta domain.AuthMetadata
}

// NewChain composes the backends. oidc may be nil.
func NewChain(local *LocalAccounts, oidc domain.Authenticator) *Chain {
	c := &Chain{Local: local, OIDC: oidc}
	if src, ok := oidc.(domain.AuthMetadataSource); ok && oidc != nil {
		c.oidcMeta = src.AuthMetadata()
	}
	return c
}

// Resolve implements domain.Authenticator.
func (c *Chain) Resolve(ctx context.Context, token string) (domain.Identity, error) {
	if token == "" {
		return domain.Identity{}, fmt.Errorf("%w: no bearer token", domain.ErrUnauthenticated)
	}
	if c.OIDC != nil && LooksLikeJWT(token) {
		return c.OIDC.Resolve(ctx, token)
	}
	if c.Local == nil {
		return domain.Identity{}, fmt.Errorf("%w: no local accounts", domain.ErrUnauthenticated)
	}
	return c.Local.Resolve(ctx, token)
}

// Revoke implements domain.Authenticator: only a kernel-issued local session can
// be revoked here; an IdP token is its issuer's to revoke.
func (c *Chain) Revoke(ctx context.Context, token string) error {
	if c.Local != nil {
		return c.Local.Revoke(ctx, token)
	}
	return nil
}

// Login implements domain.PasswordLogin through the local accounts.
func (c *Chain) Login(ctx context.Context, username, password string) (string, domain.Identity, error) {
	if c.Local == nil {
		return "", domain.Identity{}, fmt.Errorf("%w: password login is not enabled", domain.ErrUnauthenticated)
	}
	return c.Local.Login(ctx, username, password)
}

// AuthMetadata implements domain.AuthMetadataSource.
func (c *Chain) AuthMetadata() domain.AuthMetadata {
	m := c.oidcMeta
	m.PasswordLogin = c.Local != nil
	return m
}

var (
	_ domain.Authenticator      = (*Chain)(nil)
	_ domain.PasswordLogin      = (*Chain)(nil)
	_ domain.AuthMetadataSource = (*Chain)(nil)
)
