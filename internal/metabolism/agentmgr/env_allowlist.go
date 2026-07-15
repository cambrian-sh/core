package agentmgr

import (
	"os"
	"runtime"
	"strings"
)

// SEC-01 — agent environment allowlist.
//
// Agent processes were spawned with the kernel's full environment inherited
// (os.Environ()), so every agent could read the kernel's LLM API keys and other
// credentials. Agents reach the LLM through the kernel's substrate gateway
// (GenerateViaModelStream) and tools through MCP, so they never legitimately
// need a provider key. This builds a deny-by-default environment: OS essentials
// the process needs to start, plus an operator-configured passthrough, with any
// secret-looking variable stripped regardless.

// baseAgentEnvNames is the OS-essential allowlist — the variables a Python or
// native agent process needs merely to start and locate its runtime. Never
// secrets. Windows needs a broader base (the C runtime + Python resolve many
// paths from the environment); POSIX is lean.
var baseAgentEnvNames = func() []string {
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

// envKey normalizes a variable name for comparison. Windows environment names
// are case-insensitive; POSIX names are case-sensitive.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// secretFragments / secretPrefixes catch credential-shaped names so an agent can
// never receive one — even if an operator mistakenly adds it to the passthrough
// (defense in depth). Matched case-insensitively against the raw name.
var (
	secretFragments = []string{
		"API_KEY", "APIKEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD",
		"CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY", "SESSION_KEY",
	}
	secretPrefixes = []string{
		"CAMBRIAN_", "LANGFUSE_", "OPENAI_", "ANTHROPIC_", "GEMINI_",
		"OPENCODE_", "AWS_", "AZURE_", "GCP_", "GOOGLE_", "HF_", "HUGGINGFACE_",
	}
)

// isSecretEnvName reports whether a variable name looks like a credential and
// must never be passed to an agent.
func isSecretEnvName(name string) bool {
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

// allowlistedAgentEnv returns the deny-by-default environment for an agent
// process: the current environment filtered to the base allowlist plus the
// operator's configured passthrough names, minus any secret-looking variable.
func allowlistedAgentEnv(passthrough []string) []string {
	keep := make(map[string]bool, len(baseAgentEnvNames)+len(passthrough))
	for _, n := range baseAgentEnvNames {
		keep[envKey(n)] = true
	}
	for _, p := range passthrough {
		if p == "" {
			continue
		}
		keep[envKey(p)] = true
	}

	out := make([]string, 0, len(keep))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if isSecretEnvName(name) {
			continue // never, even if allowlisted
		}
		if keep[envKey(name)] {
			out = append(out, kv)
		}
	}
	return out
}
