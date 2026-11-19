package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
)

// fakeResolver answers fixed addresses per name.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	a, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return a, nil
}

// forwardProxy is a minimal HTTP forward proxy: it answers an absolute-URI request
// itself (it never reaches the target) and counts what it was asked for.
type forwardProxy struct {
	hits atomic.Int32
	last atomic.Value // string: the last target URL
}

func (p *forwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	p.last.Store(r.URL.String())
	_, _ = io.WriteString(w, "via proxy")
}

// ENG-1545: with a forward proxy configured, a request whose target passes the
// policy goes through the proxy (dialed although it sits on loopback); a target
// with ANY forbidden address, or a forbidden literal, is refused before the proxy
// sees it; a request the proxy function sends direct still meets the guarded
// dialer; and a nil proxy is the plain guarded transport.
func TestProxiedTransport_ChecksTheTargetBeforeTheProxy(t *testing.T) {
	fp := &forwardProxy{}
	ps := httptest.NewServer(fp)
	defer ps.Close()
	proxyURL, _ := url.Parse(ps.URL)

	res := fakeResolver{
		"public.test": {netip.MustParseAddr("93.184.216.34")},
		// One public and one private answer: the proxy, not us, would pick.
		"split.test":  {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.7")},
		"direct.test": {netip.MustParseAddr("127.0.0.1")},
	}
	proxy := func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "direct.test" { // a NO_PROXY entry
			return nil, nil
		}
		return proxyURL, nil
	}
	client := &http.Client{Transport: NewProxiedTransport(EndpointPolicy(false), res, proxy)}

	resp, err := client.Get("http://public.test/tools")
	if err != nil {
		t.Fatalf("a public target through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via proxy" || fp.hits.Load() != 1 || fp.last.Load() != "http://public.test/tools" {
		t.Fatalf("proxy not used: body=%q hits=%d last=%v", body, fp.hits.Load(), fp.last.Load())
	}

	for _, target := range []string{"http://split.test/", "http://10.0.0.5/", "http://169.254.169.254/latest/meta-data"} {
		if _, err := client.Get(target); !errors.Is(err, ErrBlockedAddr) {
			t.Fatalf("%s: err = %v, want ErrBlockedAddr before the proxy", target, err)
		}
	}
	if n := fp.hits.Load(); n != 1 {
		t.Fatalf("a refused target reached the proxy (%d hits)", n)
	}

	// Sent direct: the guarded dialer refuses loopback without the opt-in.
	if _, err := client.Get("http://direct.test/"); !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("direct request bypassed the guard: %v", err)
	}

	if tr := NewProxiedTransport(EndpointPolicy(false), res, nil); tr.Proxy != nil {
		t.Fatal("a nil proxy must leave the guarded transport proxy-less")
	}
}

// The private-network opt-in still decides for a proxied target.
func TestProxiedTransport_PrivateTargetNeedsTheOptIn(t *testing.T) {
	fp := &forwardProxy{}
	ps := httptest.NewServer(fp)
	defer ps.Close()
	proxyURL, _ := url.Parse(ps.URL)
	proxy := func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	res := fakeResolver{"intranet.test": {netip.MustParseAddr("10.1.2.3")}}

	closed := &http.Client{Transport: NewProxiedTransport(EndpointPolicy(false), res, proxy)}
	if _, err := closed.Get("http://intranet.test/mcp"); !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("private target without the opt-in: %v", err)
	}
	open := &http.Client{Transport: NewProxiedTransport(EndpointPolicy(true), res, proxy)}
	resp, err := open.Get("http://intranet.test/mcp")
	if err != nil {
		t.Fatalf("private target with the opt-in: %v", err)
	}
	resp.Body.Close()
	if fp.hits.Load() != 1 {
		t.Fatalf("proxy hits = %d, want 1", fp.hits.Load())
	}
}

func TestProxyAddr_DefaultPorts(t *testing.T) {
	for raw, want := range map[string]string{
		"http://proxy.corp":         "proxy.corp:80",
		"https://proxy.corp":        "proxy.corp:443",
		"socks5://proxy.corp":       "proxy.corp:1080",
		"http://proxy.corp:3128":    "proxy.corp:3128",
		"http://[2001:db8::1]:8080": "[2001:db8::1]:8080",
	} {
		u, _ := url.Parse(raw)
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %s, want %s", raw, got, want)
		}
	}
}
