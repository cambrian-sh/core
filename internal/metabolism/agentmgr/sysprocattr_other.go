//go:build !windows

package agentmgr

import "os/exec"

// configureSysProcAttr is a no-op on non-Windows platforms. The only attribute
// the kernel set on Windows was HideWindow (a Windows-only field), so there is
// nothing to configure on Linux/macOS — the agent child process inherits the
// default attributes and its stdio is piped by the caller. Kept as a build-tagged
// counterpart so call sites stay platform-agnostic (see sysprocattr_windows.go).
func configureSysProcAttr(_ *exec.Cmd) {}
