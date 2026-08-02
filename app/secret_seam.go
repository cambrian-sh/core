package app

import (
	"errors"
	"sync"
)

var (
	errConfigStoreOff  = errors.New("named-secret store unavailable: the config store is off or not yet attached")
	errSecretNameEmpty = errors.New("named secret requires a name")
)

// The named-secret seam behind KernelServices.ResolveNamedSecret (ADR-0112).
//
// The config store is opened in Run and deliberately NOT passed into
// bootstrapKernel — "the store IS a config layer" (see Run). Plugins, however,
// are built inside bootstrapKernel, before the store is attached to the
// kernel. The same tension was solved once already for LLM credentials with
// llm.SetSecretResolver: a package-level holder set from Run, read late. This
// is that pattern, kept in app because the consumer is the plugin seam rather
// than the llm clients.
//
// Late binding is safe for the one consumer this exists for: transports
// resolve credentials per delivery at serve time, long after Run has attached
// the store. A call before attachment reports ok=false — honest degradation,
// never a panic.

// namedSecretSource is what the seam needs from the store: the ADR-0101
// env-then-store resolution. *storage.BoltConfigStore satisfies it.
type namedSecretSource interface {
	Resolve(name, envVar string) string
}

// namedSecretAdmin is the WRITE half (ADR-0112 §13, the DW-3 credential
// plane): encrypted set/clear plus presence reporting — never a read-back.
// *storage.BoltConfigStore satisfies it via config.SecretStore.
type namedSecretAdmin interface {
	SetSecret(name, value string) error
	ClearSecret(name string) error
	Configured(name string) bool
	LastFour(name string) string
}

var (
	namedSecretMu  sync.RWMutex
	namedSecretSrc namedSecretSource
	namedSecretAdm namedSecretAdmin
)

// setNamedSecretSource attaches the secret source. Called from Run once the
// config store exists; a nil store leaves the seam answering ok=false. When
// the same value also satisfies the admin half, writes come alive too.
func setNamedSecretSource(s namedSecretSource) {
	namedSecretMu.Lock()
	defer namedSecretMu.Unlock()
	namedSecretSrc = s
	if adm, ok := s.(namedSecretAdmin); ok {
		namedSecretAdm = adm
	}
}

// storeNamedSecret is KernelServices.StoreNamedSecret: encrypt-and-store one
// named credential. The value crosses this function and is never retained by
// the caller's plane — write-only credentials, the telegram/generator rule.
func storeNamedSecret(name, value string) error {
	namedSecretMu.RLock()
	adm := namedSecretAdm
	namedSecretMu.RUnlock()
	if adm == nil {
		return errConfigStoreOff
	}
	if name == "" {
		return errSecretNameEmpty
	}
	return adm.SetSecret(name, value)
}

// clearNamedSecret is KernelServices.ClearNamedSecret.
func clearNamedSecret(name string) error {
	namedSecretMu.RLock()
	adm := namedSecretAdm
	namedSecretMu.RUnlock()
	if adm == nil {
		return errConfigStoreOff
	}
	return adm.ClearSecret(name)
}

// namedSecretStatus is KernelServices.NamedSecretStatus: presence + last
// four, never the value.
func namedSecretStatus(name string) (bool, string) {
	namedSecretMu.RLock()
	adm := namedSecretAdm
	namedSecretMu.RUnlock()
	if adm == nil || name == "" {
		return false, ""
	}
	if !adm.Configured(name) {
		return false, ""
	}
	return true, adm.LastFour(name)
}

// resolveNamedSecret is the function handed to plugins as
// KernelServices.ResolveNamedSecret. No env indirection: a plugin credential
// has no environment form — that is the SEC-01 lesson (credential-shaped env
// vars leak into daemon environments).
func resolveNamedSecret(name string) (string, bool) {
	namedSecretMu.RLock()
	src := namedSecretSrc
	namedSecretMu.RUnlock()
	if src == nil || name == "" {
		return "", false
	}
	v := src.Resolve(name, "")
	return v, v != ""
}
