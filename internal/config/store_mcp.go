package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/internal/procenv"
)

// MCP servers are stored PER ID, like generators (ADR-0101 D9, extended to MCP
// servers by the integrations surface, T-20261106-integrations-manage-mcp-
// servers-from-the).
//
// Contract 0097 stored the WHOLE server list under `mcp.servers`, and koanf
// replaces a list wholesale, so the first console save hid every server added
// to mcp.json afterwards. Each stored server now lives under its own key and is
// merged BY ID over the file layers' list at load: a stored server REPLACES the
// file server with the same id, a tombstone hides one, and a stored server with
// no file entry is appended. mcp.json is never rewritten by a console write
// (owner directive for integrations: operator state lives in the store, the
// file stays the deployment's own).
//
// Precedence for one server id, lowest to highest:
//
//	file layers (config.json … mcp.json, koanf per-key merge)
//	  < the store's per-id entry (whole-entry replace, or tombstone)
//	    < an env var pinning the whole `mcp.servers` list (D1; the merge is skipped)
//
// Credentials follow D5 separately: a kernel env var named in the server's
// `env` passthrough beats the stored secret of the same name.

// MCPServersKey is the config path of the MCP server list in the file layers.
const MCPServersKey = "mcp.servers"

// MCPServerOverridePrefix + id is the store key of one stored MCP server. The id
// is everything after the prefix, so an id containing dots is kept intact: these
// keys are split out of the overrides before the dotted-key expansion.
const MCPServerOverridePrefix = "mcp.server_overrides."

// MCPServerOverrideKey is the store key for server id.
func MCPServerOverrideKey(id string) string { return MCPServerOverridePrefix + id }

// StoredMCPServer is one stored server entry: the full server (it REPLACES the
// file entry with the same id) or a tombstone (Removed) hiding a file server the
// console removed.
type StoredMCPServer struct {
	MCPServerConfig
	Removed bool `json:"removed,omitempty"`
}

// IsEnabled reports whether the server should be started. nil ⇒ true.
func (s MCPServerConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// SplitMCPServerOverrides separates the store's MCP server entries from its
// other overrides. It reads both layouts: per-id keys and the contract-0097
// whole stored list under `mcp.servers`, each element of which becomes an entry
// unless a per-id key for the same id exists. legacy reports the old form.
func SplitMCPServerOverrides(overrides map[string]any) (servers map[string]StoredMCPServer, rest map[string]any, legacy bool) {
	servers = map[string]StoredMCPServer{}
	rest = make(map[string]any, len(overrides))
	var legacyRaw any
	for k, v := range overrides {
		switch {
		case strings.HasPrefix(k, MCPServerOverridePrefix):
			id := strings.TrimPrefix(k, MCPServerOverridePrefix)
			var ss StoredMCPServer
			if decodeInto(v, &ss) == nil && id != "" {
				ss.ID = id // the key is the identity
				servers[id] = ss
			}
		case k == MCPServersKey:
			legacyRaw, legacy = v, true
		default:
			rest[k] = v
		}
	}
	if legacy {
		var list []MCPServerConfig
		if decodeInto(legacyRaw, &list) == nil {
			for _, s := range list {
				if s.ID == "" {
					continue
				}
				if _, newer := servers[s.ID]; !newer {
					servers[s.ID] = StoredMCPServer{MCPServerConfig: s}
				}
			}
		}
	}
	return servers, rest, legacy
}

// MergeMCPServers applies stored entries by id over a base (file) list: replace
// in place, drop on tombstone, append new ids in id order (deterministic).
func MergeMCPServers(base []MCPServerConfig, stored map[string]StoredMCPServer) []MCPServerConfig {
	out := make([]MCPServerConfig, 0, len(base)+len(stored))
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s.ID] = true
		if ss, ok := stored[s.ID]; ok {
			if ss.Removed {
				continue
			}
			out = append(out, ss.MCPServerConfig)
			continue
		}
		out = append(out, s)
	}
	ids := make([]string, 0, len(stored))
	for id := range stored {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ss := stored[id]; !ss.Removed {
			out = append(out, ss.MCPServerConfig)
		}
	}
	return out
}

// MigrateStoredMCPServers rewrites a contract-0097 stored server list into
// per-id entries and deletes the list key (AGENTS.md §3.4: a schema change ships
// with its migration). Idempotent. Returns the migrated ids.
//
// Semantics change on purpose, as for generators: the old list HID every file
// server it did not name; per-id entries do not.
func MigrateStoredMCPServers(store Store) ([]string, error) {
	if store == nil {
		return nil, nil
	}
	overrides, err := store.Overrides()
	if err != nil {
		return nil, fmt.Errorf("migrate stored mcp servers: read: %w", err)
	}
	if _, ok := overrides[MCPServersKey]; !ok {
		return nil, nil
	}
	servers, _, _ := SplitMCPServerOverrides(overrides)
	ids := make([]string, 0, len(servers))
	for id, ss := range servers {
		if _, exists := overrides[MCPServerOverrideKey(id)]; exists {
			continue
		}
		if err := store.SetOverride(MCPServerOverrideKey(id), ss); err != nil {
			return nil, fmt.Errorf("migrate stored mcp servers: write %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	if err := store.DeleteOverride(MCPServersKey); err != nil {
		return nil, fmt.Errorf("migrate stored mcp servers: delete list: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mcpServerIDRE bounds a server id: it becomes part of every tool name
// (mcp:<id>/<tool>), a store key and a secret name.
var mcpServerIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidMCPServerID reports whether id is an acceptable server id.
func ValidMCPServerID(id string) bool { return mcpServerIDRE.MatchString(id) }

// ValidateMCPServer checks the integration fields of one server. It is run on
// every boot (Config.Validate) and on every console write, so a mistake in a
// file fails loudly at boot and a console write is refused before it is stored.
func ValidateMCPServer(s MCPServerConfig) []string {
	var errs []string
	p := fmt.Sprintf("mcp.servers[%s]", s.ID)
	for name := range s.EnvValues {
		switch {
		case !envNameRE.MatchString(name):
			errs = append(errs, fmt.Sprintf("%s.env_values: %q is not a valid environment variable name", p, name))
		case procenv.IsKernelName(name):
			errs = append(errs, fmt.Sprintf("%s.env_values: %q is a kernel variable (CAMBRIAN_*/LANGFUSE_*) and is never given to a server", p, name))
		case procenv.IsSecretName(name):
			errs = append(errs, fmt.Sprintf("%s.env_values: %q looks like a credential; store it as a secret (secret_env) instead of a literal value", p, name))
		}
	}
	for _, name := range s.SecretEnv {
		switch {
		case !envNameRE.MatchString(name):
			errs = append(errs, fmt.Sprintf("%s.secret_env: %q is not a valid environment variable name", p, name))
		case procenv.IsKernelName(name):
			errs = append(errs, fmt.Sprintf("%s.secret_env: %q is a kernel variable (CAMBRIAN_*/LANGFUSE_*) and is never given to a server", p, name))
		}
		if _, dup := s.EnvValues[name]; dup {
			errs = append(errs, fmt.Sprintf("%s: %q is both a literal env value and a secret", p, name))
		}
	}
	errs = append(errs, validateUserCredential(p, s)...)
	errs = append(errs, ValidateMCPServerSandbox(p, s.TrustTier, s.Sandbox)...)
	return errs
}

// validateUserCredential checks a server's ADR-0140 credential model. The
// injection mode must fit the transport: a stdio server has an environment
// and no request headers, an http/sse server the reverse.
func validateUserCredential(p string, s MCPServerConfig) []string {
	uc := s.UserCredential
	if uc == nil {
		return nil
	}
	var errs []string
	add := func(format string, a ...any) {
		errs = append(errs, p+".user_credential: "+fmt.Sprintf(format, a...))
	}
	switch uc.Scope {
	case "", CredentialShared:
		return nil // the per-server model; the remaining fields are inert
	case CredentialPerUser, CredentialEither:
	default:
		add("scope %q is not shared, per_user or either", uc.Scope)
		return errs
	}
	if uc.Provider == "" {
		add("provider is required for scope %s", uc.Scope)
	}
	switch uc.Inject {
	case "env":
		if s.Transport != "stdio" {
			add("inject env needs a stdio server (this one is %s)", s.Transport)
		}
		switch {
		case !envNameRE.MatchString(uc.Name):
			add("inject env needs name = a valid environment variable name")
		case procenv.IsKernelName(uc.Name):
			add("%q is a kernel variable and is never given to a server", uc.Name)
		}
		if _, dup := s.EnvValues[uc.Name]; dup {
			add("%q is also a literal env value", uc.Name)
		}
	case "bearer":
		if s.Transport == "stdio" {
			add("inject bearer needs an http/sse server")
		}
	case "header":
		if s.Transport == "stdio" {
			add("inject header needs an http/sse server")
		}
		if uc.Name == "" {
			add("inject header needs name = the header name")
		}
	default:
		add("inject %q is not env, bearer or header", uc.Inject)
	}
	if uc.SharedFallback && uc.Scope != CredentialEither {
		add("shared_fallback is only meaningful for scope either")
	}
	return errs
}
