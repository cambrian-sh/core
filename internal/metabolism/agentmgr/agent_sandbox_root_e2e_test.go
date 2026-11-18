//go:build linux

package agentmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2026 end to end through the REAL launch path as a root kernel with
// bubblewrap: an agent whose code directory sits under a root-owned 0711
// directory is refused by InstanceManager.GetOrBootInstance before anything is
// spawned, the error naming the path and the fix — even on a degrade tier —
// and nothing is registered or left behind. Opened to 0755, the same agent
// boots and answers health over its socket. Elsewhere it skips; the CI
// sandbox job runs it as root.
func TestE2E_RootKernelRefusesABindUnderATraverseOnlyAncestor(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs a root kernel")
	}
	if !bwrapHere() {
		t.Skip("bubblewrap not usable here")
	}
	root := e2eRoot(t)
	workRoot := filepath.Join(root, "w")
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: workRoot, KernelDir: filepath.Join(root, "kernel")})
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe"})

	locked := filepath.Join(root, "locked")
	exe := copyProbeExe(t, locked) // <root>/locked/probe/<test binary>
	if err := os.Chmod(locked, 0o711); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	def := &domain.AgentDefinition{ID: "probe", Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := im.GetOrBootInstance(ctx, def, "")
	var refused *sandbox.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a sandbox refusal, got %v", err)
	}
	for _, s := range []string{filepath.Dir(exe), locked + " (mode 0711)", "chmod 0755 " + locked} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the refusal must name %q: %v", s, err)
		}
	}
	for _, c := range refused.Report.Enforced {
		if c == sandbox.ControlLowPrivilege {
			t.Fatalf("an unbootable plan reported low_privilege enforced: %s", refused.Report.Summary())
		}
	}
	assertNothingRegistered(t, im, def)
	if m, _ := filepath.Glob(filepath.Join(workRoot, "first-party", "*")); len(m) != 0 {
		t.Fatalf("a refused boot left a scratch dir: %v", m)
	}
	t.Logf("refused: %v", err)

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("with 0755 ancestors the agent must boot: %v", err)
	}
	if err := waitHealthy(ctx, inst.SocketPath, 10*time.Second); err != nil {
		t.Fatalf("the booted agent does not answer health: %v", err)
	}
	im.mu.Lock()
	pe := im.exits[inst.ID]
	im.mu.Unlock()
	im.EvictInstance(inst.ID)
	select {
	case <-pe.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the evicted agent did not exit")
	}
}
