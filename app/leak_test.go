//go:build chaos

package app

import (
	"context"
	"net"
	"os"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/config"

	"go.uber.org/goleak"
)

func TestKernel_NoGoroutineLeak(t *testing.T) {
	// A full kernel boot needs Postgres and an embedder, so the config comes from
	// the environment rather than a checked-in file: CAMBRIAN_TEST_CONFIG names a
	// config.json for a disposable kernel. (The old hard-coded
	// "../../configs/test-config.json" pointed above the repo root and never
	// existed, so the test skipped on every machine.)
	path := os.Getenv("CAMBRIAN_TEST_CONFIG")
	if path == "" {
		t.Skip("set CAMBRIAN_TEST_CONFIG to a kernel config.json to run the full-kernel leak test")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k, err := bootstrapKernel(ctx, cfg, lis, Options{})
	if err != nil {
		t.Fatalf("bootstrapKernel: %v", err)
	}

	// Execute a minimal plan to exercise worker paths.
	h := k.Server
	_, _ = h.Execute(ctx, &pb.Handoff{
		Id:        "leak-test-001",
		FromAgent: "test",
		ToAgent:   "test",
		Payload:   &pb.Object{Id: "p", Type: "text", Data: []byte("hello")},
	})

	k.Shutdown(ctx)

	// goleak retries internally with backoff before reporting, so no hand-rolled
	// sleep loop is needed.
	goleak.VerifyNone(t)
}
