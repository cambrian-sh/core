package procenv

import (
	"strings"
	"testing"
)

func envHas(env []string, name string) (string, bool) {
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 && Key(kv[:eq]) == Key(name) {
			return kv[eq+1:], true
		}
	}
	return "", false
}

func TestIsSecretName(t *testing.T) {
	secret := []string{
		"OPENCODE_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY",
		"CAMBRIAN_OPERATOR_PASSWORD", "CAMBRIAN_ADMIN_TOKEN", "LANGFUSE_SECRET_KEY",
		"AWS_SECRET_ACCESS_KEY", "HF_TOKEN", "DB_PASSWORD", "my_private_key",
	}
	for _, s := range secret {
		if !IsSecretName(s) {
			t.Errorf("expected %q to be treated as a secret", s)
		}
	}
	notSecret := []string{"PATH", "LANG", "TMPDIR", "PYTHONUTF8", "HOME", "SYSTEMROOT"}
	for _, s := range notSecret {
		if IsSecretName(s) {
			t.Errorf("expected %q NOT to be treated as a secret", s)
		}
	}
}

func TestAllowlisted_StripsSecrets_KeepsBase(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "sk-super-secret")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "hunter2")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SOME_UNRELATED_VAR", "nope")
	t.Setenv("MY_AGENT_CACHE", "/tmp/cache")

	env := Allowlisted([]string{"MY_AGENT_CACHE"})

	for _, leaked := range []string{"OPENCODE_API_KEY", "CAMBRIAN_OPERATOR_PASSWORD"} {
		if _, ok := envHas(env, leaked); ok {
			t.Errorf("SECRET LEAK: %s passed through", leaked)
		}
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

// The strict (agent) mode: an operator cannot allowlist a secret name.
func TestAllowlisted_PassthroughCannotOverrideSecretGuard(t *testing.T) {
	t.Setenv("SNEAKY_API_KEY", "leak")
	if _, ok := envHas(Allowlisted([]string{"SNEAKY_API_KEY"}), "SNEAKY_API_KEY"); ok {
		t.Error("SECRET LEAK: a secret-named var was passed because it was allowlisted")
	}
}

// The tool-server mode: a NAMED third-party secret passes (firecrawl needs its
// key), an unnamed one does not, and the kernel's own variables never do.
func TestAllowlistedNamedSecrets(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEY", "fc-key")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("CAMBRIAN_SECRET_KEY", "master")
	t.Setenv("LANGFUSE_SECRET_KEY", "lf")

	env := AllowlistedNamedSecrets([]string{"FIRECRAWL_API_KEY", "CAMBRIAN_SECRET_KEY", "LANGFUSE_SECRET_KEY"})
	if v, ok := envHas(env, "FIRECRAWL_API_KEY"); !ok || v != "fc-key" {
		t.Errorf("named server secret must pass: %q ok=%v", v, ok)
	}
	for _, leaked := range []string{"ANTHROPIC_API_KEY", "CAMBRIAN_SECRET_KEY", "LANGFUSE_SECRET_KEY"} {
		if _, ok := envHas(env, leaked); ok {
			t.Errorf("SECRET LEAK: %s reached a tool server", leaked)
		}
	}
}
