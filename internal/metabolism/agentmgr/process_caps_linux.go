//go:build linux

package agentmgr

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// applyResourceCaps caps a spawned agent's address space via RLIMIT_AS (SEC-01).
// A process exceeding the cap fails its next allocation (mmap/brk returns
// ENOMEM) and typically aborts, containing an OOM to the agent rather than the
// kernel host. Prlimit is applied post-start on the child's PID; the brief
// window before the child allocates is acceptable. memLimitMB <= 0 disables.
func applyResourceCaps(cmd *exec.Cmd, memLimitMB int) (func(), error) {
	if memLimitMB <= 0 || cmd.Process == nil {
		return nil, nil
	}
	lim := uint64(memLimitMB) * 1024 * 1024
	rl := &unix.Rlimit{Cur: lim, Max: lim}
	if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, rl, nil); err != nil {
		return nil, fmt.Errorf("prlimit RLIMIT_AS: %w", err)
	}
	return nil, nil
}
