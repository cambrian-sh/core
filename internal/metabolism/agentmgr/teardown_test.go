package agentmgr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1963: every teardown path (EvictInstance, ReleaseInstance, EvictAgent,
// KillAllAgents) runs the instance's resource-cap cleanup exactly once —
// the Windows Job Object close on the unsandboxed launch — removes its
// socket and its token, and runs that cleanup with the manager lock RELEASED
// (a docker-run agent's cleanup is a `docker rm -f`).

type teardownProbe struct {
	calls     atomic.Int32
	underLock atomic.Int32
}

// probeLockCheck serialises the probes' own TryLock/Unlock of im.mu.
// KillAllAgents runs its teardowns concurrently, outside the lock; without
// this, one probe's momentary TryLock hold made a sibling probe's TryLock
// fail and count a false "ran under the lock" (ENG-1984: 79 of 500 runs of
// the KillAllAgents subtest under -race). With the checks serialised, a
// failed TryLock can only mean the teardown path itself holds im.mu.
var probeLockCheck sync.Mutex

// placeInstance registers a fake instance with a socket file and a cap
// cleanup that records whether im.mu was held while it ran.
func placeInstance(t *testing.T, im *InstanceManager, agentID string, mode domain.InstanceMode, p *teardownProbe) *domain.Instance {
	t.Helper()
	inst := domain.NewInstance(agentID)
	inst.Mode = mode
	inst.SocketPath = filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(inst.SocketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := im.tokens.Mint(agentID, inst.ID); err != nil {
		t.Fatal(err)
	}
	im.mu.Lock()
	im.instances[inst.ID] = inst
	im.agentIndex[agentID] = append(im.agentIndex[agentID], inst.ID)
	im.capCleanups[inst.ID] = func() {
		p.calls.Add(1)
		probeLockCheck.Lock()
		defer probeLockCheck.Unlock()
		if im.mu.TryLock() {
			im.mu.Unlock()
		} else {
			p.underLock.Add(1)
		}
	}
	im.mu.Unlock()
	return inst
}

func assertTornDown(t *testing.T, im *InstanceManager, p *teardownProbe, want int32, insts ...*domain.Instance) {
	t.Helper()
	if got := p.calls.Load(); got != want {
		t.Errorf("cap cleanup ran %d times, want %d", got, want)
	}
	if n := p.underLock.Load(); n != 0 {
		t.Errorf("cap cleanup ran %d times while the manager lock was held", n)
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, inst := range insts {
		if _, ok := im.capCleanups[inst.ID]; ok {
			t.Errorf("instance %s: capCleanups entry left behind", inst.ID)
		}
		if _, ok := im.instances[inst.ID]; ok {
			t.Errorf("instance %s still tracked", inst.ID)
		}
		if _, err := os.Stat(inst.SocketPath); !os.IsNotExist(err) {
			t.Errorf("instance %s: socket %s not removed (%v)", inst.ID, inst.SocketPath, err)
		}
	}
	if im.tokens.Len() != 0 {
		t.Errorf("%d tokens still live", im.tokens.Len())
	}
	if len(im.agentIndex) != 0 {
		t.Errorf("agentIndex not empty: %v", im.agentIndex)
	}
}

func TestTeardownPaths_CleanupOutsideTheLockAndSocketRemoved(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	t.Run("EvictInstance", func(t *testing.T) {
		im := NewInstanceManager("127.0.0.1:1")
		p := &teardownProbe{}
		inst := placeInstance(t, im, "a", domain.ModeJIT, p)
		im.EvictInstance(inst.ID)
		im.EvictInstance(inst.ID) // idempotent
		assertTornDown(t, im, p, 1, inst)
	})
	t.Run("ReleaseInstance", func(t *testing.T) {
		im := NewInstanceManager("127.0.0.1:1")
		p := &teardownProbe{}
		daemon := placeInstance(t, im, "d", domain.ModeDaemon, &teardownProbe{})
		im.ReleaseInstance(daemon.ID) // only a JIT instance is released
		if _, ok := im.GetInstanceByID(daemon.ID); !ok {
			t.Fatal("ReleaseInstance evicted a daemon instance")
		}
		im.EvictInstance(daemon.ID)
		inst := placeInstance(t, im, "a", domain.ModeJIT, p)
		im.ReleaseInstance(inst.ID)
		assertTornDown(t, im, p, 1, inst)
	})
	t.Run("EvictAgent", func(t *testing.T) {
		im := NewInstanceManager("127.0.0.1:1")
		p := &teardownProbe{}
		i1 := placeInstance(t, im, "a", domain.ModeDaemon, p)
		i2 := placeInstance(t, im, "a", domain.ModeJIT, p)
		im.EvictAgent("a")
		assertTornDown(t, im, p, 2, i1, i2)
	})
	t.Run("KillAllAgents", func(t *testing.T) {
		im := NewInstanceManager("127.0.0.1:1")
		p := &teardownProbe{}
		i1 := placeInstance(t, im, "a", domain.ModeJIT, p)
		i2 := placeInstance(t, im, "b", domain.ModeDaemon, p)
		if err := im.KillAllAgents(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertTornDown(t, im, p, 2, i1, i2)
	})
}

// The unsandboxed launch's cap cleanup also runs when the process exits on
// its own (an exited instance that is never evicted kept its Job Object
// handle), and exactly once: a later eviction does not run it again.
func TestCapCleanupOnExit_RunsOnceAfterTheProcessExits(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := NewInstanceManager("127.0.0.1:1")
	p := &teardownProbe{}
	inst := placeInstance(t, im, "a", domain.ModeJIT, p)

	cmd := exec.Command(os.Args[0], "-test.run=^$") // exits at once
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	im.mu.Lock()
	im.cmds[inst.ID] = cmd
	im.mu.Unlock()
	pe := im.watchExitThen(inst.ID, cmd, im.capCleanupOnExit(inst.ID))
	select {
	case <-pe.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the child did not exit")
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("cap cleanup ran %d times at exit, want 1", got)
	}
	im.EvictInstance(inst.ID)
	assertTornDown(t, im, p, 1, inst)
}

// Through the real unsandboxed launch (a Job Object per agent on Windows):
// after each teardown path, and after the process exits on its own, no cap
// cleanup is left registered and the socket file is gone.
func TestTeardown_RealLaunchLeavesNoCapHandleOrSocket(t *testing.T) {
	skipShort(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	ctx := context.Background()
	for name, teardown := range map[string]func(im *InstanceManager, inst *domain.Instance){
		"EvictInstance":   func(im *InstanceManager, inst *domain.Instance) { im.EvictInstance(inst.ID) },
		"ReleaseInstance": func(im *InstanceManager, inst *domain.Instance) { im.ReleaseInstance(inst.ID) },
		"EvictAgent":      func(im *InstanceManager, inst *domain.Instance) { im.EvictAgent(inst.AgentID) },
		"KillAllAgents": func(im *InstanceManager, inst *domain.Instance) {
			if err := im.KillAllAgents(ctx); err != nil {
				t.Error(err)
			}
		},
		"ProcessExit": func(im *InstanceManager, inst *domain.Instance) { _ = im.GetCmd(inst.ID).Process.Kill() },
	} {
		t.Run(name, func(t *testing.T) {
			im := tokenProbeManager(t)
			inst, err := im.GetOrBootInstance(ctx, tokenProbeAgent("token_probe"), "")
			if err != nil {
				t.Fatal(err)
			}
			_ = readTokenReport(t, inst)
			im.mu.Lock()
			pe := im.exits[inst.ID]
			_, hasCap := im.capCleanups[inst.ID]
			im.mu.Unlock()
			if runtime.GOOS == "windows" && !hasCap {
				t.Fatal("the unsandboxed Windows launch registered no Job Object cleanup")
			}
			teardown(im, inst)
			select {
			case <-pe.done:
			case <-time.After(10 * time.Second):
				t.Fatal("the agent was not reaped")
			}
			im.mu.Lock()
			left := len(im.capCleanups)
			im.mu.Unlock()
			if left != 0 {
				t.Fatalf("%d cap cleanups left registered", left)
			}
			if name != "ProcessExit" {
				if _, err := os.Stat(inst.SocketPath); !os.IsNotExist(err) {
					t.Fatalf("socket %s not removed: %v", inst.SocketPath, err)
				}
			}
		})
	}
}
