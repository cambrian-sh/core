package identity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261118-operator-rpcs-answer-unauthenticated-invalid-or: a token that could
// not be CHECKED because a store did not answer is ErrAuthUnavailable, never
// ErrUnauthenticated — a client drops its session on the latter. A token that is
// genuinely bad stays ErrUnauthenticated. Neither is ever accepted.

var errConnRefused = errors.New("dial tcp 127.0.0.1:5433: connect: connection refused")

// flakyRegistry is a MemRegistry whose reads fail with a transport error while
// down is set — what PgRegistry returns while Postgres is stopped.
type flakyRegistry struct {
	*MemRegistry
	down atomic.Bool
	// gone answers ErrPrincipalNotFound for every principal (the person was
	// removed after the session was minted).
	gone atomic.Bool
}

func (f *flakyRegistry) GetPrincipal(ctx context.Context, id string) (domain.Principal, error) {
	if f.down.Load() {
		return domain.Principal{}, errConnRefused
	}
	if f.gone.Load() {
		return domain.Principal{}, domain.ErrPrincipalNotFound
	}
	return f.MemRegistry.GetPrincipal(ctx, id)
}

func (f *flakyRegistry) ResolveIdentity(ctx context.Context, iss, sub string) (domain.Principal, error) {
	if f.down.Load() {
		return domain.Principal{}, errConnRefused
	}
	return f.MemRegistry.ResolveIdentity(ctx, iss, sub)
}

func (f *flakyRegistry) GetCredential(ctx context.Context, username string) (LocalCredential, error) {
	if f.down.Load() {
		return LocalCredential{}, errConnRefused
	}
	return f.MemRegistry.GetCredential(ctx, username)
}

func mustUnavailable(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrAuthUnavailable) {
		t.Fatalf("%s: err = %v, want ErrAuthUnavailable", what, err)
	}
	if errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("%s: err %v wraps ErrUnauthenticated too", what, err)
	}
}

func mustUnauthenticated(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrAuthUnavailable) {
		t.Fatalf("%s: err = %v, want ErrUnauthenticated only", what, err)
	}
}

func TestLocalAccounts_StoreDownIsUnavailableNotUnauthenticated(t *testing.T) {
	ctx := context.Background()
	reg := &flakyRegistry{MemRegistry: NewMemRegistry()}
	la := NewLocalAccounts(reg, reg)
	if _, err := la.EnsureAccount(ctx, "op", "pw", []string{"operator"}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := la.Login(ctx, "op", "pw")
	if err != nil {
		t.Fatal(err)
	}

	reg.down.Store(true)
	id, err := la.Resolve(ctx, tok)
	mustUnavailable(t, "resolve while the registry is down", err)
	if !id.Principal.IsZero() {
		t.Fatal("an unchecked token produced an identity")
	}
	_, err = NewChain(la, nil).Resolve(ctx, tok)
	mustUnavailable(t, "chain resolve while the registry is down", err)
	_, _, err = la.Login(ctx, "op", "pw")
	mustUnavailable(t, "login while the credential store is down", err)
	// A token the kernel never issued is still bad while the store is down: the
	// session table answers that without the store.
	_, err = la.Resolve(ctx, "never-issued")
	mustUnauthenticated(t, "unknown token while down", err)

	reg.down.Store(false)
	if got, err := la.Resolve(ctx, tok); err != nil || got.Principal != domain.UserPrincipal("op") {
		t.Fatalf("same token after recovery: %+v %v", got, err)
	}
	// Genuinely bad credentials stay Unauthenticated.
	_, err = la.Resolve(ctx, "never-issued")
	mustUnauthenticated(t, "unknown token", err)
	_, _, err = la.Login(ctx, "op", "wrong")
	mustUnauthenticated(t, "wrong password", err)
	_, _, err = la.Login(ctx, "nobody", "pw")
	mustUnauthenticated(t, "unknown user", err)
	// A session whose person was deleted is a bad token, not an outage.
	reg.gone.Store(true)
	_, err = la.Resolve(ctx, tok)
	mustUnauthenticated(t, "person gone", err)
}

func TestOIDC_RegistryDownIsUnavailable(t *testing.T) {
	f := newFixture(t)
	reg := &flakyRegistry{MemRegistry: f.reg}
	v, err := NewOIDCVerifier(OIDCConfig{Issuer: f.idp.srv.URL, ClientID: "console",
		FirstLogin: FirstLoginAutoCreate, DefaultRoles: []string{"viewer"}, Now: f.now}, reg)
	if err != nil {
		t.Fatal(err)
	}
	tok := signJWT(t, "RS256", "a", f.kA, f.claims("s1"))
	mustResolve(t, v, tok)
	reg.down.Store(true)
	_, err = v.Resolve(context.Background(), tok)
	mustUnavailable(t, "oidc resolve while the registry is down", err)
	reg.down.Store(false)
	mustResolve(t, v, tok)
}
