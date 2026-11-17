package app

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
)

// agentSocketServer serves the kernel's gRPC plane on one unix socket per
// KERNEL-ONLY agent instance (ADR-0157 S2, D3). A third-party agent — and every
// agent under execution.agents.sandbox.network: kernel_only_all — runs with no
// network, so the agent manager asks for a socket in the instance's own socket
// directory (which the sandbox already shows at its own path) and passes it as
// --substrate-addr unix:<path>.
//
// It is the SAME grpc.Server as the TCP plane, with the same interceptor chain
// (surface → agent credential → operator auth → rate limit) and the same
// transport credentials: there is no second plane and no relaxed check. A unix
// peer is local by construction (authz.routablePeer), exactly as a loopback one.
//
// Ordering: the server is built inside the gRPC goroutine after bootstrap, and
// grpc-go refuses RegisterService after Serve, while daemons and the chat pool
// can spawn before that. So Open listens AT ONCE — the agent's dial waits in the
// listen backlog, as it does on the TCP listener, which is also bound long
// before Serve — and Serve starts on it once start(srv) is called, right before
// the main listener's Serve. Each served listener has one goroutine, owned by
// this type: it ends when the instance's socket is closed (instance cleanup),
// by GracefulStop at shutdown, or — for a listener never served — by stop.
type agentSocketServer struct {
	mu      sync.Mutex
	srv     *grpc.Server
	pending map[net.Listener]struct{}
	stopped bool
	wg      sync.WaitGroup
}

func newAgentSocketServer() *agentSocketServer {
	return &agentSocketServer{pending: map[net.Listener]struct{}{}}
}

// errAgentSocketsStopped is returned by Open once the kernel is shutting down.
var errAgentSocketsStopped = errors.New("kernel agent sockets are stopped (shutting down)")

// Open listens on path (listenAgentUnix: the one listener policy) and serves the
// kernel's plane there, now or once the gRPC server starts. Close stops it and
// removes the socket file.
func (s *agentSocketServer) Open(path string) (io.Closer, error) {
	lis, err := listenAgentUnix(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		_ = lis.Close()
		return nil, errAgentSocketsStopped
	}
	if s.srv != nil {
		s.serveLocked(lis)
	} else {
		s.pending[lis] = struct{}{}
	}
	return &agentSocketCloser{s: s, lis: lis}, nil
}

// start serves every pending socket and every later one on srv. Called once,
// after every service is registered and right before the main Serve.
func (s *agentSocketServer) start(srv *grpc.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.srv != nil {
		return
	}
	s.srv = srv
	for lis := range s.pending {
		s.serveLocked(lis)
	}
	s.pending = map[net.Listener]struct{}{}
}

func (s *agentSocketServer) serveLocked(lis net.Listener) {
	srv := s.srv
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Returns when the listener is closed (instance cleanup) or the server
		// stops; neither is an error worth more than a debug line.
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
			slog.Debug("ADR-0157: kernel agent socket stopped serving", "addr", lis.Addr().String(), "err", err)
		}
	}()
}

// stop refuses new sockets, closes those never served and waits for every
// serving goroutine. Call it after the gRPC server's (Graceful)Stop, which
// ends the served ones.
func (s *agentSocketServer) stop() {
	s.mu.Lock()
	s.stopped = true
	for lis := range s.pending {
		_ = lis.Close()
	}
	s.pending = map[net.Listener]struct{}{}
	s.mu.Unlock()
	s.wg.Wait()
}

type agentSocketCloser struct {
	s    *agentSocketServer
	lis  net.Listener
	once sync.Once
}

func (c *agentSocketCloser) Close() error {
	var err error
	c.once.Do(func() {
		c.s.mu.Lock()
		delete(c.s.pending, c.lis)
		c.s.mu.Unlock()
		if cerr := c.lis.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = fmt.Errorf("close kernel agent socket: %w", cerr)
		}
	})
	return err
}
