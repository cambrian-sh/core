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

// PythonSpawnReadiness is the sandbox Python env's half of the spawn gate
// (ENG-2137). Ready closes once Python agents may start (pyenv.Env.ReadyC);
// Settled closes once no env build is running or pending (pyenv.Env.SettledC).
// Settled without Ready means the env will not become ready by itself — a
// failed build, or nothing built with build_on_boot off — so a waiting spawn
// goes ahead and the launch gate refuses it with the reason and the fix,
// rather than the spawn waiting silently until shutdown. A nil Ready is no
// Python wait (no managed env, or the env is unavailable on this host).
type PythonSpawnReadiness struct {
	Ready   <-chan struct{}
	Settled <-chan struct{}
}

// AwaitSpawnReady blocks until a daemon spawned now would be served on its
// first dial and could launch, or ctx ends (its error is returned). It is the
// ONE gate a plugin's boot-time daemon spawn waits on (a restored ingress, a
// reactive watch's stream daemon), so every readiness condition a spawn needs
// composes here rather than at each caller: the kernel serving (ENG-2135),
// then the sandbox Python env ready (ENG-2137; see PythonSpawnReadiness for
// when it stops waiting on an env that will not become ready).
//
// An open gate answers nil even when ctx is already done, so a caller can ask
// "would a spawn go now?" without blocking by passing a cancelled context —
// the reactive watch handler does, to spawn synchronously when it can.
//
// A nil Serving channel / zero PythonEnv — KernelServices{} in tests, or a
// kernel built without the signal — is no gate, which is the behaviour before
// ENG-2135. This orders startup; it is not an access control, so absent means
// "do not wait".
func (s KernelServices) AwaitSpawnReady(ctx context.Context) error {
	return spawnGate{Serving: s.Serving, PythonEnv: s.PythonEnv}.await(ctx)
}

// spawnGate is the one implementation of the boot-time spawn gate: the plugins
// reach it through KernelServices.AwaitSpawnReady, the core chat pool through
// chatPoolStarter (ENG-2148), so a new spawn precondition is added here once
// and every boot-time spawner waits on it (AGENTS.md §4.3).
type spawnGate struct {
	Serving   <-chan struct{}
	PythonEnv PythonSpawnReadiness
}

// await blocks until a daemon spawned now would be served on its first dial
// and could launch, or ctx ends; see KernelServices.AwaitSpawnReady.
func (g spawnGate) await(ctx context.Context) error {
	if err := awaitEither(ctx, g.Serving, nil); err != nil {
		return err
	}
	if g.PythonEnv.Ready == nil {
		return nil
	}
	return awaitEither(ctx, g.PythonEnv.Ready, g.PythonEnv.Settled)
}

// serving reports, without blocking, whether the kernel-serving half of the
// gate is open (a nil Serving is no gate, so open) — for a readiness reason.
func (g spawnGate) serving() bool { return isClosed(g.Serving, true) }

// isClosed reports whether ch is closed, without blocking; ifNil for a nil ch.
func isClosed(ch <-chan struct{}, ifNil bool) bool {
	if ch == nil {
		return ifNil
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// awaitEither returns nil once a or b is closed — at once when one already is,
// whatever ctx says — else ctx's error when ctx ends first. Both nil is no gate.
// A nil channel never fires in a select, so a single gate passes b == nil.
func awaitEither(ctx context.Context, a, b <-chan struct{}) error {
	if a == nil && b == nil {
		return nil
	}
	select {
	case <-a:
		return nil
	case <-b:
		return nil
	default:
	}
	select {
	case <-a:
		return nil
	case <-b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
