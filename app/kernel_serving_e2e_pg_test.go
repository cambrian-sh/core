package app

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// ENG-2135 through the real boot: bootstrapKernel hands a plugin the kernel's
// serving signal at Build; it stays shut through the rest of the boot and the
// lifecycle start; startKernelServices opens it as it serves; and a daemon
// released by AwaitSpawnReady at that moment connects to the real kernel server
// on its first dial. ENG-2148: the core chat pool (on by default) is behind the
// same gate — its lifecycle starts with the others, before Serve, and spawns
// nothing until the kernel serves; /healthz?component=chat says so meanwhile.
// Postgres via PG_TEST_DSN (a scratch database, dropped).
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

	chatReason := func() string {
		t.Helper()
		st, ok := k.Health.Component(chatLaneComponent)
		if !ok {
			t.Fatal("the default boot registered no chat lane health component")
		}
		return st.Reason
	}

	// startKernelServices in two halves, so the window between the lifecycles
	// starting and Serve is held open and observable.
	g, gCtx := errgroup.WithContext(ctx)
	startBackgroundWorkers(g, gCtx, k)
	time.Sleep(500 * time.Millisecond) // a spawn behind no gate would have gone by now
	if r := chatReason(); !strings.Contains(r, "waiting for the kernel to serve") {
		t.Fatalf("chat lane before the kernel serves: %q; want it waiting for the kernel to serve", r)
	}
	select {
	case r := <-got:
		t.Fatalf("the plugin spawn went before the kernel serves: %+v", r)
	default:
	}
	operatorIDP, agentAuth, planeLimiter, ok := buildPlaneAuth(g, gCtx, k)
	if !ok {
		t.Fatalf("buildPlaneAuth failed: %v", g.Wait())
	}
	serveGRPC(g, gCtx, k, operatorIDP, agentAuth, planeLimiter)
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
	// The kernel serves: the chat pool went on to its start. A test boot has no
	// chat agent to spawn (no agents directory), so the start is refused and
	// the lane reports that refusal — no longer waiting on the kernel.
	deadline := time.Now().Add(30 * time.Second)
	for r := chatReason(); strings.Contains(r, "waiting for the kernel to serve") || r == ""; r = chatReason() {
		if time.Now().After(deadline) {
			t.Fatalf("chat lane still %q after the kernel served", r)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	_ = lis.Close()
	_ = g.Wait()
	shutdownWithin(t, k, 30*time.Second)
}
