package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// The Authenticator port (ADR-0142 D1).
//
// It replaces the password-shaped operator.OperatorIdentity (ADR-0047 D13). The
// split exists because OIDC login happens AT THE IDENTITY PROVIDER: the console
// runs authorization code + PKCE in the system browser, a desktop client may use
// the device grant, and the kernel receives only tokens. So:
//
//   - Authenticator.Resolve(token) → who and which roles. It is the ONLY method
//     an interceptor calls, on the gRPC operator plane and the HTTP admin routes.
//   - PasswordLogin is an OPTIONAL capability: a backend that verifies a
//     username/password itself (local accounts) implements it, and the Login RPC
//     uses it when present. An OIDC verifier does not — shoving an authorization
//     code into Login(username, password) would lie about the contract.
//
// Cambrian VERIFIES identity; it never issues identity to people and runs no
// authorization server (D1). The only tokens the kernel mints are the opaque
// session tokens a local-account login returns.
// ─────────────────────────────────────────────────────────────────────────────

// Identity is an authenticated caller.
type Identity struct {
	// Principal is the registry person (kind `user`) the credential resolved to.
	Principal PrincipalRef
	// Roles is the person's plane-role set (`operator`, `viewer`, …).
	Roles []string
	// Issuer names the backend that verified the credential: "local" for a local
	// account session, the issuer URL for an OIDC token.
	Issuer string
	// ExpiresAt is the credential's horizon (zero = none known).
	ExpiresAt time.Time
	// Claims are the VERIFIED claim set of an OIDC token (nil for a local-account
	// session). A premium Authenticator wrapper reads the IdP's group and
	// department claims from it to apply mapping rules at login (ADR-0142 D5,
	// JIT). Never a source of identity: Principal is.
	Claims map[string]json.RawMessage
}

// HasRole reports whether the identity holds role r.
func (i Identity) HasRole(r string) bool {
	for _, x := range i.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// ErrUnauthenticated is the one answer for every credential that does not verify
// (unknown, expired, wrong signature, wrong audience, suspended person). It is
// deliberately undetailed toward the caller; backends log the reason.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator resolves a bearer credential to an Identity.
type Authenticator interface {
	// Resolve verifies token. Every failure is ErrUnauthenticated (wrapped).
	Resolve(ctx context.Context, token string) (Identity, error)
	// Revoke ends a kernel-issued session token (Logout). Revoking an unknown
	// token, or a token the kernel did not issue (an IdP's JWT, which only its
	// issuer can revoke), is not an error.
	Revoke(ctx context.Context, token string) error
}

// PasswordLogin is the optional capability of a backend that verifies passwords
// itself (ADR-0142 D1.4 local accounts).
type PasswordLogin interface {
	Login(ctx context.Context, username, password string) (token string, id Identity, err error)
}

// AuthMetadata is what a client needs to know to sign in: whether the kernel
// takes a password, and which OIDC issuer and client id to run PKCE / the device
// grant against. Advertised on the operator handshake (contract: additive).
type AuthMetadata struct {
	PasswordLogin bool
	OIDCIssuer    string
	OIDCClientID  string
}

// AuthMetadataSource is implemented by an Authenticator that can describe itself.
type AuthMetadataSource interface {
	AuthMetadata() AuthMetadata
}
