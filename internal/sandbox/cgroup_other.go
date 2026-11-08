//go:build !linux && !windows

package sandbox

import "syscall"

// Pdeathsig and cgroups are Linux-only; macOS/BSD get the sh ulimit limits and
// the process group.

func setPdeathsig(*syscall.SysProcAttr) {}

func cgroupUsable(string) (bool, string) { return false, "cgroups are Linux-only" }

type cgroup struct{}

func newCgroup(string, string, TierPolicy) (*cgroup, error) { return nil, nil }
func (*cgroup) attach(*syscall.SysProcAttr)                 {}
func (*cgroup) closeFD()                                    {}
func (*cgroup) kill() error                                 { return nil }
func (*cgroup) remove() error                               { return nil }
