package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Dial-time enforcement.
//
// The obvious implementation resolves the host, checks the addresses, and then
// hands the URL to an ordinary HTTP client — which resolves the host AGAIN when
// it dials. Between the two lookups the answer can change: a hostile resolver
// returns a public address to the check and 169.254.169.254 to the dial (DNS
// rebinding). So the dialer resolves ITSELF, validates every answer, dials a
// validated address by IP and verifies the peer it reached. There is no second
// lookup for anyone to poison.

// Resolver is the seam tests replace. nil ⇒ net.DefaultResolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type netResolver struct{}

func (netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// Dialer resolves, validates and dials by validated address.
type Dialer struct {
	Policy   Policy
	Resolver Resolver // nil ⇒ the system resolver
	// BeforeLookup, when set, runs before each DNS lookup and may refuse it
	// (premium discovery charges a session budget here).
	BeforeLookup func() error
	// Dial is the underlying dial; nil ⇒ a zero net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnDial, when set, records what was actually connected to.
	OnDial func(host string, addr netip.Addr)
}

// DialContext implements the DialContext shape http.Transport and websocket
// dialers take.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	// A literal address skips resolution but not validation: http://10.0.0.1/
	// has no DNS step and is exactly as forbidden.
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := d.Policy.CheckAddr(ip); err != nil {
			return nil, err
		}
		return d.connect(ctx, network, ip, port, host)
	}
	if d.BeforeLookup != nil {
		if err := d.BeforeLookup(); err != nil {
			return nil, err
		}
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = netResolver{}
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	// EVERY answer is validated and the first permitted one is dialed: a host
	// that resolves to one public and one private address must never have the
	// private one dialed, and a resolver returning both is precisely the attack.
	var firstErr error
	for _, ip := range addrs {
		if err := d.Policy.CheckAddr(ip); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return d.connect(ctx, network, ip, port, host)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("%w: %s", ErrNoAddress, host)
}

// connect dials a validated address and verifies the peer it reached.
func (d *Dialer) connect(ctx context.Context, network string, ip netip.Addr, port, host string) (net.Conn, error) {
	dial := d.Dial
	if dial == nil {
		var std net.Dialer
		dial = std.DialContext
	}
	conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, err
	}
	// Belt and braces against a dialer that redirects at a lower layer.
	if remote, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		if got, ok := netip.AddrFromSlice(remote.IP); ok {
			if err := d.Policy.CheckAddr(got); err != nil {
				conn.Close()
				return nil, fmt.Errorf("egress: connected peer failed validation: %w", err)
			}
		}
	}
	if d.OnDial != nil {
		d.OnDial(host, ip)
	}
	return conn, nil
}

// NewDialContext returns a guarded dial function for an http.Transport, a
// websocket handshake or anything else that takes a DialContext.
func NewDialContext(p Policy, resolver Resolver) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &Dialer{Policy: p, Resolver: resolver}
	return d.DialContext
}

// MaxRedirects bounds a redirect chain: what a sane endpoint needs (one hop to
// the canonical host) with margin.
const MaxRedirects = 3

// NewTransport is the guarded http.Transport: every connection through the
// guarded dialer, NO environment proxy (a proxy resolves the host itself,
// outside every check).
func NewTransport(p Policy, resolver Resolver) *http.Transport {
	return &http.Transport{
		DialContext:         NewDialContext(p, resolver),
		Proxy:               nil,
		MaxIdleConns:        8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// NewClient builds a guarded HTTP client: NewTransport, every redirect hop
// re-validated (CheckRedirect) and stripped of credentials, no cookie jar.
// timeout 0 means no client-wide timeout — required for long-lived streams
// (an MCP SSE GET) whose calls are bounded by their own contexts.
func NewClient(p Policy, timeout time.Duration, resolver Resolver) *http.Client {
	return &http.Client{
		Transport:     NewTransport(p, resolver),
		Jar:           nil,
		CheckRedirect: CheckRedirect(p),
		Timeout:       timeout,
	}
}

// CheckRedirect re-validates a redirect hop: at most MaxRedirects, the target
// must pass the policy, https must not downgrade, and credentials are stripped
// on EVERY hop (Go forwards them to exact and subdomain matches, so a source able
// to redirect within its own domain would receive them).
func CheckRedirect(p Policy) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return fmt.Errorf("egress: more than %d redirects", MaxRedirects)
		}
		u, err := p.CheckURL(req.URL.String())
		if err != nil {
			return err
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && u.Scheme != "https" {
			return fmt.Errorf("%w: redirect downgrades https to %s", ErrScheme, u.Scheme)
		}
		StripCredentials(req.Header)
		return nil
	}
}

// StripCredentials removes credential-bearing headers from a redirected request.
func StripCredentials(h http.Header) {
	h.Del("Authorization")
	h.Del("Cookie")
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-api") {
			h.Del(name)
		}
	}
}
