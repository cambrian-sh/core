package app

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ENG-2135: the signal closes once, and marking it again is harmless — the
// serve group could in principle reach serveMarked twice on a retried boot.
func TestServingSignal_ClosesExactlyOnce(t *testing.T) {
	s := newServingSignal()
	select {
	case <-s.C():
		t.Fatal("closed before the kernel served")
	default:
	}
	s.markServing()
	s.markServing() // a second close would panic
	select {
	case <-s.C():
	default:
		t.Fatal("not closed after markServing")
	}
	var nilSig *servingSignal
	if nilSig.C() != nil {
		t.Fatal("a nil signal must answer a nil channel (no gate)")
	}
}

// signalCheckingServer records whether the serving signal was already closed
// when Serve was entered.
type signalCheckingServer struct {
	sig         *servingSignal
	closedAtTop bool
	calls       int
}

func (f *signalCheckingServer) Serve(net.Listener) error {
	f.calls++
	select {
	case <-f.sig.C():
		f.closedAtTop = true
	default:
	}
	return errors.New("served")
}

// The signal closes right before Serve: not earlier (nothing outside
// serveMarked closes it), and not after Serve returns.
func TestServeMarked_ClosesTheSignalRightBeforeServe(t *testing.T) {
	sig := newServingSignal()
	srv := &signalCheckingServer{sig: sig}
	if err := serveMarked(srv, nil, sig); err == nil || err.Error() != "served" {
		t.Fatalf("serveMarked must return Serve's error, got %v", err)
	}
	if srv.calls != 1 || !srv.closedAtTop {
		t.Fatalf("Serve calls=%d, signal closed on entry=%v; want 1, true", srv.calls, srv.closedAtTop)
	}
	if err := serveMarked(&signalCheckingServer{sig: newServingSignal()}, nil, nil); err == nil {
		t.Fatal("a nil signal still serves")
	}
}

func TestAwaitSpawnReady(t *testing.T) {
	t.Run("nil Serving is no gate", func(t *testing.T) {
		if err := (KernelServices{}).AwaitSpawnReady(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("waits until serving", func(t *testing.T) {
		sig := newServingSignal()
		svc := KernelServices{Serving: sig.C()}
		got := make(chan error, 1)
		go func() { got <- svc.AwaitSpawnReady(context.Background()) }()
		select {
		case err := <-got:
			t.Fatalf("returned before the kernel served: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		sig.markServing()
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not return once the kernel served")
		}
	})
	t.Run("ends with ctx", func(t *testing.T) {
		svc := KernelServices{Serving: newServingSignal().C()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := svc.AwaitSpawnReady(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})
}

// daemonDial is a stand-in for a boot-spawned daemon's first contact: it dials
// the kernel address with a short connect timeout (the SDK's behaviour — a
// connection that never gets HTTP/2 SETTINGS is abandoned and redialled), makes
// one RPC, and reports how many TCP dials that took.
func daemonDial(t *testing.T, addr string) (dials int32, err error) {
	t.Helper()
	var n atomic.Int32
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) {
			n.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", a)
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 50 * time.Millisecond, Multiplier: 1, MaxDelay: 50 * time.Millisecond},
			MinConnectTimeout: 150 * time.Millisecond,
		}),
	)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
	return n.Load(), err
}

// ENG-2135 end to end: the kernel's boot order — listener bound early, a
// registration window, then serveMarked — with a daemon spawned during boot
// through the plugin seam (KernelServices.AwaitSpawnReady, as the premium
// Telegram restore does). Gated, the daemon connects on its FIRST dial. The
// ungated control spawns at Build time, as shipped, and needs more than one.
func TestE2E_BootSpawnedDaemonConnectsOnItsFirstDial(t *testing.T) {
	const registrationWindow = 600 * time.Millisecond // stands in for the 40-45 s ConnectAll window

	boot := func(t *testing.T, gated bool) int32 {
		lis, err := net.Listen("tcp", "127.0.0.1:0") // bound early (SEC-03)
		if err != nil {
			t.Fatal(err)
		}
		sig := newServingSignal()
		svc := KernelServices{Serving: sig.C()} // what buildKernelServices hands every plugin
		srv := grpc.NewServer()
		t.Cleanup(srv.Stop)

		type result struct {
			dials int32
			err   error
		}
		got := make(chan result, 1)
		go func() { // the plugin's boot-time spawn
			if gated {
				if err := svc.AwaitSpawnReady(context.Background()); err != nil {
					got <- result{err: err}
					return
				}
			}
			d, err := daemonDial(t, lis.Addr().String())
			got <- result{d, err}
		}()

		time.Sleep(registrationWindow) // services register; nothing is accepted yet
		healthpb.RegisterHealthServer(srv, health.NewServer())
		go func() { _ = serveMarked(srv, lis, sig) }()

		r := <-got
		if r.err != nil {
			t.Fatalf("daemon RPC: %v", r.err)
		}
		return r.dials
	}

	t.Run("gated", func(t *testing.T) {
		if d := boot(t, true); d != 1 {
			t.Fatalf("a daemon spawned through AwaitSpawnReady took %d dials; want exactly 1", d)
		}
	})
	t.Run("ungated control", func(t *testing.T) {
		if d := boot(t, false); d < 2 {
			t.Fatalf("control: an ungated boot-time daemon took %d dial(s); the backlog window should force a redial", d)
		}
	})
}
