package agentmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Graceful agent shutdown (ENG-2095, ADR-0157 D5.1).
//
// An instance's out/ is collected into the run ledger by its process's one
// Wait goroutine, after the exit (agent_sandbox.go cleanup). A teardown only
// KILLS the tree, so a shutdown that returned after the kills let the kernel
// close the ledger pool while those collections were still running, and the
// outputs of every agent that was still alive were lost. The order is now:
//
//  1. StopAdmitting — no new instance is booted or registered;
//  2. StopAllDaemons (AgentManager.Shutdown) — daemons stop as expected exits;
//  3. KillAllAgents — every instance is detached and killed, then the kernel
//     waits, bounded by execution.agents.shutdown_timeout_ms, until every
//     started child's cleanup (out/ collection included) has finished;
//  4. the caller closes the ledger (Kernel.Shutdown closes the store after
//     MetabolismStack.Shutdown returns).
//
// On timeout the instances whose cleanup had not finished are logged by agent
// and instance — their outputs are dropped; the next boot sweeps their scratch
// (ENG-2088) — and KillAllAgents returns an error naming them, so the kernel
// still exits.

// ErrShuttingDown refuses a boot that starts after shutdown began.
var ErrShuttingDown = errors.New("the kernel is shutting down; no new agent instance is started")

// defaultShutdownTimeout bounds the wait when no
// execution.agents.shutdown_timeout_ms is configured.
const defaultShutdownTimeout = 10 * time.Second

// SetShutdownTimeout sets how long KillAllAgents waits for the instances'
// cleanups (out/ collection) before it gives up on them
// (execution.agents.shutdown_timeout_ms). <= 0 keeps defaultShutdownTimeout.
func (im *InstanceManager) SetShutdownTimeout(d time.Duration) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.shutdownTimeout = d
}

// ShutdownTimeout is the effective bound KillAllAgents applies.
func (im *InstanceManager) ShutdownTimeout() time.Duration {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.shutdownTimeoutLocked()
}

func (im *InstanceManager) shutdownTimeoutLocked() time.Duration {
	if im.shutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return im.shutdownTimeout
}

// StopAdmitting refuses every later boot with ErrShuttingDown; a boot already
// in flight is refused when it would register its instance, and its child is
// killed. One-way: a shut-down manager is not reopened.
func (im *InstanceManager) StopAdmitting() {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.closing = true
}

// admitting is the boot paths' first check.
func (im *InstanceManager) admitting() error {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.closing {
		return ErrShuttingDown
	}
	return nil
}

// abandonBoot ends a child whose boot finished after shutdown began: it is
// never registered, so no teardown path would reach it. Its Wait goroutine
// runs the cleanup (out/ collection, sandbox release) as for any exit.
func (im *InstanceManager) abandonBoot(instanceID string, cmd *exec.Cmd) {
	im.forgetExit(instanceID)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if c := im.takeCapCleanup(instanceID); c != nil {
		c()
	}
}

// KillAllAgents detaches and kills every instance, kills any child still
// booting, and waits until every started child's cleanup — a sandboxed
// instance's out/ collection into the run ledger included — has finished, so
// the caller may close the ledger afterwards (ENG-2095). The whole wait is
// bounded by the shutdown timeout (SetShutdownTimeout) and by ctx; on timeout
// it logs every instance whose outputs were dropped and returns an error
// naming them. The maps are cleared under the lock; the kills run after it is
// released (ENG-1963).
func (im *InstanceManager) KillAllAgents(ctx context.Context) error {
	im.mu.Lock()
	timeout := im.shutdownTimeoutLocked()
	teardowns := make([]func(), 0, len(im.instances))
	for id := range im.instances {
		teardowns = append(teardowns, im.detachLocked(id))
	}
	// Every started child not yet cleaned up: the instances just detached, the
	// ones an earlier teardown (StopAllDaemons, an eviction) left draining, and
	// any still booting.
	children := make(map[string]*processExit, len(im.children))
	for id, pe := range im.children {
		children[id] = pe
	}
	im.mu.Unlock()
	if len(teardowns) == 0 && len(children) == 0 {
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, td := range teardowns {
		wg.Add(1)
		go func(td func()) {
			defer wg.Done()
			td()
		}(td)
	}
	// A child that is not (or no longer) an instance is killed here: a booting
	// one would otherwise run until its boot timeout. Killing an exited child
	// is a no-op (os.ErrProcessDone).
	for _, pe := range children {
		select {
		case <-pe.done:
		default:
			if pe.cmd != nil && pe.cmd.Process != nil {
				_ = pe.cmd.Process.Kill()
			}
		}
	}
	teardownsDone := make(chan struct{})
	go func() { wg.Wait(); close(teardownsDone) }()

	timedOut := false
	select {
	case <-teardownsDone:
	case <-waitCtx.Done():
		timedOut = true
	}
	var dropped []string
	for id, pe := range children {
		if !timedOut {
			select {
			case <-pe.done:
				continue
			case <-waitCtx.Done():
				timedOut = true
			}
		}
		select {
		case <-pe.done:
		default:
			dropped = append(dropped, id)
			slog.Warn("ENG-2095: agent shutdown timed out before this instance's cleanup finished; its out/ is not collected into the run ledger (the next boot sweeps its scratch)",
				"agent", pe.agentID, "instance", id, "timeout", timeout)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		return fmt.Errorf("agent shutdown timed out after %s; out/ not collected for instance(s) %s: %w",
			timeout, strings.Join(dropped, ", "), waitCtx.Err())
	}
	if timedOut {
		return fmt.Errorf("agent teardown timed out after %s: %w", timeout, waitCtx.Err())
	}
	return nil
}

// Shutdown is the graceful agent shutdown, in order: refuse new boots, stop the
// daemons as expected exits (ADR-0033: a bare kill would read as a crash and
// respawn them), then kill every instance and wait — bounded — for each one's
// out/ to be collected (KillAllAgents). The ledger must stay open until it
// returns (MetabolismStack.Shutdown, then Kernel.Shutdown closes the store).
func (m *AgentManager) Shutdown(ctx context.Context) error {
	m.StopHeartbeatWatch() // ADR-0070 D3: no liveness kill races the shutdown
	m.StopAdmitting()
	m.StopAllDaemons()
	return m.KillAllAgents(ctx)
}
