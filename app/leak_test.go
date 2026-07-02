//go:build chaos

package app

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"

	"go.uber.org/goleak"
)

func TestKernel_NoGoroutineLeak(t *testing.T) {
	cfg, err := config.LoadConfig("../../configs/test-config.json")
	if err != nil {
		t.Skipf("chaos test requires test-config.json: %v", err)
	}

	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k, err := bootstrapKernel(ctx, cfg, lis)
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

	// Retry with backoff: slow CI runners may need >100ms for goroutines to exit.
	var lastErr error
	for range 10 {
		if lastErr = goleak.Find(); lastErr == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak detected: %v", lastErr)
}
