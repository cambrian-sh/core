//go:build !windows

package app

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetach detaches the child into its own session so it survives the
// setup process (and its terminal) exiting.
func applyDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// pidAlive reports whether pid is a live process (signal 0 probe).
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// terminateProcess asks the kernel to shut down gracefully (SIGTERM — the
// kernel drains: health flips NOT_SERVING before GracefulStop, ADR-0065).
func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// killProcess is the SIGKILL escalation after a graceful stop times out.
func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
