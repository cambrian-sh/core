package app

import (
	"context"
	"net"
	"sync"
)

// servingSignal announces that the kernel's gRPC server is serving
// (T-20261118-plugin-daemons-spawn-before-the-kernel-serves, ENG-2135).
//
// The operator/agent listener is bound early (SEC-03 fail-fast), but Serve runs
// only at the end of the serve group, after every service is registered. A
// daemon spawned in between dials into the OS backlog: the TCP handshake
// completes, no HTTP/2 SETTINGS ever arrive, the SDK times out and redials, and
// each abandoned connection sits in CLOSE_WAIT on the kernel side (ENG-353).
// Serve cannot move earlier than registration, so spawners wait for this.
//
// Closed exactly once, by serveMarked, immediately before Serve.
type servingSignal struct {
	once sync.Once
	ch   chan struct{}
}

func newServingSignal() *servingSignal { return &servingSignal{ch: make(chan struct{})} }

// C is closed once the kernel serves.
// A nil signal answers nil, which AwaitSpawnReady treats as no gate.
func (s *servingSignal) C() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.ch
}

func (s *servingSignal) markServing() { s.once.Do(func() { close(s.ch) }) }

// grpcServer is the one method serveMarked needs from *grpc.Server.
type grpcServer interface {
	Serve(net.Listener) error
}

// serveMarked closes the serving signal and serves. Every registration has run
// by the time it is called, so a daemon released by the signal dials a server
// whose Accept loop starts in the same instant; the window between close and
// Accept is one function call, and a dial landing in it is accepted as soon as
// Serve starts rather than abandoned. A nil signal serves unmarked.
func serveMarked(srv grpcServer, lis net.Listener, sig *servingSignal) error {
	if sig != nil {
		sig.markServing()
	}
	return srv.Serve(lis)
}

// AwaitSpawnReady blocks until a daemon spawned now would be served on its
// first dial, or ctx ends (its error is returned). It is the ONE gate a
// plugin's boot-time daemon spawn waits on (a restored ingress, say), so every
// readiness condition a spawn needs composes here rather than at each caller:
// today the kernel serving; the sandbox Python env's readiness is meant to join
// it here (ENG-2137) without any plugin changing.
//
// A nil Serving channel — KernelServices{} in tests, or a kernel built without
// the signal — is no gate, which is the behaviour before ENG-2135. This orders
// startup; it is not an access control, so absent means "do not wait".
func (s KernelServices) AwaitSpawnReady(ctx context.Context) error {
	if s.Serving == nil {
		return ctx.Err()
	}
	select {
	case <-s.Serving:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
