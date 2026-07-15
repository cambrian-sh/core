//go:build !windows && !linux

package agentmgr

import "os/exec"

// applyResourceCaps is a no-op on platforms without a portable per-process memory
// cap wired here (e.g. macOS/BSD lack prlimit; setrlimit affects the parent).
// The hard boot/exec lifecycle and process kill remain the blast-radius control;
// a cgroup/Job-Object equivalent is a per-platform follow-up (SEC-01). Kept as a
// build-tagged counterpart so call sites stay platform-agnostic.
func applyResourceCaps(_ *exec.Cmd, _ int) (func(), error) { return nil, nil }
