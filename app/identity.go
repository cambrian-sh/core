package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Identity composition (ADR-0142 D1/D3, stage 1).
//
// This file is the ONE place the principal registry and the Authenticator are
// built. The registry is the store of record for persons; the Authenticator is
// what every operator-plane interceptor and the HTTP admin routes resolve a
// bearer through. Both replace the V1 operatorBootstrapIdentity, which built a
// one-user in-memory StaticIdentity from env and was not pluggable.

// identityStores is the registry and the credential store local accounts use.
// Both registries implement both interfaces.
type identityStores struct {
	registry domain.PrincipalRegistry
	creds    identity.CredentialStore
}

// buildPrincipalRegistry returns the Postgres registry when the schema is there
// (migration 0032), else the in-memory one. The fallback is LOUD: an in-memory
// registry forgets every OIDC first login and every person but the env bootstrap
// operator on restart, which is correct only for the database-less edge profile.
func buildPrincipalRegistry(ctx context.Context, pool *pgxpool.Pool) identityStores {
	if pool != nil {
		pg, err := identity.NewPgRegistry(ctx, pool)
		if err == nil {
			return identityStores{registry: pg, creds: pg}
		}
		slog.Warn("ADR-0142: principal registry unavailable; using an in-memory registry "+
			"(persons other than the env bootstrap operator do not survive a restart)", "err", err)
	}
	mem := identity.NewMemRegistry()
	return identityStores{registry: mem, creds: mem}
}

// bootstrapOperatorFromEnv reads the V1 bootstrap operator: CAMBRIAN_OPERATOR_USER
// / CAMBRIAN_OPERATOR_PASSWORD, role CAMBRIAN_OPERATOR_ROLE (viewer, else
// operator). ok=false when unset — secure by default, no creds ⇒ no login.
func bootstrapOperatorFromEnv() (user, password string, role operator.Role, ok bool) {
	user, password = os.Getenv("CAMBRIAN_OPERATOR_USER"), os.Getenv("CAMBRIAN_OPERATOR_PASSWORD")
	if user == "" || password == "" {
		return "", "", "", false
	}
	role = operator.Role(os.Getenv("CAMBRIAN_OPERATOR_ROLE"))
	if role != operator.RoleViewer {
		role = operator.RoleOperator
	}
	return user, password, role, true
}

// buildAuthenticator composes the kernel Authenticator:
//
//  1. local accounts over the registry, with the env bootstrap operator
//     re-asserted as a local account on every boot (the migration from the env
//     vars: same username ⇒ same principal `user:<username>`, same role, and the
//     password still comes from env, so an existing deployment upgrades with no
//     change and its audit trail keeps naming the same operator);
//  2. the ONE configured OIDC issuer, when identity.oidc.issuer is set;
//  3. a plugin's NewAuthenticator (premium multi-issuer federation), which may
//     wrap or replace 1+2.
func buildAuthenticator(ctx context.Context, cfg *config.Config, st identityStores,
	newAuth NewAuthenticatorFunc) (domain.Authenticator, error) {
	local := identity.NewLocalAccounts(st.registry, st.creds)
	if user, pw, role, ok := bootstrapOperatorFromEnv(); ok {
		if _, err := local.EnsureAccount(ctx, user, pw, []string{string(role)}); err != nil {
			return nil, fmt.Errorf("ADR-0142: seeding the bootstrap operator %q as a local account: %w", user, err)
		}
		slog.Info("ADR-0142: bootstrap operator is a local account", "principal", "user:"+user, "role", string(role))
	}

	var oidc domain.Authenticator
	if o := cfg.Identity.OIDC; o.Enabled() {
		v, err := identity.NewOIDCVerifier(oidcConfigFrom(o), st.registry)
		if err != nil {
			return nil, fmt.Errorf("ADR-0142: identity.oidc: %w", err)
		}
		oidc = v
		slog.Info("ADR-0142: OIDC sign-in enabled", "issuer", o.Issuer, "client_id", o.ClientID,
			"first_login", firstLoginOf(o))
	}
	var auth domain.Authenticator = identity.NewChain(local, oidc)
	if newAuth != nil {
		a, err := newAuth(st.registry, auth)
		if err != nil {
			return nil, fmt.Errorf("ADR-0142: plugin authenticator: %w", err)
		}
		if a != nil {
			auth = a
		}
	}
	return auth, nil
}

func firstLoginOf(o config.OIDCIdentityConfig) identity.FirstLoginPolicy {
	if o.FirstLogin == string(identity.FirstLoginAutoCreate) {
		return identity.FirstLoginAutoCreate
	}
	return identity.FirstLoginRequireProvisioned
}

// oidcConfigFrom threads every parsed identity.oidc key to the verifier (AGENTS.md
// §3.4: a validated config field reaches a consumer in the same change).
func oidcConfigFrom(o config.OIDCIdentityConfig) identity.OIDCConfig {
	skew := 60 * time.Second
	if o.ClockSkewSeconds > 0 {
		skew = time.Duration(o.ClockSkewSeconds) * time.Second
	}
	return identity.OIDCConfig{
		Issuer:       strings.TrimSpace(o.Issuer),
		ClientID:     strings.TrimSpace(o.ClientID),
		Audiences:    o.Audiences,
		RolesClaim:   o.RolesClaim,
		RoleMap:      o.RoleMap,
		DefaultRoles: o.DefaultRoles,
		FirstLogin:   firstLoginOf(o),
		ClockSkew:    skew,
	}
}

// importMachineOwners registers every worker-machine owner (ADR-0127 tokens issued
// with a free-text --owner) as a registry person, with its legacy `agent:<id>`
// alias (ADR-0142 migration step 6). Idempotent, so it runs on every boot. An owner
// naming an unbound MCP client (`mcp:<c>`) is an agent, not a person, and is left
// alone (domain.PersonPrincipal).
func importMachineOwners(ctx context.Context, reg domain.PrincipalRegistry, clients []string,
	resolve func(string) (string, bool)) {
	for _, c := range clients {
		owner, ok := resolve(mcpserve.WorkerOwnerSecretName(c))
		owner = strings.TrimSpace(owner)
		if !ok || owner == "" || domain.PersonPrincipal(owner).Kind != domain.PrincipalUser {
			continue
		}
		if err := ImportPerson(ctx, reg, owner); err != nil {
			slog.Warn("ADR-0142: could not register a worker-machine owner as a person",
				"machine", c, "owner", owner, "err", err)
		}
	}
}

// ImportPerson is the one-time, idempotent import of a referenced person
// (ADR-0142 migration step 2): a registry row under the id it is already
// referenced by, plus the `agent:<id>` alias it ran under on MCP and chat before
// the user-kind switch, so audit queries match both forms. Exported for the
// premium import of bindings, person scope rows, group members and links.
func ImportPerson(ctx context.Context, reg domain.PrincipalRegistry, id string) error {
	if reg == nil {
		return nil
	}
	p, _, err := reg.EnsurePrincipal(ctx, domain.Principal{
		ID: id, Kind: domain.PrincipalUser, DisplayName: id, Source: domain.SourceImport,
	})
	if err != nil {
		return err
	}
	return reg.AddAlias(ctx, domain.LegacyAgentAlias(p.ID), p.ID)
}

// principalDirectoryOrRegistry is act-for's directory: a plugin's (premium answers
// registry ∪ legacy references during the migration window) or, in OSS, the
// registry itself — which makes act-for usable in OSS at all (ADR-0142 D3; before,
// it failed closed with no directory installed).
func principalDirectoryOrRegistry(plugin domain.PrincipalDirectory, reg domain.PrincipalRegistry) domain.PrincipalDirectory {
	if plugin != nil {
		return plugin
	}
	if reg == nil {
		return nil
	}
	return reg
}
