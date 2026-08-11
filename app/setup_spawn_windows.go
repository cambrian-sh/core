//go:build windows

package app

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	winDetachedProcess       = 0x00000008
	winCreateNewProcessGroup = 0x00000200
)

// applyDetach detaches the child from the console so it survives the setup
// process (and its terminal) exiting.
func applyDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winDetachedProcess | winCreateNewProcessGroup,
		HideWindow:    true,
	}
}

// pidAlive reports whether pid is a live process. On Windows, FindProcess
// actually opens a handle, so an error means the process is gone.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// terminateProcess stops the kernel. Windows has no SIGTERM for unrelated
// processes — TerminateProcess is the stop mechanism, so the drain path
// (ADR-0065) is skipped here; the REACT-01 journal makes a hard stop safe.
func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	return p.Kill()
}

func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		defer p.Release()
		_ = p.Kill()
	}
}
