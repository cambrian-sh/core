package mcp

import (
	"os"
	"sync"
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
)

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
// environment variable when set (a deployment wins), then the encrypted store.
func tokenFor(serverID, envVar string) string {
	secretsMu.RLock()
	r := secrets
	secretsMu.RUnlock()

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
