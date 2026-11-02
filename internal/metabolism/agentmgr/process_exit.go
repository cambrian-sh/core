package agentmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// agentBootTimeout bounds how long a boot waits for the child's UDS socket when
// no execution.agents.agent_boot_timeout_ms is configured
// (InstanceManager.SetAgentBootTimeout). One value for both boot paths (it was a
// const duplicated in bootAgent and bootDaemonAgent, free to drift); a var so a
// test can shorten it.
var agentBootTimeout = 10 * time.Second

// processExit is the ONE Wait a child process gets. done closes when the child
// has exited and err holds Wait's result; a closed channel can be received from
// any number of times, so the boot path and the ADR-0033 crash watcher can both
// observe the same exit without racing for cmd.Wait (which may only be called
// once per process).
type processExit struct {
	done chan struct{}
	err  error
}

// watchExit starts the child's Wait goroutine and registers it under instanceID.
func (im *InstanceManager) watchExit(instanceID string, cmd *exec.Cmd) *processExit {
	pe := &processExit{done: make(chan struct{})}
	im.mu.Lock()
	im.exits[instanceID] = pe
	im.mu.Unlock()
	go func() {
		pe.err = cmd.Wait()
		close(pe.done)
	}()
	return pe
}

// forgetExit drops the registration after a failed boot (the goroutine itself
// ends when the killed child is reaped).
func (im *InstanceManager) forgetExit(instanceID string) {
	im.mu.Lock()
	delete(im.exits, instanceID)
	im.mu.Unlock()
}

// awaitExit returns a channel that closes when the child registered under
// instanceID has exited. An instance that was booted without watchExit (a test
// DaemonBootHook that only placed a cmd) gets a watcher started now, which is
// the pre-2026-11-01 behaviour of the crash watcher calling Wait itself.
func (im *InstanceManager) awaitExit(instanceID string, cmd *exec.Cmd) <-chan struct{} {
	im.mu.Lock()
	pe, ok := im.exits[instanceID]
	im.mu.Unlock()
	if ok {
		return pe.done
	}
	return im.watchExit(instanceID, cmd).done
}

// awaitSocket waits for the agent's UDS socket to appear, giving up on timeout,
// on ctx, or on the child exiting first. timeout <= 0 means agentBootTimeout.
//
// A child that dies at import time (missing dependency, bad entrypoint, unknown
// flag) used to be reported only after the full timeout, as "socket did not
// appear", which hid the real cause — its stderr had gone to forwardPipe seconds
// earlier — and made every step routed to such an agent pay the whole timeout
// before dispatch fell back
// (T-20261031-bootagent-waits-the-full-10-s-boottimeout-even).
func awaitSocket(ctx context.Context, cmd *exec.Cmd, exit *processExit, socketPath, who string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = agentBootTimeout
	}
	const pollInterval = 50 * time.Millisecond
	exited := func() error {
		return fmt.Errorf("%s: process exited before opening socket %s: %s (its stderr was forwarded to the log)",
			who, socketPath, exitStatus(exit.err))
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, statErr := os.Stat(socketPath); statErr == nil {
			return nil
		}
		select {
		case <-exit.done:
			return exited()
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return fmt.Errorf("%s: socket %s did not appear within %s", who, socketPath, timeout)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		case <-exit.done:
			return exited()
		case <-time.After(pollInterval):
		}
	}
}

func exitStatus(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}
