package config

import "fmt"

// RuntimeConfig — process-level diagnostics and limits of the kernel binary itself.
// Every key is OPT-IN and defaults to off, so a deployment that sets nothing runs
// exactly as before (T-20261103-kernel-runs-with-no-go-memory-limit-add-a,
// T-20261103-no-way-to-profile-kernel-memory-in-a-running).
type RuntimeConfig struct {
	// MemoryLimitMB is a Go SOFT memory limit (runtime/debug.SetMemoryLimit), in MiB.
	// 0 = off (the Go default: no limit, GOGC=100). The GOMEMLIMIT environment
	// variable keeps working and wins when set: the runtime reads it before main, and
	// overriding a limit a deployment pinned in its unit file would be a surprise.
	// A soft limit makes the GC work harder near the limit and returns memory to the
	// OS sooner; it is NOT an OOM guard — live data above the limit makes the GC
	// thrash instead.
	MemoryLimitMB int `json:"memory_limit_mb"`

	// PprofPort serves net/http/pprof (/debug/pprof/*) on its own listener when > 0.
	// 0 = off. pprof is unauthenticated and exposes goroutine stacks and profiles,
	// so the kernel REFUSES to boot with it set unless server.bind_address is
	// loopback (the default); it is served under the one listener policy (ADR-0130
	// D3, decisions/all-listeners-share-the-grpc-listener-policy).
	PprofPort int `json:"pprof_port"`

	// MemStatsLogIntervalSeconds logs one INFO line of runtime/metrics memory figures
	// (heap objects, heap released, total mapped, GC goal, goroutines) every N
	// seconds, so a kernel log carries a memory timeline a benchmark run can import.
	// 0 = off.
	MemStatsLogIntervalSeconds int `json:"memstats_log_interval_seconds"`
}

// validationErrors reports out-of-range values; every zero value is valid (off).
func (r RuntimeConfig) validationErrors() []string {
	var errs []string
	if r.MemoryLimitMB < 0 {
		errs = append(errs, fmt.Sprintf("runtime.memory_limit_mb must be >= 0 (0 = off), got %d", r.MemoryLimitMB))
	}
	if r.PprofPort < 0 || r.PprofPort > 65535 {
		errs = append(errs, fmt.Sprintf("runtime.pprof_port must be 0 (off) or a port 1-65535, got %d", r.PprofPort))
	}
	if r.MemStatsLogIntervalSeconds < 0 {
		errs = append(errs, fmt.Sprintf("runtime.memstats_log_interval_seconds must be >= 0 (0 = off), got %d",
			r.MemStatsLogIntervalSeconds))
	}
	return errs
}
