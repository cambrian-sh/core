package egressproxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// fakeForwardProxy is a corporate forward proxy: it answers one CONNECT per
// connection (recording the target and its Proxy-Authorization), with status,
// and on 200 relays the tunnel to upstream (the rig's echo server).
type fakeForwardProxy struct {
	ln       net.Listener
	upstream string
	status   int

	mu      sync.Mutex
	targets []string
	auths   []string
	wg      sync.WaitGroup
}

func newFakeForwardProxy(t *testing.T, upstream string, status int) *fakeForwardProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeForwardProxy{ln: ln, upstream: upstream, status: status}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go func() { defer p.wg.Done(); p.serve(c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.wg.Wait() })
	return p
}

func (p *fakeForwardProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Method+" "+req.Host)
	p.auths = append(p.auths, req.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if p.status != http.StatusOK {
		_, _ = io.WriteString(c, "HTTP/1.1 "+http.StatusText(p.status)+"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (p *fakeForwardProxy) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...), append([]string(nil), p.auths...)
}

// withProxy routes the broker through fp (for targets other than noProxyHost)
// and its dials of fp's address to fp; any other dial is direct, recorded and
// sent to the echo server, as in newRig.
func withProxy(fp *fakeForwardProxy, proxyURL, noProxyHost string) func(*rig, *Options) {
	return func(r *rig, o *Options) {
		u, _ := url.Parse(proxyURL)
		u.Host = fp.ln.Addr().String()
		if user, err := url.Parse(proxyURL); err == nil {
			u.User = user.User
		}
		o.Proxy = func(target string) (*url.URL, error) {
			if h, _, _ := net.SplitHostPort(target); h == noProxyHost {
				return nil, nil // NO_PROXY
			}
			return u, nil
		}
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			if addr == fp.ln.Addr().String() {
				return d.DialContext(ctx, network, addr)
			}
			r.dialed <- addr
			return d.DialContext(ctx, network, r.echoAddr)
		}
	}
}

// ENG-2261: behind a forward proxy, an admitted target is tunnelled through it
// with CONNECT <name:port> (Basic credentials when the proxy URL carries them),
// never dialled direct; a NO_PROXY target is dialled direct as before.
func TestProxiedBrokerTunnelsAdmittedTargetsThroughTheForwardProxy(t *testing.T) {
	var fp *fakeForwardProxy
	r := newRig(t, []string{"allowed.test", "*.wild.test"}, func(r *rig, o *Options) {
		fp = newFakeForwardProxy(t, r.echoAddr, http.StatusOK)
		withProxy(fp, "http://corp:s3cret@proxy.corp:3128", "api.wild.test")(r, o)
	})
	code, c, br := r.connect(t, "allowed.test:443")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200 through the forward proxy", code)
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("tunnel echo %q %v", line, err)
	}
	_ = c.Close()
	targets, auths := fp.seen()
	if len(targets) != 1 || targets[0] != "CONNECT allowed.test:443" {
		t.Fatalf("proxy saw %v, want one CONNECT allowed.test:443", targets)
	}
	if auths[0] != "Basic Y29ycDpzM2NyZXQ=" { // corp:s3cret
		t.Fatalf("Proxy-Authorization = %q", auths[0])
	}
	select {
	case d := <-r.dialed:
		t.Fatalf("a proxied target was dialled direct: %s", d)
	default:
	}
	// NO_PROXY: direct, to the resolved address.
	code, c, _ = r.connect(t, "api.wild.test:443")
	if code != http.StatusOK {
		t.Fatalf("NO_PROXY target: status %d", code)
	}
	_ = c.Close()
	if d := <-r.dialed; d != "203.0.113.11:443" {
		t.Fatalf("NO_PROXY target dialled %s, want the resolved address", d)
	}
	if targets, _ := fp.seen(); len(targets) != 1 {
		t.Fatalf("the NO_PROXY target reached the proxy: %v", targets)
	}
}

// The checks run before the proxy: an undeclared target, a declared name that
// resolves (even partly) to a forbidden address, and a proxy that refuses the
// CONNECT are all refused, and the first two never reach the proxy.
func TestProxiedBrokerChecksBeforeTheProxyAndReportsItsRefusal(t *testing.T) {
	var fp *fakeForwardProxy
	r := newRig(t, []string{"allowed.test", "rebind.test", "mixed.test"}, func(r *rig, o *Options) {
		fp = newFakeForwardProxy(t, r.echoAddr, http.StatusOK)
		withProxy(fp, "http://proxy.corp:3128", "")(r, o)
	})
	for target, want := range map[string]int{
		"evil.test:443":    http.StatusForbidden, // not declared
		"rebind.test:443":  http.StatusForbidden, // resolves to loopback
		"mixed.test:443":   http.StatusForbidden, // one answer is loopback; the proxy would choose
		"203.0.113.10:443": http.StatusForbidden, // literal of a declared name
	} {
		if code, _, _ := r.connect(t, target); code != want {
			t.Errorf("CONNECT %s: status %d, want %d", target, code, want)
		}
	}
	if targets, _ := fp.seen(); len(targets) != 0 {
		t.Fatalf("refused targets reached the proxy: %v", targets)
	}

	refusing := newRig(t, []string{"allowed.test"}, func(r *rig, o *Options) {
		withProxy(newFakeForwardProxy(t, r.echoAddr, http.StatusForbidden), "http://proxy.corp:3128", "")(r, o)
	})
	if code, _, _ := refusing.connect(t, "allowed.test:443"); code != http.StatusBadGateway {
		t.Fatalf("a proxy refusal must answer 502, got %d", code)
	}
	if _, refused := refusing.session.Counts(); refused != 1 {
		t.Fatalf("refused = %d, want 1", refused)
	}
}

// The production proxy function reads the kernel's environment.
func TestEnvironmentProxyHonoursNoProxy(t *testing.T) {
	// http.ProxyFromEnvironment caches the environment once per process, so
	// this only checks the target shape it is asked about: a loopback target
	// is never proxied, whatever the environment says.
	u, err := EnvironmentProxy("127.0.0.1:443")
	if err != nil || u != nil {
		t.Fatalf("loopback target proxied: %v %v", u, err)
	}
}
