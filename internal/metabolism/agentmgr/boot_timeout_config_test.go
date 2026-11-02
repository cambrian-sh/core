package agentmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: the configured boot
// timeout (execution.agents.agent_boot_timeout_ms) bounds the socket wait, not
// the built-in default, and GetOrBootInstance returns the boot's cause instead
// of a bare nil.
func TestAgentBootTimeout_ConfiguredValueBoundsTheWait(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	shortBootTimeout(t, time.Hour) // the built-in default must not be what ends the wait
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetEnvExtras([]string{chaosSleepEnv})
	im.SetAgentBootTimeout(700 * time.Millisecond)
	def := noSocketAgent()

	start := time.Now()
	inst, err := im.GetOrBootInstance(context.Background(), def, "")
	elapsed := time.Since(start)
	if inst != nil || err == nil {
		t.Fatalf("boot must fail: inst=%v err=%v", inst, err)
	}
	if !strings.Contains(err.Error(), "did not appear within 700ms") || !strings.Contains(err.Error(), "boot agent "+def.ID) {
		t.Fatalf("error lacks the configured timeout or the agent: %v", err)
	}
	if elapsed < 700*time.Millisecond || elapsed > 20*time.Second {
		t.Fatalf("wait was %s, want about 700ms", elapsed)
	}
	assertNothingRegistered(t, im, def)
}

// A tracked instance whose process has exited is evicted and a new boot is
// attempted, instead of the dead instance being handed back forever.
func TestGetOrBootInstance_ReplacesAnExitedInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	im := NewInstanceManager("", "127.0.0.1:1")
	def := noSocketAgent() // a fresh boot of it exits at once: the attempt is observable
	dead := domain.NewInstance(def.ID)
	exited := &processExit{done: make(chan struct{})}
	close(exited.done)
	im.mu.Lock()
	im.instances[dead.ID] = dead
	im.agentIndex[def.ID] = []string{dead.ID}
	im.exits[dead.ID] = exited
	im.mu.Unlock()

	inst, err := im.GetOrBootInstance(context.Background(), def, "")
	if inst == dead {
		t.Fatal("the exited instance was returned")
	}
	if err == nil || im.BootCount() != 1 {
		t.Fatalf("a new boot must be attempted: err=%v boots=%d", err, im.BootCount())
	}
	im.mu.Lock()
	_, still := im.instances[dead.ID]
	im.mu.Unlock()
	if still {
		t.Fatal("the exited instance is still tracked")
	}

	// A live instance (no exit) is returned as before, without a boot.
	live := domain.NewInstance(def.ID)
	im.mu.Lock()
	im.instances[live.ID] = live
	im.agentIndex[def.ID] = []string{live.ID}
	im.exits[live.ID] = &processExit{done: make(chan struct{})}
	im.mu.Unlock()
	if got, err := im.GetOrBootInstance(context.Background(), def, ""); got != live || err != nil || im.BootCount() != 1 {
		t.Fatalf("live instance: got=%v err=%v boots=%d", got, err, im.BootCount())
	}
}
