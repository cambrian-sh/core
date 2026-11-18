package identity

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// One contract, two implementations (ADR-0142 D3): every case runs against the
// in-memory registry always and against Postgres when PG_TEST_DSN names a
// migrated, disposable database.

type regUnderTest interface {
	domain.PrincipalRegistry
	CredentialStore
}

func registries(t *testing.T) map[string]regUnderTest {
	t.Helper()
	out := map[string]regUnderTest{"mem": NewMemRegistry()}
	if os.Getenv("PG_TEST_DSN") == "" {
		return out
	}
	// The test migrates its database itself (ENG-2100) instead of skipping on a
	// database nobody had run `migrate up` on.
	dsn, _ := pgscratch.Migrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	pg, err := NewPgRegistry(ctx, pool)
	if err != nil {
		t.Fatalf("principal registry on the migrated test database: %v", err)
	}
	clean := func() {
		// Cascades to identities, aliases and credentials. Test rows only.
		if _, err := pool.Exec(context.Background(), `DELETE FROM principals WHERE id LIKE 'prtest-%'`); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	out["pg"] = pg
	return out
}

func TestRegistry_Contract(t *testing.T) {
	for name, reg := range registries(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p, created, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-alice", Roles: []string{"viewer", "viewer", " "}})
			if err != nil || !created {
				t.Fatalf("first ensure: created=%v err=%v", created, err)
			}
			if p.Kind != domain.PrincipalUser || p.Status != domain.PrincipalActive || len(p.Roles) != 1 {
				t.Fatalf("defaults not applied: %+v", p)
			}
			// Idempotent; fill-only.
			p2, created, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-alice", DisplayName: "Alice", Roles: []string{"operator"}})
			if err != nil || created || p2.DisplayName != "Alice" || p2.Roles[0] != "viewer" {
				t.Fatalf("second ensure must fill the name and keep roles: %+v created=%v err=%v", p2, created, err)
			}
			if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-alice", Kind: domain.PrincipalActor}); !errors.Is(err, domain.ErrPrincipalKindClash) {
				t.Fatalf("kind clash: %v", err)
			}
			if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-bot", Kind: domain.PrincipalAgent}); !errors.Is(err, domain.ErrInvalidPrincipal) {
				t.Fatalf("an agent is not a person: %v", err)
			}
			if _, err := reg.GetPrincipal(ctx, "prtest-nobody"); !errors.Is(err, domain.ErrPrincipalNotFound) {
				t.Fatalf("unknown: %v", err)
			}

			// Identities: (issuer, subject) is the key; a second person cannot take it.
			if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-bob"}); err != nil {
				t.Fatal(err)
			}
			id := domain.ExternalIdentity{Issuer: "https://idp/realms/x", Subject: "s-1", PrincipalID: "prtest-alice"}
			if err := reg.LinkIdentity(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := reg.LinkIdentity(ctx, id); err != nil {
				t.Fatalf("re-link is a no-op: %v", err)
			}
			id.PrincipalID = "prtest-bob"
			if err := reg.LinkIdentity(ctx, id); !errors.Is(err, domain.ErrIdentityConflict) {
				t.Fatalf("hijack: %v", err)
			}
			if got, err := reg.ResolveIdentity(ctx, "https://idp/realms/x", "s-1"); err != nil || got.ID != "prtest-alice" {
				t.Fatalf("resolve: %+v %v", got, err)
			}
			if _, err := reg.ResolveIdentity(ctx, "https://idp/realms/x", "s-2"); !errors.Is(err, domain.ErrIdentityNotFound) {
				t.Fatalf("unknown subject: %v", err)
			}
			if err := reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: "i", Subject: "s", PrincipalID: "prtest-ghost"}); !errors.Is(err, domain.ErrPrincipalNotFound) {
				t.Fatalf("link to nobody: %v", err)
			}

			// Aliases: the audit back-compat window.
			if err := reg.AddAlias(ctx, domain.LegacyAgentAlias("prtest-alice"), "prtest-alice"); err != nil {
				t.Fatal(err)
			}
			for _, ref := range []string{"agent:prtest-alice", "user:prtest-alice"} {
				if got, ok, err := reg.Canonical(ctx, ref); err != nil || !ok || got != "prtest-alice" {
					t.Fatalf("Canonical(%s) = %q %v %v", ref, got, ok, err)
				}
			}
			if _, ok, _ := reg.Canonical(ctx, "agent:prtest-nobody"); ok {
				t.Fatal("an unknown alias named somebody")
			}
			if al, _ := reg.AliasesOf(ctx, "prtest-alice"); len(al) != 1 {
				t.Fatalf("aliases: %v", al)
			}

			// Lifecycle drives KnownPrincipal (act-for) and PersonMayAct (doors).
			if ok, _ := reg.KnownPrincipal(ctx, "prtest-alice"); !ok {
				t.Fatal("active person unknown")
			}
			if err := reg.SetStatus(ctx, "prtest-alice", domain.PrincipalSuspended); err != nil {
				t.Fatal(err)
			}
			if ok, _ := reg.KnownPrincipal(ctx, "prtest-alice"); ok {
				t.Fatal("a suspended person must not be act-for-able")
			}
			if domain.PersonMayAct(ctx, reg, "prtest-alice") {
				t.Fatal("a suspended person may not act")
			}
			if !domain.PersonMayAct(ctx, reg, "prtest-legacy") {
				t.Fatal("a not-yet-imported legacy person must keep working")
			}
			if err := reg.SetStatus(ctx, "prtest-alice", "frozen"); !errors.Is(err, domain.ErrInvalidPrincipal) {
				t.Fatalf("bad status: %v", err)
			}
			if err := reg.SetRoles(ctx, "prtest-bob", []string{"operator"}); err != nil {
				t.Fatal(err)
			}
			if ps, _ := reg.ListPrincipals(ctx, domain.PrincipalFilter{Status: domain.PrincipalSuspended}); len(ps) != 1 || ps[0].ID != "prtest-alice" {
				t.Fatalf("list by status: %+v", ps)
			}
		})
	}
}

func TestPersonIDFor_Deterministic(t *testing.T) {
	a := domain.PersonIDFor("oidc", "https://idp", "sub-1")
	if a != domain.PersonIDFor("oidc", "https://idp", "sub-1") || a == domain.PersonIDFor("oidc", "https://idp", "sub-2") {
		t.Fatal("PersonIDFor must be a pure function of its key")
	}
	if len(a) != len("oidc-")+16 {
		t.Fatalf("id %q", a)
	}
	// The separator matters: ("ab","c") and ("a","bc") are different people.
	if domain.PersonIDFor("x", "ab", "c") == domain.PersonIDFor("x", "a", "bc") {
		t.Fatal("ambiguous key join")
	}
}

func TestLocalAccounts(t *testing.T) {
	for name, reg := range registries(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			la := NewLocalAccounts(reg, reg)
			p, err := la.EnsureAccount(ctx, "prtest-op", "s3cret", []string{"operator"})
			if err != nil || p.Source != domain.SourceLocal {
				t.Fatalf("ensure: %+v %v", p, err)
			}
			c, err := reg.GetCredential(ctx, "prtest-op")
			if err != nil || c.PasswordHash == "s3cret" || !VerifyPassword(c.PasswordHash, "s3cret") {
				t.Fatalf("stored credential must be an argon2id hash: %+v %v", c, err)
			}
			// Re-asserting the same password does not rehash (stable row); a new env
			// password replaces it.
			if _, err := la.EnsureAccount(ctx, "prtest-op", "s3cret", []string{"operator"}); err != nil {
				t.Fatal(err)
			}
			if c2, _ := reg.GetCredential(ctx, "prtest-op"); c2.PasswordHash != c.PasswordHash {
				t.Fatal("unchanged password was rehashed")
			}

			tok, id, err := la.Login(ctx, "prtest-op", "s3cret")
			if err != nil || id.Principal != domain.UserPrincipal("prtest-op") || !id.HasRole("operator") || id.Issuer != LocalIssuer {
				t.Fatalf("login: %+v %v", id, err)
			}
			if got, err := la.Resolve(ctx, tok); err != nil || got.Principal != id.Principal {
				t.Fatalf("resolve: %+v %v", got, err)
			}
			for _, bad := range [][2]string{{"prtest-op", "wrong"}, {"prtest-nobody", "s3cret"}, {"prtest-op", ""}} {
				if _, _, err := la.Login(ctx, bad[0], bad[1]); !errors.Is(err, domain.ErrUnauthenticated) {
					t.Fatalf("login %v: %v", bad, err)
				}
			}
			// Suspension takes effect on the next call, not at token expiry.
			_ = reg.SetStatus(ctx, "prtest-op", domain.PrincipalSuspended)
			if _, err := la.Resolve(ctx, tok); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("suspended resolve: %v", err)
			}
			if _, _, err := la.Login(ctx, "prtest-op", "s3cret"); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("suspended login: %v", err)
			}
			_ = reg.SetStatus(ctx, "prtest-op", domain.PrincipalActive)
			// Revoke, then TTL.
			_ = la.Revoke(ctx, tok)
			if _, err := la.Resolve(ctx, tok); err == nil {
				t.Fatal("revoked token resolved")
			}
			now := time.Now()
			la.now = func() time.Time { return now }
			tok2, _, _ := la.Login(ctx, "prtest-op", "s3cret")
			la.now = func() time.Time { return now.Add(SessionTTL - time.Minute) }
			if _, err := la.Resolve(ctx, tok2); err != nil {
				t.Fatalf("within TTL: %v", err)
			}
			la.now = func() time.Time { return now.Add(SessionTTL) }
			if _, err := la.Resolve(ctx, tok2); err == nil {
				t.Fatal("expired token resolved")
			}
		})
	}
}

func TestVerifyPassword_MalformedHashesVerifyNothing(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2id$v=19$m=0,t=2,p=1$AAAA$AAAA", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=19456,t=2,p=1$!!$AAAA"} {
		if VerifyPassword(h, "") || VerifyPassword(h, "plain") {
			t.Fatalf("hash %q verified", h)
		}
	}
}

// The chain routes by token shape and only revokes kernel sessions.
func TestChain_RoutesByShape(t *testing.T) {
	ctx := context.Background()
	reg := NewMemRegistry()
	la := NewLocalAccounts(reg, reg)
	if _, err := la.EnsureAccount(ctx, "op", "pw", []string{"operator"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAuth{id: domain.Identity{Principal: domain.UserPrincipal("oidc-x")}}
	c := NewChain(la, fake)
	tok, _, err := c.Login(ctx, "op", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := c.Resolve(ctx, tok); err != nil || id.Principal.ID != "op" || fake.calls != 0 {
		t.Fatalf("local token: %+v %v calls=%d", id, err, fake.calls)
	}
	if id, err := c.Resolve(ctx, jwtShaped); err != nil || id.Principal.ID != "oidc-x" || fake.calls != 1 {
		t.Fatalf("jwt: %+v %v", id, err)
	}
	if _, err := c.Resolve(ctx, ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("empty token")
	}
	if m := c.AuthMetadata(); !m.PasswordLogin {
		t.Fatalf("metadata %+v", m)
	}
	// No OIDC configured: a JWT-shaped string is just an unknown local token.
	if _, err := NewChain(la, nil).Resolve(ctx, jwtShaped); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("jwt without oidc")
	}
}

// jwtShaped is base64url({"alg":"RS256"}).e30.sig — routed to OIDC by shape.
const jwtShaped = "eyJhbGciOiJSUzI1NiJ9.e30.c2ln"

type fakeAuth struct {
	id    domain.Identity
	calls int
}

func (f *fakeAuth) Resolve(context.Context, string) (domain.Identity, error) {
	f.calls++
	return f.id, nil
}
func (f *fakeAuth) Revoke(context.Context, string) error { return nil }

// TestRegistry_UpdateProfile: unlike EnsurePrincipal's fill-only rule,
// UpdateProfile REPLACES a set value (the directory that owns a person's profile
// renames them, ENG-683); an empty argument keeps the stored value, and an
// unknown id is ErrPrincipalNotFound.
func TestRegistry_UpdateProfile(t *testing.T) {
	for name, reg := range registries(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-carol", DisplayName: "Carol", Email: "carol@old.example"}); err != nil {
				t.Fatal(err)
			}
			// EnsurePrincipal never overwrites: the pre-fix path that kept the first name forever.
			if p, _, _ := reg.EnsurePrincipal(ctx, domain.Principal{ID: "prtest-carol", DisplayName: "Carol Smith"}); p.DisplayName != "Carol" {
				t.Fatalf("EnsurePrincipal must stay fill-only, got %q", p.DisplayName)
			}
			if err := reg.UpdateProfile(ctx, "prtest-carol", "Carol Smith", ""); err != nil {
				t.Fatal(err)
			}
			p, err := reg.GetPrincipal(ctx, "prtest-carol")
			if err != nil || p.DisplayName != "Carol Smith" || p.Email != "carol@old.example" {
				t.Fatalf("rename must replace the name and keep the email: %+v %v", p, err)
			}
			if err := reg.UpdateProfile(ctx, "prtest-carol", "", "carol@new.example"); err != nil {
				t.Fatal(err)
			}
			if p, _ = reg.GetPrincipal(ctx, "prtest-carol"); p.DisplayName != "Carol Smith" || p.Email != "carol@new.example" {
				t.Fatalf("email change: %+v", p)
			}
			if err := reg.UpdateProfile(ctx, "prtest-ghost", "X", ""); !errors.Is(err, domain.ErrPrincipalNotFound) {
				t.Fatalf("unknown id: %v", err)
			}
		})
	}
}
