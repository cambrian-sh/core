//go:build !linux

package agentmgr

import "os/exec"

// containLifetime is a no-op off Linux.
//
// On Windows the equivalent guarantee is the Job Object created in applyResourceCaps,
// which carries JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE — the OS reaps the tree when the
// kernel's handles close, including on a hard kill. That is applied AFTER start, so
// there is nothing to do here.
//
// Other platforms currently have no containment guarantee, which is stated plainly
// rather than implied by an empty function.
func containLifetime(_ *exec.Cmd) {}
