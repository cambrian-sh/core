package agentmgr

import (
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// A respawn that FAILS is another restart attempt: it backs off and, past the
// flap limit, lands in the operator-visible quarantined state with a
// DaemonQuarantinedEvent (ADR-0070 D2). Before the fix the first failed respawn
// logged a WARN and returned, leaving the stream "unavailable" forever
// (T-20261113-daemon-auto-restart-a-failed-respawn-leaves-the).
func TestCrashDetection_FailedRespawnBacksOffThenQuarantines(t *testing.T) {
	m := newTestManager()
	bus := newCountingBus()
	m.EventBus = bus
	m.RestartPolicy = NewDaemonRestartPolicy(3, time.Minute, time.Millisecond, 2*time.Millisecond)
	var boots atomic.Int32
	m.DaemonBootHook = func(string, string, map[string]any) (string, *exec.Cmd, error) {
		boots.Add(1)
		return "", nil, errors.New("boot timeout")
	}
	defer m.StopAllDaemons()

	m.handleDaemonExit(&domain.Instance{AgentID: "tg", ID: "inst-1"}, "stream-tg", true)

	waitUntil(t, "quarantine after repeated failed respawns",
		func() bool { return bus.count(domain.EventTypeDaemonQuarantined) == 1 })
	if got := m.GetDaemonStatus("stream-tg"); got != "quarantined" {
		t.Fatalf("status = %q, want quarantined", got)
	}
	if got := boots.Load(); got != 3 {
		t.Fatalf("respawn attempts = %d, want 3 (the flap limit)", got)
	}
}

// StopAllDaemons cancels a pending restart: no daemon is respawned after
// shutdown began (AGENTS.md §3.3, one owner per background goroutine).
func TestStopAllDaemons_CancelsPendingRestart(t *testing.T) {
	m := newTestManager()
	m.RestartPolicy = NewDaemonRestartPolicy(5, time.Minute, 50*time.Millisecond, 50*time.Millisecond)
	m.RestartPolicy.jitter = func(max time.Duration) time.Duration { return max } // a certain 50 ms delay
	var boots atomic.Int32
	m.DaemonBootHook = func(string, string, map[string]any) (string, *exec.Cmd, error) {
		boots.Add(1)
		return "inst-2", nil, nil
	}

	m.handleDaemonExit(&domain.Instance{AgentID: "tg", ID: "inst-1"}, "stream-tg", true)

	done := make(chan struct{})
	go func() { m.StopAllDaemons(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopAllDaemons did not cancel the pending restart")
	}
	time.Sleep(150 * time.Millisecond) // well past the restart delay
	if got := boots.Load(); got != 0 {
		t.Fatalf("daemon respawned after shutdown began (%d boots)", got)
	}
	// A crash observed after shutdown schedules nothing.
	m.handleDaemonExit(&domain.Instance{AgentID: "tg", ID: "inst-3"}, "stream-tg", true)
	time.Sleep(20 * time.Millisecond)
	if got := boots.Load(); got != 0 {
		t.Fatalf("restart scheduled after shutdown (%d boots)", got)
	}
}
