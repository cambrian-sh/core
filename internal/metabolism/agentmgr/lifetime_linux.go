//go:build linux

package agentmgr

import (
	"os/exec"
	"syscall"
)

// containLifetime ties a spawned agent's lifetime to the kernel's, BEFORE start.
//
// PDEATHSIG asks the OS to signal the child when its parent dies — including when the
// parent is killed outright, which is exactly the case where the kernel's own cleanup
// never runs. Without it a hard kill strands every spawned agent, and a stranded daemon
// is not merely untidy: a Telegram poller that outlives its kernel keeps the bot token
// and answers the replacement with 409 indefinitely.
//
// It must be set before Start: SysProcAttr is read when the process is created.
func containLifetime(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
