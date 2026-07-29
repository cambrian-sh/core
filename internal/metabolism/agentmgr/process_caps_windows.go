//go:build windows

package agentmgr

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyResourceCaps places a spawned agent in a Windows Job Object.
//
// The job always carries JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, which is LIFETIME
// CONTAINMENT rather than resource capping: when the job handle closes the whole
// process tree dies, including grandchildren a bare Process.Kill cannot reach.
// Windows closes a dead process's handles, so this holds even when the kernel is
// killed outright — the case where cleanup code never runs and orphans used to
// survive. An orphaned daemon is not merely untidy: a stranded Telegram poller
// holds the bot token and answers the kernel's replacement with 409 forever.
//
// JOB_OBJECT_LIMIT_PROCESS_MEMORY is added ONLY when a cap is configured. The two
// were previously conflated behind one memLimitMB check, so a deployment that had
// not opted into memory caps — the default — silently had no containment either.
// Lifetime and resource limits are separate concerns and are now gated separately.
//
// The caller keeps the returned cleanup and invokes it on eviction.
func applyResourceCaps(cmd *exec.Cmd, memLimitMB int) (func(), error) {
	if cmd.Process == nil {
		return nil, nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// Containment is unconditional; the memory cap is opt-in.
	flags := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	var memLimit uintptr
	if memLimitMB > 0 {
		flags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		memLimit = uintptr(memLimitMB) * 1024 * 1024
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: flags,
		},
		ProcessMemoryLimit: memLimit,
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("OpenProcess(%d): %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	// Keep the job handle open for the process lifetime; closing it enforces
	// kill-on-close and reaps the whole tree.
	return func() { _ = windows.CloseHandle(job) }, nil
}
