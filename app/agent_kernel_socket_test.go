package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ADR-0157 S2: the kernel serves its one gRPC plane on a kernel-only agent's
// unix socket. A socket opened before the server starts waits in the backlog
// and is served once start runs; Close stops it and removes the file; stop
// closes never-served sockets, refuses new ones and leaves no goroutine.
func TestAgentSocketServer_ServesTheKernelPlaneOnAUnixSocket(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir, err := os.MkdirTemp("", "aks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "kernel.sock")
	never := filepath.Join(dir, "never.sock")

	s := newAgentSocketServer()
	c1, err := s.Open(path) // before the server exists: pending
	if err != nil {
		t.Fatal(err)
	}
	cNever, err := s.Open(never)
	if err != nil {
		t.Fatal(err)
	}
	if err := cNever.Close(); err != nil { // closed while pending: never served
		t.Fatal(err)
	}

	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	s.start(srv) // after registration, as app.go does right before the main Serve

	conn, err := grpc.NewClient("unix:"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health over the kernel socket: %v %v", resp, err)
	}
	_ = conn.Close()

	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file left after Close: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	pending, err := s.Open(filepath.Join(dir, "late.sock")) // served at once now
	if err != nil {
		t.Fatal(err)
	}
	srv.GracefulStop() // Shutdown order: GracefulStop, then stop
	s.stop()
	_ = pending.Close()
	if _, err := s.Open(filepath.Join(dir, "after.sock")); !errors.Is(err, errAgentSocketsStopped) {
		t.Fatalf("Open after stop: %v", err)
	}
}
