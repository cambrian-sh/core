package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
)

// ADR-0142: the principal CLI pre-provisions a person for require_provisioned
// OIDC (link), creates a local account (passwd), and drives lifecycle.
func TestPrincipalCommand(t *testing.T) {
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	run := func(args ...string) error { return principalCommand(ctx, reg, reg, args, nil) }
	if err := run("add", "maria", "--name", "Maria", "--roles", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := run("link", "maria", "--issuer", "https://idp/realms/x", "--subject", "s-9"); err != nil {
		t.Fatal(err)
	}
	if p, err := reg.ResolveIdentity(ctx, "https://idp/realms/x", "s-9"); err != nil || p.ID != "maria" {
		t.Fatalf("link: %+v %v", p, err)
	}
	t.Setenv("CAMBRIAN_PRINCIPAL_PASSWORD", "pw-1")
	if err := run("passwd", "maria"); err != nil {
		t.Fatal(err)
	}
	if _, id, err := identity.NewLocalAccounts(reg, reg).Login(ctx, "maria", "pw-1"); err != nil || id.Principal != domain.UserPrincipal("maria") {
		t.Fatalf("login after passwd: %+v %v", id, err)
	}
	if err := run("roles", "maria", "operator,viewer"); err != nil {
		t.Fatal(err)
	}
	if err := run("suspend", "maria"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := reg.KnownPrincipal(ctx, "maria"); ok {
		t.Fatal("suspend did not take")
	}
	if err := run("passwd", "nobody"); err == nil {
		t.Fatal("passwd for an unknown person must fail")
	}
	if err := run("bogus"); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
