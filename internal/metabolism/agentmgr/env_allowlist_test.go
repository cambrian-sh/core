package agentmgr

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/procenv"
)

// SEC-01 per-agent memory cap resolution: declared manifest limit wins; system
// organs are exempt from the fleet-wide default; user agents get the default.
func TestEffectiveMemLimitMB(t *testing.T) {
	im := withPython(NewInstanceManager("localhost:0"), "python")
	im.SetAgentMemoryLimitMB(1024) // global default
	manifests := map[string]*domain.AgentManifest{
		"heavy_user_agent": {MemoryLimitMB: 8192}, // declares its own headroom
		"docling_agent":    {MemoryLimitMB: 6144}, // system organ WITH a declared limit
	}
	im.SetManifestResolver(func(_ context.Context, id string) *domain.AgentManifest { return manifests[id] })

	// A declared limit always wins (user or system).
	if got := im.effectiveMemLimitMB(context.Background(), "heavy_user_agent"); got != 8192 {
		t.Errorf("declared user limit: want 8192, got %d", got)
	}
	if got := im.effectiveMemLimitMB(context.Background(), "docling_agent"); got != 6144 {
		t.Errorf("declared system limit: want 6144, got %d", got)
	}
	// A user agent with no declared limit gets the global default.
	if got := im.effectiveMemLimitMB(context.Background(), "analyst_agent"); got != 1024 {
		t.Errorf("undeclared user agent: want global default 1024, got %d", got)
	}
	// A system organ with no declared limit is EXEMPT from the fleet-wide default.
	if !domain.IsSystemAgent("retrieval_agent") {
		t.Fatal("test precondition: retrieval_agent should be a system agent")
	}
	if got := im.effectiveMemLimitMB(context.Background(), "retrieval_agent"); got != 0 {
		t.Errorf("undeclared system organ must be uncapped by the default, got %d", got)
	}
}

// The allowlist itself (secret stripping, base names, passthrough) lives in
// internal/procenv and is tested there; agentmgr only calls procenv.Allowlisted.

func envHas(env []string, name string) (string, bool) {
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 && procenv.Key(kv[:eq]) == procenv.Key(name) {
			return kv[eq+1:], true
		}
	}
	return "", false
}

// The agent launcher really applies the strict allowlist: a kernel secret in the
// environment does not reach the agent's command.
func TestBuildAgentCmd_EnvIsAllowlisted(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "sk-super-secret")
	t.Setenv("SOME_UNRELATED_VAR", "nope")
	im := withPython(NewInstanceManager("localhost:50051"), "python")
	def := &domain.AgentDefinition{ID: "a", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "i1"}, "")
	if err != nil {
		t.Fatalf("buildAgentCmd: %v", err)
	}
	if _, ok := envHas(cmd.Env, "OPENCODE_API_KEY"); ok {
		t.Error("SECRET LEAK: OPENCODE_API_KEY passed to agent env")
	}
	if _, ok := envHas(cmd.Env, "SOME_UNRELATED_VAR"); ok {
		t.Error("deny-by-default failed: unrelated var passed through")
	}
}

// Config-derived extras reach the agent env WITHOUT the kernel's own
// environment carrying the variable — the owner rule (2026-08-11) is that a
// knob never depends on .env, and the extras seam is what enforces it: the
// value flows config → SetEnvExtras → child env, and the kernel process env is
// never consulted.
func TestBuildAgentCmd_EnvExtrasComeFromConfigNotKernelEnv(t *testing.T) {
	// Deliberately NOT set in the kernel's environment.
	os.Unsetenv("CAMBRIAN_TOOL_MENU_K")

	im := withPython(NewInstanceManager("localhost:50051"), "python")
	im.SetEnvExtras([]string{"CAMBRIAN_TOOL_MENU_K=5"})

	def := &domain.AgentDefinition{ID: "a", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "i1"}, "")
	if err != nil {
		t.Fatalf("buildAgentCmd: %v", err)
	}
	if v, ok := envHas(cmd.Env, "CAMBRIAN_TOOL_MENU_K"); !ok || v != "5" {
		t.Fatalf("config-derived extra missing from agent env: got %q ok=%v", v, ok)
	}
}
