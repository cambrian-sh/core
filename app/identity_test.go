package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
)

// ADR-0142 back-compat: an existing deployment's env bootstrap operator keeps
// signing in, as the same principal, with the same role — now as a local account.
func TestBuildAuthenticator_EnvBootstrapBecomesLocalAccount(t *testing.T) {
	t.Setenv("CAMBRIAN_OPERATOR_USER", "operator")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "pw")
	t.Setenv("CAMBRIAN_OPERATOR_ROLE", "")
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	st := identityStores{registry: reg, creds: reg}
	auth, err := buildAuthenticator(ctx, &config.Config{}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, ok := auth.(domain.PasswordLogin)
	if !ok {
		t.Fatal("the default authenticator must serve password login")
	}
	tok, id, err := pl.Login(ctx, "operator", "pw")
	if err != nil || id.Principal != domain.UserPrincipal("operator") || !id.HasRole("operator") {
		t.Fatalf("bootstrap login: %+v %v", id, err)
	}
	if got, err := auth.Resolve(ctx, tok); err != nil || got.Principal.ID != "operator" {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	p, err := reg.GetPrincipal(ctx, "operator")
	if err != nil || p.Source != domain.SourceLocal {
		t.Fatalf("registry row: %+v %v", p, err)
	}
	// A rotated env password takes over on the next boot; the old one stops working.
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "pw-2")
	auth2, err := buildAuthenticator(ctx, &config.Config{}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth2.(domain.PasswordLogin).Login(ctx, "operator", "pw"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("the old env password still works after rotation")
	}
	if _, _, err := auth2.(domain.PasswordLogin).Login(ctx, "operator", "pw-2"); err != nil {
		t.Fatal(err)
	}
}

// With no env creds no login can succeed (secure by default, unchanged).
func TestBuildAuthenticator_NoEnvNoLogin(t *testing.T) {
	t.Setenv("CAMBRIAN_OPERATOR_USER", "")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "")
	reg := identity.NewMemRegistry()
	auth, err := buildAuthenticator(context.Background(), &config.Config{}, identityStores{reg, reg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.(domain.PasswordLogin).Login(context.Background(), "operator", ""); err == nil {
		t.Fatal("login succeeded with no account")
	}
}

// identity.oidc reaches the verifier (advertised by DescribeAuth), and a plugin's
// NewAuthenticator receives the registry and the OSS default and wins.
func TestBuildAuthenticator_OIDCAndPluginSeam(t *testing.T) {
	t.Setenv("CAMBRIAN_OPERATOR_USER", "")
	reg := identity.NewMemRegistry()
	cfg := &config.Config{}
	cfg.Identity.OIDC = config.OIDCIdentityConfig{Issuer: "https://idp.example/realms/cambrian", ClientID: "console"}
	auth, err := buildAuthenticator(context.Background(), cfg, identityStores{reg, reg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := auth.(domain.AuthMetadataSource).AuthMetadata()
	if m.OIDCIssuer != "https://idp.example/realms/cambrian" || m.OIDCClientID != "console" || !m.PasswordLogin {
		t.Fatalf("metadata %+v", m)
	}
	var gotReg domain.PrincipalRegistry
	var gotOSS domain.Authenticator
	replaced := &identity.Chain{}
	auth2, err := buildAuthenticator(context.Background(), cfg, identityStores{reg, reg},
		func(r domain.PrincipalRegistry, oss domain.Authenticator) (domain.Authenticator, error) {
			gotReg, gotOSS = r, oss
			return replaced, nil
		})
	if err != nil || auth2 != replaced || gotReg != reg || gotOSS == nil {
		t.Fatalf("plugin seam: %v %v", auth2, err)
	}
	if _, err := buildAuthenticator(context.Background(), cfg, identityStores{reg, reg},
		func(domain.PrincipalRegistry, domain.Authenticator) (domain.Authenticator, error) {
			return nil, errors.New("boom")
		}); err == nil {
		t.Fatal("a failing plugin authenticator must fail the boot, not fall back")
	}
}

// Worker-machine owners become registry persons with their legacy alias; an
// owner naming an unbound MCP client is an agent and is left alone.
func TestImportMachineOwners(t *testing.T) {
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	resolve := func(name string) (string, bool) {
		switch name {
		case workerOwnerName("laptop"):
			return "alice", true
		case workerOwnerName("bot"):
			return "mcp:claude", true
		}
		return "", false
	}
	importMachineOwners(ctx, reg, []string{"laptop", "bot", "none"}, resolve)
	if _, err := reg.GetPrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetPrincipal(ctx, "mcp:claude"); !errors.Is(err, domain.ErrPrincipalNotFound) {
		t.Fatal("an MCP client owner is not a person")
	}
	if id, ok, _ := reg.Canonical(ctx, "agent:alice"); !ok || id != "alice" {
		t.Fatal("legacy alias missing")
	}
}

func workerOwnerName(c string) string { return mcpserve.WorkerOwnerSecretName(c) }
