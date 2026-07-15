//go:build windows

package agentmgr

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyResourceCaps caps a spawned agent's memory via a Windows Job Object
// (SEC-01). The job carries JOB_OBJECT_LIMIT_PROCESS_MEMORY (a process that
// exceeds the cap is terminated by the OS) and JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// (closing the returned handle kills the whole tree — reaping grandchildren the
// bare Process.Kill cannot reach). The caller keeps the returned cleanup and
// invokes it on eviction. memLimitMB <= 0 disables (returns nil cleanup).
func applyResourceCaps(cmd *exec.Cmd, memLimitMB int) (func(), error) {
	if memLimitMB <= 0 || cmd.Process == nil {
		return nil, nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY |
				windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		ProcessMemoryLimit: uintptr(memLimitMB) * 1024 * 1024,
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
