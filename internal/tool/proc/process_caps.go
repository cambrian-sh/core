package proc

import "os/exec"

// applyResourceCaps applies OS resource limits to a tool child process. The hard
// timeout (CommandContext) is the cross-platform blast-radius control; tighter
// caps (rlimit / cgroup memory + nproc, process-group kill of grandchildren) are
// a Unix follow-up applied here behind a build tag. No-op by default so the
// handler builds and runs everywhere.
func applyResourceCaps(_ *exec.Cmd) {}
