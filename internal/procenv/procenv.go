// Package procenv builds the deny-by-default environment for every process the
// kernel launches (ADR-0139 D8: "Environment: allowlisted for every launched
// process, including MCP stdio servers").
//
// It was SEC-01's agent allowlist (internal/metabolism/agentmgr), extracted here
// so the agent launcher and the MCP stdio launcher share ONE allowlist rather
// than two copies that drift (AGENTS.md §4.3). It imports only the standard
// library, so any launcher — however low in the tree — can use it.
//
// Spawning with an inherited os.Environ() hands the child every credential the
// kernel holds: provider API keys, CAMBRIAN_SECRET_KEY, Langfuse keys. Children
// reach the LLM through the kernel's gateway and do not need them. The
// environment built here is: the OS essentials a process needs to start, plus an
// operator-configured passthrough, with credential-shaped names stripped.
package procenv

import (
	"os"
	"runtime"
	"strings"
)

// baseNames is the OS-essential allowlist — the variables a Python, Node or
// native process needs merely to start and locate its runtime. Never secrets.
// Windows needs a broader base (the C runtime, Python and npx resolve many paths
// from the environment); POSIX is lean.
var baseNames = func() []string {
	names := []string{
		"PATH", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TZ", "HOME",
		"TMPDIR", "TMP", "TEMP",
	}
	if runtime.GOOS == "windows" {
		names = append(names,
			"PATHEXT", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC",
			"HOMEDRIVE", "HOMEPATH", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"PROGRAMDATA", "ALLUSERSPROFILE", "PROGRAMFILES", "PROGRAMFILES(X86)",
			"PROGRAMW6432", "COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)",
			"PUBLIC", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
			"PROCESSOR_IDENTIFIER", "OS", "USERNAME", "USERDOMAIN",
		)
	}
	return names
}()

// Key normalizes a variable name for comparison. Windows environment names are
// case-insensitive; POSIX names are case-sensitive.
func Key(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// secretFragments / secretPrefixes catch credential-shaped names. Matched
// case-insensitively against the raw name.
//
// kernelPrefixes are the kernel's OWN credentials and configuration. Unlike a
// third-party key, no child process has a legitimate need for them, so they are
// stripped in every mode — including the mode that lets an operator pass a named
// secret through to an MCP server.
var (
	secretFragments = []string{
		"API_KEY", "APIKEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD",
		"CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY", "SESSION_KEY",
	}
	secretPrefixes = []string{
		"CAMBRIAN_", "LANGFUSE_", "OPENAI_", "ANTHROPIC_", "GEMINI_",
		"OPENCODE_", "AWS_", "AZURE_", "GCP_", "GOOGLE_", "HF_", "HUGGINGFACE_",
	}
	kernelPrefixes = []string{"CAMBRIAN_", "LANGFUSE_"}
)

// IsSecretName reports whether a variable name looks like a credential.
func IsSecretName(name string) bool {
	u := strings.ToUpper(name)
	for _, f := range secretFragments {
		if strings.Contains(u, f) {
			return true
		}
	}
	for _, p := range secretPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// isKernelName reports whether a variable belongs to the kernel itself.
func isKernelName(name string) bool {
	u := strings.ToUpper(name)
	for _, p := range kernelPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// Allowlisted returns the strict deny-by-default environment: the current
// environment filtered to the base allowlist plus passthrough, minus any
// secret-looking variable — even one the operator listed (defense in depth).
// This is the agent posture: an agent never legitimately needs a credential.
func Allowlisted(passthrough []string) []string {
	return build(passthrough, false)
}

// AllowlistedNamedSecrets is Allowlisted for a launched TOOL server (MCP stdio).
// A third-party server often needs its own credential — firecrawl-mcp reads
// FIRECRAWL_API_KEY — so a secret-shaped name passes when the operator NAMED it
// in that server's passthrough. Nothing secret-shaped arrives unnamed, and the
// kernel's own variables (CAMBRIAN_*, LANGFUSE_*) never pass at all.
func AllowlistedNamedSecrets(passthrough []string) []string {
	return build(passthrough, true)
}

func build(passthrough []string, namedSecretsAllowed bool) []string {
	keep := make(map[string]bool, len(baseNames)+len(passthrough))
	for _, n := range baseNames {
		keep[Key(n)] = true
	}
	named := make(map[string]bool, len(passthrough))
	for _, p := range passthrough {
		if p == "" {
			continue
		}
		keep[Key(p)] = true
		named[Key(p)] = true
	}

	out := make([]string, 0, len(keep))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		k := Key(name)
		if !keep[k] || isKernelName(name) {
			continue
		}
		if IsSecretName(name) && !(namedSecretsAllowed && named[k]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
