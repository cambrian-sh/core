package procenv

import (
	"strings"
	"testing"
)

// ServerEnv: explicit per-server values (settings + that server's secrets) on
// top of the allowlist; an explicit value replaces an inherited one; kernel
// variables never pass, even when given explicitly.
func TestServerEnv_ExplicitValuesOverlayTheAllowlist(t *testing.T) {
	t.Setenv("PASS_ME", "inherited")
	t.Setenv("OTHER_API_KEY", "must-not-leak")
	env := strings.Join(ServerEnv([]string{"PASS_ME"}, map[string]string{
		"PASS_ME":             "explicit",
		"SANDBOX_MODE":        "none",
		"SERVICE_API_KEY":     "server-secret",
		"CAMBRIAN_SECRET_KEY": "kernel",
		"LANGFUSE_PUBLIC_KEY": "kernel",
	}), "\n")
	for _, want := range []string{"PASS_ME=explicit", "SANDBOX_MODE=none", "SERVICE_API_KEY=server-secret"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	for _, bad := range []string{"PASS_ME=inherited", "must-not-leak", "CAMBRIAN_SECRET_KEY", "LANGFUSE_PUBLIC_KEY"} {
		if strings.Contains(env, bad) {
			t.Errorf("%s reached the server", bad)
		}
	}
}
