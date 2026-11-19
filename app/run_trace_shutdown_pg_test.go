package app

import (
	"context"
	"net"
	"os"
	"testing"
)

// ENG-2102, through the real boot: a kernel booted by bootstrapKernel with
// execution.trace on writes its trace file into t.TempDir, and Kernel.Shutdown
// releases it — the file is removable afterwards (on Windows an open file is
// not, which is how this bug surfaced in the ENG-1123 boot-graph test).
func TestPg_KernelShutdownReleasesTheRunTraceFile(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	traceDir := t.TempDir()
	cfg.Execution.Trace.Enabled, cfg.Execution.Trace.Dir = true, traceDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	k, err := bootstrapKernel(ctx, cfg, lis, Options{}, nil)
	if err != nil {
		_ = lis.Close()
		t.Fatalf("bootstrapKernel: %v", err)
	}
	if k.runTrace == nil {
		t.Fatal("execution.trace on: the kernel must hold the trace file")
	}
	k.Shutdown(ctx)

	files, err := os.ReadDir(traceDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("trace files = %v (%v), want 1", files, err)
	}
	if err := os.Remove(traceDir + string(os.PathSeparator) + files[0].Name()); err != nil {
		t.Fatalf("after Shutdown the trace file must be released: %v", err)
	}
}
