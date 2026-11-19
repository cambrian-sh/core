package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

// Forward-proxy support (ENG-1545, owner decision 2026-11-19: guarded MCP egress
// honours HTTP(S)_PROXY / NO_PROXY, for enterprise networks where the internet is
// reachable only through a proxy).
//
// The plain guarded transport has no proxy, because a proxy resolves the host
// itself — outside every check this package makes. NewProxiedTransport keeps the
// checks by moving them in FRONT of the proxy, as the kernel's ENG-688 allowlist
// broker does for sandboxed launches: before a request is handed to a proxy, its
// TARGET must pass the policy (scheme, port, and every address the name resolves
// to). Every address, not the first permitted one as the direct dialer takes:
// here the proxy, not this process, chooses which answer to dial, so one
// forbidden answer refuses the request. The connection to the proxy itself is
// dialed plainly — an operator-configured forward proxy usually sits on the
// private network the policy forbids for targets. A request the proxy function
// sends direct (NO_PROXY, no proxy configured; loopback is never proxied by
// http.ProxyFromEnvironment) goes through the guarded dialer exactly as before.
//
// Residual, stated: a resolver that answers the check differently from how the
// proxy later resolves (DNS rebinding against the proxy's resolver) is outside
// what a client can see. An operator who sets a proxy delegates final resolution
// to it.

// ProxyFunc chooses a proxy for a request (nil URL = direct), the shape of
// http.Transport.Proxy; http.ProxyFromEnvironment is the production one.
type ProxyFunc func(*http.Request) (*url.URL, error)

// NewProxiedTransport is NewTransport with a forward proxy chosen per request by
// proxy. A nil proxy is NewTransport unchanged.
func NewProxiedTransport(p Policy, resolver Resolver, proxy ProxyFunc) *http.Transport {
	t := NewTransport(p, resolver)
	if proxy == nil {
		return t
	}
	pt := &proxiedTransport{policy: p, resolver: resolver, proxy: proxy, guarded: t.DialContext}
	t.Proxy = pt.chooseProxy
	t.DialContext = pt.dial
	return t
}

type proxiedTransport struct {
	policy   Policy
	resolver Resolver
	proxy    ProxyFunc
	guarded  func(ctx context.Context, network, addr string) (net.Conn, error)

	// proxies holds the host:port of every proxy chooseProxy returned: the only
	// addresses dial connects to without the guard.
	proxies sync.Map
}

// chooseProxy asks the proxy function and, when it names a proxy, validates the
// request's target under the policy first.
func (t *proxiedTransport) chooseProxy(req *http.Request) (*url.URL, error) {
	pu, err := t.proxy(req)
	if err != nil || pu == nil {
		return nil, err
	}
	if err := t.checkTarget(req.Context(), req.URL); err != nil {
		return nil, err
	}
	t.proxies.Store(proxyAddr(pu), struct{}{})
	return pu, nil
}

// checkTarget is the policy check a proxied request passes before it leaves:
// the URL (scheme, port), then every address its host resolves to.
func (t *proxiedTransport) checkTarget(ctx context.Context, u *url.URL) error {
	if _, err := t.policy.CheckURL(u.String()); err != nil {
		return err
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return t.policy.CheckAddr(ip)
	}
	resolver := t.resolver
	if resolver == nil {
		resolver = netResolver{}
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s", ErrNoAddress, host)
	}
	for _, ip := range addrs {
		if err := t.policy.CheckAddr(ip); err != nil {
			return fmt.Errorf("egress: %s resolves to a forbidden address, refused before the proxy: %w", host, err)
		}
	}
	return nil
}

// dial connects to a proxy chooseProxy named without the guard, and to anything
// else through it.
func (t *proxiedTransport) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if _, ok := t.proxies.Load(addr); ok {
		d := net.Dialer{Timeout: 15 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	return t.guarded(ctx, network, addr)
}

// proxyAddr is the host:port http.Transport dials for a proxy URL (its default
// port by scheme when the URL names none).
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
