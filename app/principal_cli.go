package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// RunPrincipal is the `principal` subcommand (ADR-0142 D3): offline administration
// of the principal registry against the kernel's database. It is how an operator
// pre-provisions a person for `identity.oidc.first_login = require_provisioned`
// (link an issuer subject), creates local accounts beyond the env bootstrap
// operator, and suspends or reactivates a person. It writes through the same
// registry the kernel reads, so a change takes effect on the person's next call.
//
// Lives in `app` so both the OSS and the premium binary dispatch to it.
func RunPrincipal(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, principalUsage)
		return 2
	}
	base := config.ResolveBaseDir()
	cfg, err := config.LoadConfig(filepath.Join(base, "configs", "config.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	pool, err := postgres.OpenPlainPool(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer pool.Close()
	reg, err := identity.NewPgRegistry(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := principalCommand(ctx, reg, reg, args, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "principal %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

const principalUsage = `usage:
  cambrian principal list
  cambrian principal add <id> [--name <display>] [--email <e>] [--roles operator,viewer]
  cambrian principal link <id> --issuer <iss> --subject <sub> [--email <e>]
  cambrian principal passwd <id> [--username <u>]   (password from CAMBRIAN_PRINCIPAL_PASSWORD or one stdin line)
  cambrian principal roles <id> <role,role,...>
  cambrian principal suspend|activate|deprovision <id>
`

// principalCommand is the testable core of RunPrincipal.
func principalCommand(ctx context.Context, reg domain.PrincipalRegistry, creds identity.CredentialStore,
	args []string, stdin *os.File) error {
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("principal "+sub, flag.ContinueOnError)
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email (informational; never an identity key)")
	roles := fs.String("roles", "", "comma-separated roles")
	issuer := fs.String("issuer", "", "OIDC issuer URL")
	subject := fs.String("subject", "", "OIDC subject")
	username := fs.String("username", "", "local-account username (default: the id)")
	id := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	var positional []string
	if sub == "roles" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		positional, rest = rest[:1], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	needID := func() error {
		if id == "" {
			return errors.New("an <id> is required")
		}
		return nil
	}
	switch sub {
	case "list":
		ps, err := reg.ListPrincipals(ctx, domain.PrincipalFilter{})
		if err != nil {
			return err
		}
		for _, p := range ps {
			fmt.Printf("%-40s %-6s %-13s %-7s roles=%s  %s\n", p.Ref().String(), p.Kind, p.Status, p.Source,
				strings.Join(p.Roles, ","), p.DisplayName)
		}
		return nil
	case "add":
		if err := needID(); err != nil {
			return err
		}
		p, created, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: id, DisplayName: *name, Email: *email,
			Source: domain.SourceEditor, Roles: splitRoles(*roles)})
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", p.Ref(), map[bool]string{true: "created", false: "already exists (unchanged)"}[created])
		return nil
	case "link":
		if err := needID(); err != nil {
			return err
		}
		if *issuer == "" || *subject == "" {
			return errors.New("--issuer and --subject are required")
		}
		return reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: *issuer, Subject: *subject, Email: *email, PrincipalID: id})
	case "passwd":
		if err := needID(); err != nil {
			return err
		}
		if _, err := reg.GetPrincipal(ctx, id); err != nil {
			return err
		}
		pw := os.Getenv("CAMBRIAN_PRINCIPAL_PASSWORD")
		if pw == "" && stdin != nil {
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			pw = strings.TrimRight(line, "\r\n")
		}
		if pw == "" {
			return errors.New("no password given (CAMBRIAN_PRINCIPAL_PASSWORD or stdin)")
		}
		u := *username
		if u == "" {
			u = id
		}
		return identity.NewLocalAccounts(reg, creds).SetPassword(ctx, id, u, pw)
	case "roles":
		if err := needID(); err != nil {
			return err
		}
		if len(positional) == 0 {
			return errors.New("roles needs a comma-separated role list")
		}
		return reg.SetRoles(ctx, id, splitRoles(positional[0]))
	case "suspend", "activate", "deprovision":
		if err := needID(); err != nil {
			return err
		}
		st := map[string]domain.PrincipalStatus{"suspend": domain.PrincipalSuspended,
			"activate": domain.PrincipalActive, "deprovision": domain.PrincipalDeprovisioned}[sub]
		return reg.SetStatus(ctx, id, st)
	}
	return fmt.Errorf("unknown subcommand %q\n%s", sub, principalUsage)
}

func splitRoles(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
