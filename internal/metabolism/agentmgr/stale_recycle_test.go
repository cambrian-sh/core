package agentmgr

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// trackInstance registers a fake instance of agentID (no process).
func trackInstance(t *testing.T, m *AgentManager, agentID string, mode domain.InstanceMode) *domain.Instance {
	t.Helper()
	inst := domain.NewInstance(agentID)
	inst.Mode = mode
	inst.SocketPath = t.TempDir() + "/absent.sock"
	im := m.InstanceManager
	im.mu.Lock()
	im.instances[inst.ID] = inst
	im.agentIndex[agentID] = append(im.agentIndex[agentID], inst.ID)
	im.mu.Unlock()
	return inst
}

// ADR-0157 D7.10.2: a stale instance busy with an Execute is not killed; it is
// recycled after its last Execute ends, and from the moment it is chosen it
// admits nothing new and is no longer handed out.
func TestStaleInstanceRecycledAfterItsExecute(t *testing.T) {
	m := newTestManager()
	inst := trackInstance(t, m, "analyst", domain.ModeJIT)

	if !m.ExecuteBegan(inst.ID) || !m.ExecuteBegan(inst.ID) {
		t.Fatal("a live instance admits Executes")
	}
	m.MarkAgentStale("analyst")
	if _, ok := m.GetInstanceByID(inst.ID); !ok {
		t.Fatal("a busy stale instance must not be killed mid-Execute")
	}
	if m.ExecuteEnded(inst.ID) {
		t.Fatal("still one Execute in flight: not recycled yet")
	}
	if _, ok := m.GetInstanceByID(inst.ID); !ok {
		t.Fatal("recycled while an Execute was still running")
	}
	if !m.ExecuteEnded(inst.ID) {
		t.Fatal("the last Execute on a stale instance must recycle it (ExecuteEnded true)")
	}
	if _, ok := m.GetInstanceByID(inst.ID); ok {
		t.Fatal("a recycled JIT instance must be evicted (the next call boots on the new layer)")
	}
	if m.ExecuteBegan(inst.ID) {
		t.Fatal("an evicted instance admits nothing")
	}
}

// An idle stale instance is recycled at once; an instance that is not stale is
// never recycled by an Execute ending.
func TestIdleStaleInstanceRecycledAtOnce(t *testing.T) {
	m := newTestManager()
	idle := trackInstance(t, m, "analyst", domain.ModeJIT)
	other := trackInstance(t, m, "reporter", domain.ModeJIT)

	if !m.ExecuteBegan(other.ID) || m.ExecuteEnded(other.ID) {
		t.Fatal("an Execute on a current instance recycles nothing")
	}
	m.MarkAgentStale("analyst")
	if _, ok := m.GetInstanceByID(idle.ID); ok {
		t.Fatal("an idle stale instance must be recycled when marked")
	}
	if _, ok := m.GetInstanceByID(other.ID); !ok {
		t.Fatal("another agent's instance must be untouched")
	}
	m.MarkAgentStale("nobody") // no instances: nothing happens
}

// A retiring instance (stale, its last Execute just ended, teardown pending)
// is skipped by the lookups the dispatch paths use.
func TestRetiringInstanceIsNotHandedOut(t *testing.T) {
	m := newTestManager()
	inst := trackInstance(t, m, "analyst", domain.ModeJIT)
	im := m.InstanceManager
	im.mu.Lock()
	im.recycle.init()
	im.recycle.retiring[inst.ID] = true
	im.mu.Unlock()
	if _, ok := m.GetInstance("analyst"); ok {
		t.Fatal("GetInstance must skip a retiring instance")
	}
	if m.ExecuteBegan(inst.ID) {
		t.Fatal("a retiring instance admits no new Execute")
	}
}

// A stale daemon (a chat-pool worker) is recycled when idle onto the same
// stream with the same references, and the next CallDaemon waits for the new
// process instead of reading the worker as lost.
func TestStaleDaemonRecycledOntoTheSameStream(t *testing.T) {
	m := newTestManager()
	var boots atomic.Int32
	gate := make(chan struct{})
	m.DaemonBootHook = func(agentID, streamID string, _ map[string]any) (string, *exec.Cmd, error) {
		n := boots.Add(1)
		if n > 1 {
			<-gate // the recycle's boot waits until the test lets it go
		}
		inst := domain.NewInstance(agentID)
		inst.ID = fmt.Sprintf("worker-%d", n)
		inst.Mode = domain.ModeDaemon
		inst.SocketPath = t.TempDir() + "/absent.sock"
		im := m.InstanceManager
		im.mu.Lock()
		im.instances[inst.ID] = inst
		im.agentIndex[agentID] = append(im.agentIndex[agentID], inst.ID)
		im.mu.Unlock()
		return inst.ID, nil, nil
	}
	ctx := context.Background()
	if _, err := m.SpawnDaemon(ctx, "chat", "pool-0", map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	m.IncrementDaemonRef("pool-0", "")
	if got := m.DaemonRefCount("pool-0"); got != 2 {
		t.Fatalf("refs: %d", got)
	}
	if !m.ExecuteBegan("worker-1") {
		t.Fatal("begin")
	}
	m.MarkAgentStale("chat")
	if id, _ := m.DaemonInstanceID("pool-0"); id != "worker-1" {
		t.Fatalf("a busy daemon keeps serving its turn: %q", id)
	}
	m.ExecuteEnded("worker-1") // the turn ended: recycle starts

	// A turn arriving now waits for the replacement.
	done := make(chan error, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := m.CallDaemon(cctx, "pool-0", &domain.Handoff{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("CallDaemon must wait for the recycle's boot, returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	err := <-done
	// The fake socket does not exist, so the call fails at dial — but on the NEW
	// worker, not with "no daemon registered".
	if err == nil || containsAny(err.Error(), "no daemon registered", "not live", "being recycled") {
		t.Fatalf("the turn must reach the replacement worker: %v", err)
	}
	if id, _ := m.DaemonInstanceID("pool-0"); id != "worker-2" {
		t.Fatalf("the stream must be served by the replacement: %q", id)
	}
	if _, ok := m.GetInstanceByID("worker-1"); ok {
		t.Fatal("the stale worker must be evicted")
	}
	if got := m.DaemonRefCount("pool-0"); got != 2 {
		t.Fatalf("the recycle keeps the stream's references: %d", got)
	}
	m.StopAllDaemons()
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
