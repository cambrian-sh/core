package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"

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

var (
	secretsMu sync.RWMutex
	secrets   SecretResolver
	// orgCreds is the installed account provider's org credentials (ADR-0141
	// phase 2; premium). nil in OSS.
	orgCreds domain.CredentialSource
)

// SetOrgCredentials installs the account provider that owns the
// organisation's SHARED server credentials (ADR-0141 phase 2). Once set, a
// shared credential resolves env-first, then from the provider — and the
// consumer-named store entries (mcp:<id>:token, mcp:<id>:env:<NAME>) are no
// longer read (phase 5): the provider's boot migration moved them into org
// accounts, and reading both would be two answers to one question. nil
// restores the OSS behaviour (store-backed).
func SetOrgCredentials(src domain.CredentialSource) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	orgCreds = src
}

// OrgCredentials returns the installed org-credential provider, or nil.
func OrgCredentials() domain.CredentialSource {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	return orgCreds
}

// orgCredential asks the provider for one shared slot. "" when none is bound
// (or the account was revoked — the server then connects without it and its
// health shows why).
func orgCredential(src domain.CredentialSource, serverID, slot string) string {
	c, err := src.Credential(context.Background(), domain.CredentialRequest{
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

// SetSecretResolver installs the process credential store. Process-wide and read
// at connect time, so a token saved from the console works on the very next
// (re)connect without a restart.
func SetSecretResolver(r SecretResolver) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	secrets = r
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
func tokenFor(serverID, envVar string) string {
	secretsMu.RLock()
	r, org := secrets, orgCreds
	secretsMu.RUnlock()

	if org != nil && serverID != "" {
		// A deployment's environment still wins (D5), then the org account.
		if envVar != "" {
			if v := os.Getenv(envVar); v != "" {
				return v
			}
		}
		return orgCredential(org, serverID, domain.SlotToken)
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
func envSecretFor(serverID, name, envVar string) string {
	secretsMu.RLock()
	r, org := secrets, orgCreds
	secretsMu.RUnlock()
	if org != nil && serverID != "" {
		if envVar != "" {
			if v := os.Getenv(envVar); v != "" {
				return v
			}
		}
		return orgCredential(org, serverID, domain.SlotEnvPrefix+name)
	}
	if r != nil && serverID != "" {
		return r.Resolve(EnvSecretName(serverID, name), envVar)
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}
