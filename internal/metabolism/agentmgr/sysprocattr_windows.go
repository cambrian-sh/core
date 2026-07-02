//go:build windows

package agentmgr

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr applies Windows-specific child-process attributes when the
// kernel spawns an agent. HideWindow is left false (the historical behavior): the
// child is a console subprocess whose stdio is piped, so no window is shown anyway.
// The Windows-only SysProcAttr.HideWindow field lives here behind a build tag so
// the package still compiles on Linux/macOS (see sysprocattr_other.go).
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
}
