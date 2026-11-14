package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cambrian-sh/core/internal/config"
)

// The runtime block is opt-in: with nothing set the kernel must behave exactly as before.
func TestRuntimeConfig_DefaultsAreOff(t *testing.T) {
	t.Parallel()
	rc := config.DefaultConfig().Runtime
	if rc.MemoryLimitMB != 0 || rc.PprofPort != 0 || rc.MemStatsLogIntervalSeconds != 0 {
		t.Fatalf("runtime diagnostics must default to off, got %+v", rc)
	}
	if got := applyMemoryLimit(rc, func(string) string { return "" }); got != 0 {
		t.Fatalf("no limit may be applied by default, applied %d", got)
	}
	if why := pprofRefusal(config.DefaultConfig()); why != "" {
		t.Fatalf("off pprof must not refuse: %s", why)
	}
}

// Not parallel: it changes the process-wide memory limit and restores it.
func TestApplyMemoryLimit_SetsLimitAndEnvWins(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	if got := applyMemoryLimit(config.RuntimeConfig{MemoryLimitMB: 3072}, func(string) string { return "" }); got != 3072<<20 {
		t.Fatalf("applied %d, want %d", got, int64(3072)<<20)
	}
	if cur := debug.SetMemoryLimit(-1); cur != 3072<<20 {
		t.Fatalf("runtime limit = %d, want %d", cur, int64(3072)<<20)
	}
	debug.SetMemoryLimit(prev)
	env := func(k string) string {
		if k == "GOMEMLIMIT" {
			return "2GiB"
		}
		return ""
	}
	if got := applyMemoryLimit(config.RuntimeConfig{MemoryLimitMB: 3072}, env); got != 0 {
		t.Fatalf("GOMEMLIMIT must win, but the config limit was applied (%d)", got)
	}
}

func TestPprofRefusal_OnlyLoopback(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Runtime.PprofPort = 6060
	for bind, refused := range map[string]bool{"": false, "127.0.0.1": false, "localhost": false, "::1": false,
		"0.0.0.0": true, "10.0.0.5": true} {
		c := *cfg
		c.Server.BindAddress = bind
		if got := pprofRefusal(&c) != ""; got != refused {
			t.Errorf("bind %q: refused=%v, want %v", bind, got, refused)
		}
	}
}

// End to end through the listener policy: the pprof listener serves /debug/pprof/
// on loopback and stops with ctx.
func TestStartRuntimeDiagnostics_ServesPprofOnLoopback(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	cfg := config.DefaultConfig()
	cfg.Runtime.PprofPort = port
	cfg.Runtime.MemStatsLogIntervalSeconds = 1
	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	startRuntimeDiagnostics(g, gctx, cfg)

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/debug/pprof/"
	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "goroutine") {
		t.Fatalf("pprof index not served; body=%q", body)
	}
	cancel()
	if err := g.Wait(); err != nil {
		t.Fatalf("diagnostics must stop cleanly with ctx: %v", err)
	}
}

func TestReadMemStats_ReportsTheFigures(t *testing.T) {
	t.Parallel()
	m := readMemStats()
	for _, k := range []string{"heap_objects_bytes", "total_mapped_bytes", "goroutines"} {
		if m[k] == 0 {
			t.Errorf("%s missing or zero: %v", k, m)
		}
	}
}
