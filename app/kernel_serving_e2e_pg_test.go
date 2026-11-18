package app

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// ENG-2135 through the real boot: bootstrapKernel hands a plugin the kernel's
// serving signal at Build; it stays shut through the rest of the boot and the
// lifecycle start; startKernelServices opens it as it serves; and a daemon
// released by AwaitSpawnReady at that moment connects to the real kernel server
// on its first dial. Postgres via PG_TEST_DSN (a scratch database, dropped).
func TestPg_BootSpawnGateOpensWhenTheKernelServes(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var svc KernelServices
	k, err := bootstrapKernel(ctx, cfg, lis, Options{Plugins: []Plugin{bootProbePlugin{got: &svc}}}, nil)
	if err != nil {
		_ = lis.Close()
		t.Fatalf("bootstrapKernel: %v", err)
	}
	if svc.Serving == nil || svc.Serving != k.serving.C() {
		t.Fatal("the plugin was not handed the kernel's own serving signal")
	}
	select {
	case <-svc.Serving:
		t.Fatal("the serving signal opened during bootstrapKernel, before anything serves")
	default:
	}

	// The plugin's boot-time spawn: waits on the gate, then dials the kernel.
	type result struct {
		dials int32
		err   error
	}
	got := make(chan result, 1)
	go func() {
		if err := svc.AwaitSpawnReady(ctx); err != nil {
			got <- result{err: err}
			return
		}
		d, err := daemonDial(t, lis.Addr().String())
		got <- result{d, err}
	}()

	g, gCtx := errgroup.WithContext(ctx)
	startKernelServices(g, gCtx, k)
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("boot-spawned daemon: %v", r.err)
		}
		if r.dials != 1 {
			t.Fatalf("a daemon released by the gate took %d dials; want exactly 1", r.dials)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the serving signal never opened")
	}
	cancel()
	_ = lis.Close()
	_ = g.Wait()
	shutdownWithin(t, k, 30*time.Second)
}
