package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Resolver looks up a host's addresses (net.DefaultResolver satisfies it).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Options configure a Broker. The zero value is the production setting.
type Options struct {
	// Resolver resolves admitted names; nil ⇒ net.DefaultResolver.
	Resolver Resolver
	// Dial opens the upstream connection; nil ⇒ a net.Dialer with DialTimeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialTimeout bounds one upstream dial; 0 ⇒ 15 s.
	DialTimeout time.Duration
	// HandshakeTimeout bounds reading the CONNECT request; 0 ⇒ 30 s.
	HandshakeTimeout time.Duration
}

// ErrNotRunning is returned by Open and Serve before Start or after Stop.
var ErrNotRunning = errors.New("egressproxy: broker is not running")

// Broker owns every proxy session and every goroutine serving one (AGENTS.md
// §3.3: Start(ctx)/Stop(), one owner). The kernel builds one at the
// composition root and injects it into the sandbox policy; Stop at shutdown
// ends every session and waits for its goroutines.
type Broker struct {
	opts Options

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	running  bool
	sessions map[*Session]struct{}
	wg       sync.WaitGroup
}

// New builds a Broker; it serves nothing until Start.
func New(opts Options) *Broker {
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 15 * time.Second
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = 30 * time.Second
	}
	if opts.Dial == nil {
		d := &net.Dialer{Timeout: opts.DialTimeout}
		opts.Dial = d.DialContext
	}
	return &Broker{opts: opts, sessions: map[*Session]struct{}{}}
}

// Start makes the broker accept sessions. ctx bounds the broker's life (its
// cancellation is a Stop without the wait); Stop is still required.
func (b *Broker) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return errors.New("egressproxy: broker already started")
	}
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.running = true
	return nil
}

// Stop ends every session (closing its multiplex, which ends its streams and
// their upstream connections) and waits for every goroutine. Idempotent.
func (b *Broker) Stop() {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		b.wg.Wait()
		return
	}
	b.running = false
	b.cancel()
	sessions := make([]*Session, 0, len(b.sessions))
	for s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()
	for _, s := range sessions {
		s.closeTransports()
	}
	b.wg.Wait()
}

// Open starts a session for one launch of serverID, admitting only hosts
// (ParseAllowlist's syntax).
func (b *Broker) Open(serverID string, hosts []string) (*Session, error) {
	allow, err := ParseAllowlist(hosts)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return nil, ErrNotRunning
	}
	s := &Session{b: b, server: serverID, allow: allow, ready: make(chan struct{})}
	b.sessions[s] = struct{}{}
	return s, nil
}

// Session is one launch's proxy: the server's allowlist and the multiplexes
// its forwarder speaks over.
type Session struct {
	b      *Broker
	server string
	allow  *Allowlist

	mu         sync.Mutex
	transports []io.Closer
	closed     bool
	wg         sync.WaitGroup

	readyOnce sync.Once
	ready     chan struct{}

	admitted atomic.Int64
	refused  atomic.Int64
}

// Hosts is the session's allowlist as declared.
func (s *Session) Hosts() []string { return s.allow.Hosts() }

// Ready is closed when a forwarder announced it is listening.
func (s *Session) Ready() <-chan struct{} { return s.ready }

// Counts reports how many CONNECTs were admitted and refused.
func (s *Session) Counts() (admitted, refused int64) {
	return s.admitted.Load(), s.refused.Load()
}

// Serve serves one forwarder's multiplex on rwc in a goroutine the broker
// owns, until rwc ends or the session closes. rwc is closed when it returns.
func (s *Session) Serve(rwc io.ReadWriteCloser) error {
	s.b.mu.Lock()
	running := s.b.running
	ctx := s.b.ctx
	if running {
		s.b.wg.Add(1)
	}
	s.b.mu.Unlock()
	if !running {
		_ = rwc.Close()
		return ErrNotRunning
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.b.wg.Done()
		_ = rwc.Close()
		return errors.New("egressproxy: session closed")
	}
	s.transports = append(s.transports, rwc)
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.b.wg.Done()
		defer s.wg.Done()
		err := serveMux(rwc, func() { s.readyOnce.Do(func() { close(s.ready) }) },
			func(c net.Conn) { s.serveConn(ctx, c) })
		if err != nil {
			slog.Warn("egress proxy: forwarder multiplex ended with an error", "server", s.server, "err", err)
		}
	}()
	return nil
}

func (s *Session) closeTransports() {
	s.mu.Lock()
	s.closed = true
	ts := s.transports
	s.transports = nil
	s.mu.Unlock()
	for _, t := range ts {
		_ = t.Close()
	}
}

// Close ends the session's multiplexes and waits for their goroutines.
// Idempotent.
func (s *Session) Close() {
	s.closeTransports()
	s.wg.Wait()
	s.b.mu.Lock()
	delete(s.b.sessions, s)
	s.b.mu.Unlock()
}

// serveConn handles one relayed connection: one CONNECT request, admitted or
// refused, then a byte tunnel.
func (s *Session) serveConn(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(s.b.opts.HandshakeTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		s.refuse(c, http.StatusMethodNotAllowed, req.Method+" "+req.Host,
			"the egress proxy tunnels HTTPS only (CONNECT); plain-HTTP requests are refused")
		return
	}
	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		s.refuse(c, http.StatusBadRequest, req.Host, "CONNECT target must be host:port")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		s.refuse(c, http.StatusBadRequest, req.Host, "bad port")
		return
	}
	ok, literal := s.allow.match(host, port)
	if !ok {
		why := "not in the server's declared hosts"
		if net.ParseIP(host) != nil {
			why = "an IP address that is not itself a declared host (a declared name never admits its address as a literal)"
		}
		s.refuse(c, http.StatusForbidden, req.Host, why)
		return
	}
	var addrs []net.IP
	if literal != nil {
		addrs = []net.IP{literal} // an explicit literal entry is the operator's choice
	} else {
		lctx, cancel := context.WithTimeout(ctx, s.b.opts.DialTimeout)
		ips, lerr := s.b.opts.Resolver.LookupIPAddr(lctx, host)
		cancel()
		if lerr != nil {
			s.refuse(c, http.StatusBadGateway, req.Host, "resolve: "+lerr.Error())
			return
		}
		var blocked []string
		for _, ip := range ips {
			if forbidden(ip.IP) {
				blocked = append(blocked, ip.IP.String())
				continue
			}
			addrs = append(addrs, ip.IP)
		}
		if len(addrs) == 0 {
			s.refuse(c, http.StatusForbidden, req.Host,
				"resolves only to loopback, link-local or unspecified addresses ("+strings.Join(blocked, ",")+")")
			return
		}
	}
	var up net.Conn
	var derr error
	for _, ip := range addrs {
		dctx, cancel := context.WithTimeout(ctx, s.b.opts.DialTimeout)
		// The resolved address is dialled, never the name again: the check
		// above and the connection are about the same address.
		up, derr = s.b.opts.Dial(dctx, "tcp", net.JoinHostPort(ip.String(), portStr))
		cancel()
		if derr == nil {
			break
		}
	}
	if up == nil {
		s.refuse(c, http.StatusBadGateway, req.Host, "dial: "+derr.Error())
		return
	}
	defer func() { _ = up.Close() }()
	s.admitted.Add(1)
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	// Both copies are owned here: the first to end (or the broker stopping)
	// closes both connections, which ends the other; both are waited for.
	var copies sync.WaitGroup
	first := make(chan struct{}, 2)
	copies.Add(2)
	go func() { defer copies.Done(); _, _ = io.Copy(up, c); first <- struct{}{} }()
	go func() { defer copies.Done(); _, _ = io.Copy(c, up); first <- struct{}{} }()
	select {
	case <-first:
	case <-ctx.Done():
	}
	_ = up.Close()
	_ = c.Close()
	copies.Wait()
}

func (s *Session) refuse(c net.Conn, status int, target, why string) {
	s.refused.Add(1)
	slog.Warn("egress proxy: refused", "server", s.server, "target", target, "reason", why)
	body := "cambrian egress proxy: " + target + " refused: " + why + "\n"
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// forbidden addresses are never dialled for an admitted NAME: the kernel's
// own loopback services, link-local (the cloud metadata endpoint
// 169.254.169.254), unspecified and multicast. Private ranges stay
// reachable: an on-premises API on 10.x is a legitimate declared host.
func forbidden(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		if ip4[0] == 0 {
			return true // 0.0.0.0/8
		}
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}
