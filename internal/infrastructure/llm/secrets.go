package llm

import (
	"os"
	"sync"
)

// Credential resolution for generators (ADR-0101 D5).
//
// This file exists because the store half of that decision was written and then
// never read. `SetGeneratorKey` encrypted a credential, the console reported
// `key_configured` and its last four, and every client here called
// `os.Getenv(APIKeyEnv)` and nothing else — so a key saved from the console was
// stored, displayed, and never sent to anybody. The endpoint answered 401 while
// the panel showed a key installed, which is the most confusing possible pair of
// true statements.

// SecretResolver resolves a stored credential, honouring an environment variable
// that outranks it.
//
// Satisfied by storage.BoltConfigStore.Resolve. Deliberately the same
// env-beats-store precedence the console already reports in `key_source`: if the
// two disagreed, the console would name a source the kernel does not use.
type SecretResolver interface {
	Resolve(name, envVar string) string
}

var (
	secretsMu sync.RWMutex
	secrets   SecretResolver
)

// SetSecretResolver installs the process credential store.
//
// Process-wide and read at CALL time rather than injected at construction, and
// both halves of that are deliberate. There is one store per kernel, and it is
// opened before the provider is built. Resolving at call time is what lets a key
// saved from the console work on the very next request — resolving once, when
// the client was constructed, would mean a saved key did nothing until a restart
// nobody mentioned, which is the failure this whole file exists to end.
func SetSecretResolver(r SecretResolver) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	secrets = r
}

// GeneratorKeySecretName is the store name a generator's credential lives under.
//
// One function rather than the string built at each site: the writer, the reader
// and the console's "is a key installed?" check must agree exactly, and three
// copies of `"generator:" + id + ":api_key"` is three chances for a key to be
// stored where nothing looks for it.
func GeneratorKeySecretName(generatorID string) string {
	return "generator:" + generatorID + ":api_key"
}

// APIKeyFor returns the credential for one generator, or "".
//
// Precedence, matching ADR-0101 D5 and the console's `key_source`:
//  1. the environment variable, when set and non-empty — a deployment wins;
//  2. the encrypted store;
//  3. nothing.
//
// generatorID may be empty (a model-agent client configured straight from
// config, with no store entry of its own); the environment path still applies.
func APIKeyFor(generatorID, envVar string) string {
	secretsMu.RLock()
	r := secrets
	secretsMu.RUnlock()

	if r != nil && generatorID != "" {
		// Resolve already applies env-then-store, so a set variable still wins.
		if key := r.Resolve(GeneratorKeySecretName(generatorID), envVar); key != "" {
			return key
		}
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}
