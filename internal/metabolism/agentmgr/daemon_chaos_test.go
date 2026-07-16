package agentmgr

import (
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// TestHelperDaemonProcess is the child process a chaos-test "daemon" runs: it sleeps
// until the parent kills it. Guarded by an env var so it is inert during a normal run.
func TestHelperDaemonProcess(t *testing.T) {
	if os.Getenv("AGENTMGR_CHAOS_HELPER") != "1" {
		return
	}
	time.Sleep(10 * time.Minute) // live until Process.Kill()
}

// countingBus records the domain events published during the chaos run.
type countingBus struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingBus() *countingBus { return &countingBus{counts: map[string]int{}} }
func (b *countingBus) Publish(e domain.DomainEvent) error {
	b.mu.Lock()
	b.counts[e.EventType()]++
	b.mu.Unlock()
	return nil
}
func (b *countingBus) count(t string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[t]
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// LIVE chaos test (REACT-04 / ADR-0070): a real daemon subprocess is killed; the crash
// watcher must auto-restart it with backoff, and a crash-loop must land in quarantine —
// not an infinite restart spin. Exercises the real cmd.Wait → handleDaemonExit →
// RestartPolicy → SpawnDaemon loop with actual OS process kills.
func TestChaos_DaemonKillRestartQuarantine(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches real subprocesses; skipped in -short")
	}

	var mu sync.Mutex
	var procs []*exec.Cmd
	bootCount := 0

	m := NewAgentManager(nil, "", "", nil)
	bus := newCountingBus()
	m.EventBus = bus
	// Small, fast backoff; quarantine after 2 restarts in the window.
	m.RestartPolicy = NewDaemonRestartPolicy(2, time.Minute, 10*time.Millisecond, 40*time.Millisecond)
	m.DaemonBootHook = func(agentID, streamID string, _ map[string]any) (string, *exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemonProcess$")
		cmd.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=1")
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		mu.Lock()
		procs = append(procs, cmd)
		bootCount++
		id := bootCount
		mu.Unlock()
		return "inst-" + string(rune('0'+id)), cmd, nil
	}

	getBoots := func() int { mu.Lock(); defer mu.Unlock(); return bootCount }
	killNth := func(n int) { // kill the n-th (1-based) launched process
		mu.Lock()
		p := procs[n-1]
		mu.Unlock()
		_ = p.Process.Kill()
	}

	// Spawn: boot #1.
	if _, err := m.SpawnDaemon("chaos-daemon", "stream-1", nil); err != nil {
		t.Fatalf("SpawnDaemon: %v", err)
	}
	waitUntil(t, "initial boot", func() bool { return getBoots() == 1 })

	// Kill #1 → auto-restart → boot #2 + a recovery event.
	killNth(1)
	waitUntil(t, "restart after 1st kill", func() bool { return getBoots() == 2 })
	waitUntil(t, "recovery event #1", func() bool { return bus.count(domain.EventTypeDaemonRecovered) >= 1 })

	// Kill #2 → auto-restart → boot #3.
	killNth(2)
	waitUntil(t, "restart after 2nd kill", func() bool { return getBoots() == 3 })

	// Kill #3 → the flap limit (2 restarts in the window) is hit → QUARANTINE, no boot #4.
	killNth(3)
	waitUntil(t, "quarantine event", func() bool { return bus.count(domain.EventTypeDaemonQuarantined) >= 1 })
	waitUntil(t, "quarantined status", func() bool { return m.GetDaemonStatus("stream-1") == "quarantined" })

	// Give any (wrong) 4th restart a chance to happen, then assert it did NOT.
	time.Sleep(200 * time.Millisecond)
	if got := getBoots(); got != 3 {
		t.Fatalf("crash-loop must quarantine, not spin: expected 3 boots, got %d", got)
	}

	// Clean up any still-running child.
	mu.Lock()
	for _, p := range procs {
		if p.Process != nil {
			_ = p.Process.Kill()
		}
	}
	mu.Unlock()
}
