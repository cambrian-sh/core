package agentmgr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2095: a graceful shutdown returns only after every killed instance's
// cleanup (its out/ collection into the run ledger) has run, bounded by the
// shutdown timeout; new boots are refused once it began.

// startLingeringChild starts this test binary in its "sleep" shape (TestMain):
// alive until killed.
func startLingeringChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), chaosSleepEnv)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// placeLiveInstance registers a running child as a tracked instance whose exit
// cleanup is after (the stand-in for the sandbox cleanup that collects out/).
func placeLiveInstance(t *testing.T, im *InstanceManager, agentID string, after func()) (*domain.Instance, *processExit) {
	t.Helper()
	cmd := startLingeringChild(t)
	inst := domain.NewInstance(agentID)
	pe := im.watchExitThen(agentID, inst.ID, cmd, after)
	im.mu.Lock()
	im.cmds[inst.ID] = cmd
	im.instances[inst.ID] = inst
	im.agentIndex[agentID] = append(im.agentIndex[agentID], inst.ID)
	im.mu.Unlock()
	return inst, pe
}

func childrenLeft(im *InstanceManager) int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return len(im.children)
}

// The ordering: KillAllAgents does not return before the instance's cleanup
// (out/ collection) finished — so the caller may close the ledger after it.
func TestKillAllAgents_WaitsForEachInstancesCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := NewInstanceManager("127.0.0.1:1")
	var collected atomic.Int32
	slowCollect := func() {
		time.Sleep(300 * time.Millisecond) // a collection still writing when the kill lands
		collected.Add(1)
	}
	placeLiveInstance(t, im, "a", slowCollect)
	placeLiveInstance(t, im, "b", slowCollect)

	if err := im.KillAllAgents(context.Background()); err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if n := collected.Load(); n != 2 {
		t.Fatalf("KillAllAgents returned with %d of 2 cleanups finished; the ledger would close under them", n)
	}
	if n := childrenLeft(im); n != 0 {
		t.Fatalf("%d children still tracked after the wait", n)
	}
}

// A child still booting (started, not yet an instance) is killed and waited
// for too; it does not run until its boot timeout.
func TestKillAllAgents_KillsAndWaitsForABootingChild(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := NewInstanceManager("127.0.0.1:1")
	var cleaned atomic.Bool
	pe := im.watchExitThen("booting", "inst-booting", startLingeringChild(t), func() { cleaned.Store(true) })

	start := time.Now()
	if err := im.KillAllAgents(context.Background()); err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	select {
	case <-pe.done:
	default:
		t.Fatal("KillAllAgents returned before the booting child exited")
	}
	if !cleaned.Load() {
		t.Fatal("the booting child's cleanup did not run before KillAllAgents returned")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("KillAllAgents took %s for one killed child", d)
	}
}

// A hung cleanup hits the configured bound: KillAllAgents returns (the kernel
// still exits) with an error naming the instance whose outputs were dropped.
func TestKillAllAgents_HungCleanupHitsTheTimeoutAndIsNamed(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := NewInstanceManager("127.0.0.1:1")
	im.SetShutdownTimeout(300 * time.Millisecond)
	if got := im.ShutdownTimeout(); got != 300*time.Millisecond {
		t.Fatalf("ShutdownTimeout = %s", got)
	}
	release := make(chan struct{})
	hung, hungExit := placeLiveInstance(t, im, "hung", func() { <-release })
	var okDone atomic.Bool
	fine, _ := placeLiveInstance(t, im, "fine", func() { okDone.Store(true) })

	start := time.Now()
	err := im.KillAllAgents(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("KillAllAgents reported success with a cleanup still hung")
	}
	if !strings.Contains(err.Error(), hung.ID) {
		t.Fatalf("the error does not name the hung instance %s: %v", hung.ID, err)
	}
	if strings.Contains(err.Error(), fine.ID) {
		t.Fatalf("the error names the instance whose cleanup finished: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !okDone.Load() {
		t.Fatal("the healthy instance's cleanup did not finish within the bound")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("KillAllAgents waited %s past a 300ms bound", elapsed)
	}
	close(release)
	<-hungExit.done
}

// Once shutdown began no instance is booted: the boot paths refuse with
// ErrShuttingDown before anything is spawned.
func TestShutdown_RefusesNewBoots(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := newTestManager()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown of an idle manager: %v", err)
	}
	before := m.BootCount()
	if _, err := m.bootAgentInstance(context.Background(), noSocketAgent(), true); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("boot after shutdown: err = %v, want ErrShuttingDown", err)
	}
	if _, err := m.bootDaemonAgent(context.Background(), noSocketAgent(), "s", nil); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("daemon boot after shutdown: err = %v, want ErrShuttingDown", err)
	}
	if m.BootCount() != before {
		t.Fatal("a refused boot counted as a spawn")
	}
}

// A boot that was already past the admission check when shutdown began is
// refused at registration and its child is ended, never left untracked.
func TestAbandonBoot_KillsTheChildAndRunsItsCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := NewInstanceManager("127.0.0.1:1")
	cmd := startLingeringChild(t)
	var capRan, cleaned atomic.Bool
	im.mu.Lock()
	im.capCleanups["late"] = func() { capRan.Store(true) }
	im.mu.Unlock()
	pe := im.watchExitThen("late-agent", "late", cmd, func() { cleaned.Store(true) })
	im.StopAdmitting()
	im.abandonBoot("late", cmd)
	select {
	case <-pe.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned child did not exit")
	}
	if !capRan.Load() || !cleaned.Load() {
		t.Fatalf("cap cleanup ran %v, exit cleanup ran %v; want both", capRan.Load(), cleaned.Load())
	}
	im.mu.Lock()
	_, stillExit := im.exits["late"]
	im.mu.Unlock()
	if stillExit {
		t.Fatal("the abandoned boot's exit stayed registered")
	}
}
