package agentmgr

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// SEC-01 per-agent memory cap resolution: declared manifest limit wins; system
// organs are exempt from the fleet-wide default; user agents get the default.
func TestEffectiveMemLimitMB(t *testing.T) {
	im := NewInstanceManager("python", "localhost:0")
	im.SetAgentMemoryLimitMB(1024) // global default
	manifests := map[string]*domain.AgentManifest{
		"heavy_user_agent": {MemoryLimitMB: 8192}, // declares its own headroom
		"docling_agent":    {MemoryLimitMB: 6144}, // system organ WITH a declared limit
	}
	im.SetManifestResolver(func(id string) *domain.AgentManifest { return manifests[id] })

	// A declared limit always wins (user or system).
	if got := im.effectiveMemLimitMB("heavy_user_agent"); got != 8192 {
		t.Errorf("declared user limit: want 8192, got %d", got)
	}
	if got := im.effectiveMemLimitMB("docling_agent"); got != 6144 {
		t.Errorf("declared system limit: want 6144, got %d", got)
	}
	// A user agent with no declared limit gets the global default.
	if got := im.effectiveMemLimitMB("analyst_agent"); got != 1024 {
		t.Errorf("undeclared user agent: want global default 1024, got %d", got)
	}
	// A system organ with no declared limit is EXEMPT from the fleet-wide default.
	if !domain.IsSystemAgent("scout_agent") {
		t.Fatal("test precondition: scout_agent should be a system agent")
	}
	if got := im.effectiveMemLimitMB("scout_agent"); got != 0 {
		t.Errorf("undeclared system organ must be uncapped by the default, got %d", got)
	}
}

func TestIsSecretEnvName(t *testing.T) {
	secret := []string{
		"OPENCODE_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY",
		"CAMBRIAN_OPERATOR_PASSWORD", "CAMBRIAN_ADMIN_TOKEN", "LANGFUSE_SECRET_KEY",
		"AWS_SECRET_ACCESS_KEY", "HF_TOKEN", "DB_PASSWORD", "my_private_key",
	}
	for _, s := range secret {
		if !isSecretEnvName(s) {
			t.Errorf("expected %q to be treated as a secret", s)
		}
	}
	notSecret := []string{"PATH", "LANG", "TMPDIR", "PYTHONUTF8", "HOME", "SYSTEMROOT"}
	for _, s := range notSecret {
		if isSecretEnvName(s) {
			t.Errorf("expected %q NOT to be treated as a secret", s)
		}
	}
}

func envHas(env []string, name string) (string, bool) {
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 && envKey(kv[:eq]) == envKey(name) {
			return kv[eq+1:], true
		}
	}
	return "", false
}

func TestAllowlistedAgentEnv_StripsSecrets_KeepsBase(t *testing.T) {
	// A kernel secret, an OS essential, an operator passthrough, and an
	// unrelated var. Only the base + declared passthrough (non-secret) survive.
	t.Setenv("OPENCODE_API_KEY", "sk-super-secret")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "hunter2")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SOME_UNRELATED_VAR", "nope")
	t.Setenv("MY_AGENT_CACHE", "/tmp/cache")

	env := allowlistedAgentEnv([]string{"MY_AGENT_CACHE"})

	if _, ok := envHas(env, "OPENCODE_API_KEY"); ok {
		t.Error("SECRET LEAK: OPENCODE_API_KEY passed to agent env")
	}
	if _, ok := envHas(env, "CAMBRIAN_OPERATOR_PASSWORD"); ok {
		t.Error("SECRET LEAK: CAMBRIAN_OPERATOR_PASSWORD passed to agent env")
	}
	if _, ok := envHas(env, "SOME_UNRELATED_VAR"); ok {
		t.Error("deny-by-default failed: unrelated var passed through")
	}
	if v, ok := envHas(env, "PATH"); !ok || v != "/usr/bin" {
		t.Errorf("base allowlist var PATH missing/wrong: %q ok=%v", v, ok)
	}
	if v, ok := envHas(env, "MY_AGENT_CACHE"); !ok || v != "/tmp/cache" {
		t.Errorf("declared passthrough MY_AGENT_CACHE missing/wrong: %q ok=%v", v, ok)
	}
}

// An operator must not be able to allowlist a secret name — the secret guard
// wins over the passthrough.
func TestAllowlistedAgentEnv_PassthroughCannotOverrideSecretGuard(t *testing.T) {
	t.Setenv("SNEAKY_API_KEY", "leak")
	env := allowlistedAgentEnv([]string{"SNEAKY_API_KEY"})
	if _, ok := envHas(env, "SNEAKY_API_KEY"); ok {
		t.Error("SECRET LEAK: a secret-named var was passed because it was allowlisted")
	}
}
