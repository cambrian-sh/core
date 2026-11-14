package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cambrian-sh/core/internal/config"
)

// Process-level diagnostics from the `runtime` config block (config/runtime.go). Every
// one is opt-in and off by default, so a kernel that sets nothing runs exactly as
// before. T-20261103-kernel-runs-with-no-go-memory-limit-add-a and
// T-20261103-no-way-to-profile-kernel-memory-in-a-running: the 4.7 GB resident kernel
// (investigations/2026-11-03-kernel-resident-memory.md) could only be explained after
// the fact from logs and code.

// applyMemoryLimit sets the Go soft memory limit from runtime.memory_limit_mb. It
// returns the limit it applied in bytes, or 0 when it applied none. GOMEMLIMIT in the
// environment wins: the runtime has already applied it before main, and a limit a
// deployment pinned in its unit file is not silently replaced by a config value.
func applyMemoryLimit(rc config.RuntimeConfig, getenv func(string) string) int64 {
	if rc.MemoryLimitMB <= 0 {
		return 0
	}
	if env := strings.TrimSpace(getenv("GOMEMLIMIT")); env != "" {
		slog.Info("runtime: GOMEMLIMIT is set and wins over runtime.memory_limit_mb",
			"gomemlimit", env, "memory_limit_mb", rc.MemoryLimitMB)
		return 0
	}
	limit := int64(rc.MemoryLimitMB) << 20
	debug.SetMemoryLimit(limit)
	slog.Info("runtime: Go soft memory limit set (runtime.memory_limit_mb; a soft limit, not an OOM guard)",
		"memory_limit_mb", rc.MemoryLimitMB)
	return limit
}

// pprofHandler serves the net/http/pprof endpoints on a mux of its own. The handlers
// are registered explicitly rather than through http.DefaultServeMux (which importing
// net/http/pprof also populates): no kernel listener serves DefaultServeMux, and this
// keeps it that way by construction.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// pprofRefusal returns why the pprof listener must not start, or "". pprof is
// unauthenticated and exposes goroutine stacks and profiles, so it is allowed only on
// a loopback bind; on any other bind the operator asked for something unsafe, and the
// boot fails rather than silently skipping it (a requested diagnostic that is quietly
// absent is the same trap as a validated-but-unread key).
func pprofRefusal(cfg *config.Config) string {
	if cfg.Runtime.PprofPort <= 0 {
		return ""
	}
	if !isLoopback(cfg.Server.BindAddress) {
		return fmt.Sprintf("runtime.pprof_port is set but server.bind_address %q is not loopback; "+
			"pprof is unauthenticated and is served only on a loopback bind", cfg.Server.BindAddress)
	}
	return ""
}

// startRuntimeDiagnostics starts the opt-in pprof listener and memory log line under
// g, both stopped by ctx (one owner per goroutine, AGENTS.md §3.3).
func startRuntimeDiagnostics(g *errgroup.Group, ctx context.Context, cfg *config.Config) {
	if cfg.Runtime.PprofPort > 0 {
		g.Go(func() error {
			if why := pprofRefusal(cfg); why != "" {
				return fmt.Errorf("runtime diagnostics: %s", why)
			}
			srv, addr, err := serveHTTP(cfg, strconv.Itoa(cfg.Runtime.PprofPort), pprofHandler(), "pprof")
			if err != nil {
				return fmt.Errorf("runtime diagnostics: %w", err)
			}
			slog.Warn("runtime: pprof is serving (unauthenticated; loopback only)", "addr", addr)
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		})
	}
	if secs := cfg.Runtime.MemStatsLogIntervalSeconds; secs > 0 {
		g.Go(func() error {
			t := time.NewTicker(time.Duration(secs) * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					logMemStats()
				}
			}
		})
	}
}

// memStatsSamples are the runtime/metrics read by the memory log line.
var memStatsSamples = []struct{ name, key string }{
	{"/memory/classes/heap/objects:bytes", "heap_objects_bytes"},
	{"/memory/classes/heap/released:bytes", "heap_released_bytes"},
	{"/memory/classes/total:bytes", "total_mapped_bytes"},
	{"/gc/heap/goal:bytes", "gc_goal_bytes"},
	{"/sched/goroutines:goroutines", "goroutines"},
}

// readMemStats reads memStatsSamples; a metric this Go version does not support is
// omitted rather than reported as 0.
func readMemStats() map[string]uint64 {
	samples := make([]metrics.Sample, len(memStatsSamples))
	for i, s := range memStatsSamples {
		samples[i].Name = s.name
	}
	metrics.Read(samples)
	out := make(map[string]uint64, len(samples))
	for i, s := range samples {
		if s.Value.Kind() == metrics.KindUint64 {
			out[memStatsSamples[i].key] = s.Value.Uint64()
		}
	}
	return out
}

func logMemStats() {
	m := readMemStats()
	attrs := make([]any, 0, 2*len(memStatsSamples))
	for _, s := range memStatsSamples {
		if v, ok := m[s.key]; ok {
			attrs = append(attrs, s.key, v)
		}
	}
	slog.Info("runtime: memory", attrs...)
}

// osGetenv is the environment lookup applyMemoryLimit uses in production.
var osGetenv = os.Getenv
