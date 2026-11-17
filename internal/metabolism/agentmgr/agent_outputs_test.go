package agentmgr

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// recordingCollector stands in for the ledger-side collector: it reads what is
// in out/ at the moment the kernel hands it over.
type recordingCollector struct {
	mu    sync.Mutex
	calls []domain.AgentOutputs
	files map[string]string
}

func (c *recordingCollector) CollectAgentOutputs(_ context.Context, out domain.AgentOutputs) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, out)
	_ = filepath.WalkDir(out.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(out.Dir, p)
			c.files[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return len(c.files), nil
}

// ADR-0157 D5.1 through the real launch path: a sandboxed instance gets
// CAMBRIAN_AGENT_OUT inside its scratch; when it is evicted the kernel hands
// out/ — still there — to the collector with the kernel's own agent and
// instance ids, and only then removes the scratch.
func TestE2E_OutCollectedBeforeTheScratchIsRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	root := e2eRoot(t)
	workRoot := filepath.Join(root, "w")
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: workRoot, KernelDir: filepath.Join(root, "kernel")})
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	col := &recordingCollector{files: map[string]string{}}
	im.SetOutputCollector(col)
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe"})
	exe, _ := os.Executable()
	def := &domain.AgentDefinition{ID: "probe", Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := waitHealthy(ctx, inst.SocketPath, 10*time.Second); err != nil {
		t.Fatalf("probe not serving: %v", err)
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
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.calls) != 1 || col.calls[0].AgentID != "probe" || col.calls[0].InstanceID != inst.ID {
		t.Fatalf("collector calls = %+v (instance %s)", col.calls, inst.ID)
	}
	if col.files["sub/result.txt"] != "probe result" {
		t.Fatalf("out/ at collection = %v", col.files)
	}
	if m, _ := filepath.Glob(filepath.Join(workRoot, "first-party", "probe-*")); len(m) != 0 {
		t.Fatalf("scratch survived the exit: %v", m)
	}
}

// Without a collector out/ goes with the scratch: nothing leaves.
func TestCollectOutputs_NoCollectorIsANoop(t *testing.T) {
	im := NewInstanceManager("", "127.0.0.1:1")
	im.collectOutputs(context.Background(), "a", "i", t.TempDir()) // must not panic
}
