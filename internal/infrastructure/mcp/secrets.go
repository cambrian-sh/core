package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Credential resolution for MCP servers (contract 0097, following ADR-0101 D5
// and the generator precedent in internal/infrastructure/llm/secrets.go).
//
// Config references a credential by environment-variable NAME (`auth.token_env`)
// and historically the connector read that variable and nothing else. A token
// saved from the console lands in the encrypted store instead, so the connector
// must consult both — with the same env-beats-store precedence the console
// reports, or the console would name a source the kernel does not use.

// SecretResolver resolves a stored credential, honouring an environment variable
// that outranks it. Satisfied by storage.BoltConfigStore.Resolve.
type SecretResolver interface {
	Resolve(name, envVar string) string
}

// CredentialSources are where a connector's credentials come from, handed to
// NewConnector so they are in place before the first connect and fixed for the
// connector's life (T-20261113-mcp-credential-source-and-secret-resolver-are).
// They used to be process-wide package globals (SetSecretResolver,
// SetOrgCredentials) and a post-construction setter (SetUserCredentials) set
// AFTER bootstrapKernel had already run the boot ConnectAll — so the boot
// connect of a server whose token was saved from the console (or held by an
// org account) ran with no credential and only recovered on a health
// reconnect — and tests and a second connector shared them.
type CredentialSources struct {
	// Secrets is the ADR-0101 credential store (env-then-store precedence);
	// nil ⇒ environment variables only.
	Secrets SecretResolver
	// Org is the account provider that owns the organisation's SHARED server
	// credentials (ADR-0141 phase 2; premium). When set, a shared credential
	// resolves env-first, then from the provider — and the consumer-named
	// store entries (mcp:<id>:token, mcp:<id>:env:<NAME>) are not read (phase
	// 5): reading both would be two answers to one question. nil ⇒ the OSS
	// behaviour (store-backed).
	Org domain.CredentialSource
	// User is the per-user credential source (ADR-0140/0141); nil ⇒ per_user
	// servers cannot be used (calls fail "not connected"): the OSS default,
	// and a premium build whose accounts plugin is off.
	User domain.CredentialSource
	// UserLimits bounds the per-(server, principal) session pool; zero fields
	// take the defaults (32 sessions, 10 min idle).
	UserLimits UserPoolLimits
}

// orgCredentialTimeout bounds one ask of the org-credential provider. The
// provider may refresh an OAuth token over the network, so the bound is
// generous; without one a hung provider stalled a server connect (and the boot
// ConnectAll) forever (AGENTS.md §2.7,
// T-20261113-mcp-credential-source-and-secret-resolver-are).
const orgCredentialTimeout = 30 * time.Second

// orgCredential asks the provider for one shared slot, under the caller's
// context (its values and cancellation) bounded by orgCredentialTimeout. ""
// when none is bound (or the account was revoked, or the ask timed out — the
// server then connects without it and its health shows why).
func orgCredential(ctx context.Context, src domain.CredentialSource, serverID, slot string) string {
	ctx, cancel := context.WithTimeout(ctx, orgCredentialTimeout)
	defer cancel()
	c, err := src.Credential(ctx, domain.CredentialRequest{
		ServerID: serverID, Purpose: domain.CredentialPurposeMCP, Slot: slot,
	})
	if err != nil {
		if !errors.Is(err, domain.ErrCredentialNotConnected) {
			slog.Warn("ADR-0141: org account for an MCP server is unusable", "server", serverID, "slot", slot, "err", err)
		}
		return ""
	}
	return c.Token
}

// TokenSecretName is the store name an MCP server's credential lives under. One
// function so the writer, the connector and the console's "is a token
// installed?" check agree exactly.
func TokenSecretName(serverID string) string {
	return "mcp:" + serverID + ":token"
}

// tokenFor returns the credential for one server, or "". Precedence: the
// environment variable when set (a deployment wins), then the org account
// bound to the server when an account provider is installed (ADR-0141), else
// the encrypted store (the OSS default).
func (cs CredentialSources) tokenFor(ctx context.Context, serverID, envVar string) string {
	r, org := cs.Secrets, cs.Org

	if org != nil && serverID != "" {
		// A deployment's environment still wins (D5), then the org account.
		if envVar != "" {
			if v := os.Getenv(envVar); v != "" {
				return v
			}
		}
		return orgCredential(ctx, org, serverID, domain.SlotToken)
	}
	if r != nil && serverID != "" {
		// Resolve already applies env-then-store, so a set variable still wins.
		if tok := r.Resolve(TokenSecretName(serverID), envVar); tok != "" {
			return tok
		}
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

// EnvSecretName is the store name of one env-injected secret of one server
// (integrations: secret_env). Per server AND per variable, so two servers that
// both read POSTGRES_PASSWORD hold two independent credentials, and a value is
// only ever injected into the server it was saved for.
func EnvSecretName(serverID, envName string) string {
	return "mcp:" + serverID + ":env:" + envName
}

// envSecretFor resolves one env-injected secret: the kernel environment
// variable envVar when set (D5: a deployment wins; envVar is non-empty only
// when the server passes that name through), then the org account bound to
// (server, env:<name>) when an account provider is installed (ADR-0141), else
// the encrypted store.
func (cs CredentialSources) envSecretFor(ctx context.Context, serverID, name, envVar string) string {
	r, org := cs.Secrets, cs.Org
	if org != nil && serverID != "" {
		if envVar != "" {
			if v := os.Getenv(envVar); v != "" {
				return v
			}
		}
		return orgCredential(ctx, org, serverID, domain.SlotEnvPrefix+name)
	}
	if r != nil && serverID != "" {
		return r.Resolve(EnvSecretName(serverID, name), envVar)
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}
