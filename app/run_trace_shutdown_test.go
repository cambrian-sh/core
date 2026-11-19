package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2102: the kernel holds the run-trace file wireRunTrace opened and
// Shutdown closes it, so an in-process kernel leaves nothing open (on Windows
// an open file keeps t.TempDir from being removed).
func TestKernelShutdown_ClosesTheRunTraceFile(t *testing.T) {
	if runTraceCloser(nil) != nil || runTraceCloser(wireRunTrace(config.TraceConfig{})) != nil {
		t.Fatal("tracing off must give Shutdown nothing to close")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	tr := wireRunTrace(config.TraceConfig{Enabled: true, Dir: dir})
	if tr == nil {
		t.Fatal("on must wire a tracer")
	}
	tr.Span(t.Context(), domain.TraceSpan{Name: "chat m"})

	k := &Kernel{runTrace: runTraceCloser(tr)}
	k.Shutdown(context.Background())

	tr.Span(t.Context(), domain.TraceSpan{Name: "late"}) // after Shutdown: dropped, no panic
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("trace files = %v (%v), want 1", files, err)
	}
	if err := os.Remove(filepath.Join(dir, files[0].Name())); err != nil {
		t.Fatalf("Shutdown must release the trace file: %v", err)
	}
}
