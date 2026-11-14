package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// errStopBeforeConnect lets a test observe WHAT would have been dialed without
// opening a socket. The address passed to the dial function is the assertion.
var errStopBeforeConnect = errors.New("test: stopped before connecting")

// ADR-0115 Phase 2 / ADR-0153 — the network boundary, tested adversarially.
// Moved from cambrian-premium/sourcediscovery with the guard (ENG-1419).

// strictPolicy is https-only, default ports, no private space.
func strictPolicy() Policy { return Policy{Version: "test"} }

//
// Every case here is written as the attack it prevents rather than the rule it
// implements, so a future reader sees what breaks if the check is removed.

func TestBlockedAddressRanges(t *testing.T) {
	p := strictPolicy()
	for _, tc := range []struct {
		name string
		addr string
	}{
		{"the cloud metadata service", "169.254.169.254"},
		{"link-local generally, so an allowlist of one address cannot be routed around", "169.254.1.1"},
		{"IPv6 link-local", "fe80::1"},
		{"loopback", "127.0.0.1"},
		{"loopback by another name", "127.5.5.5"},
		{"IPv6 loopback", "::1"},
		{"private class A", "10.0.0.1"},
		{"private class B", "172.16.0.1"},
		{"private class C", "192.168.1.1"},
		{"unique-local IPv6", "fd00::1"},
		{"unspecified", "0.0.0.0"},
		{"carrier-grade NAT", "100.64.0.1"},
		{"multicast", "224.0.0.1"},
		{"NAT64, which reaches IPv4 space through an IPv6 address", "64:ff9b::a00:1"},
		{"reserved space", "240.0.0.1"},

		// The classic bypass: an IPv4-mapped IPv6 address answers false to the
		// IPv4 predicates unless it is unmapped first.
		{"IPv4-mapped IPv6 metadata service", "::ffff:169.254.169.254"},
		{"IPv4-mapped IPv6 loopback", "::ffff:127.0.0.1"},
		{"IPv4-mapped IPv6 private", "::ffff:10.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			if err := p.CheckAddr(addr); !errors.Is(err, ErrBlockedAddr) {
				t.Errorf("%s must be refused, got %v", tc.addr, err)
			}
		})
	}
}

func TestPublicAddressesAreAllowed(t *testing.T) {
	p := strictPolicy()
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700::1111"} {
		if err := p.CheckAddr(netip.MustParseAddr(a)); err != nil {
			t.Errorf("%s should be reachable: %v", a, err)
		}
	}
}

func TestURLShapeRefusals(t *testing.T) {
	p := strictPolicy()
	for _, tc := range []struct {
		name string
		raw  string
		want error
	}{
		{"file scheme", "file:///etc/passwd", ErrScheme},
		{"gopher, the classic SSRF pivot", "gopher://example.com/", ErrScheme},
		{"cleartext without an explicit decision", "http://example.com/", ErrScheme},
		{"credentials smuggled in the URL", "https://user:pass@example.com/", ErrUserInfo},
		{"a management port", "https://example.com:9200/", ErrPort},
		{"another management port", "https://example.com:2375/", ErrPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.CheckURL(tc.raw); !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestCleartextIsAnExplicitDecision(t *testing.T) {
	p := strictPolicy()
	if _, err := p.CheckURL("http://example.com/"); err == nil {
		t.Fatal("cleartext must be refused by default")
	}
	p.AllowHTTP = true
	if _, err := p.CheckURL("http://example.com/"); err != nil {
		t.Errorf("an operator who allowed cleartext should get it: %v", err)
	}
}

// ---- The rebinding window -----------------------------------------------

// hostileResolver answers differently each time it is asked, which is exactly
// what a resolver under an attacker's control does.
type hostileResolver struct{ calls int }

func (h *hostileResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	h.calls++
	if h.calls == 1 {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
}

func TestDialerResolvesOnceAndDialsWhatItValidated(t *testing.T) {
	// The property: there is no second lookup for anyone to poison. If the
	// dialer re-resolved at connect time, the hostile second answer would be
	// the address actually dialed.
	res := &hostileResolver{}
	var dialedTo string
	d := &Dialer{
		Policy: strictPolicy(), Resolver: res,
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialedTo = addr
			return nil, errStopBeforeConnect
		},
	}
	_, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, errStopBeforeConnect) {
		t.Fatalf("expected to reach the dial step, got %v", err)
	}
	if dialedTo != "93.184.216.34:443" {
		t.Errorf("dialed %q — it must dial the address it validated, by IP", dialedTo)
	}
	if res.calls != 1 {
		t.Errorf("resolver called %d times; a second lookup is a rebinding window", res.calls)
	}
}

// rebindResolver returns a public address and a metadata address together,
// which is the "any permitted one exists" trap.
type rebindResolver struct{}

func (rebindResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return []netip.Addr{
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("93.184.216.34"),
	}, nil
}

func TestEveryResolvedAddressIsValidated(t *testing.T) {
	// A host resolving to one blocked and one public address must never have
	// the blocked one dialed. Checking "does a permitted address exist" would
	// pass this and then dial whatever the client picked.
	var dialedTo string
	d := &Dialer{
		Policy: strictPolicy(), Resolver: rebindResolver{},
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialedTo = addr
			return nil, errStopBeforeConnect
		},
	}
	_, _ = d.DialContext(context.Background(), "tcp", "example.com:443")
	if dialedTo != "93.184.216.34:443" {
		t.Errorf("dialed %q — the blocked address must be skipped, not chosen", dialedTo)
	}
}

func TestLiteralAddressesSkipResolutionButNotValidation(t *testing.T) {
	// `https://10.0.0.1/` has no DNS step and is exactly as forbidden.
	d := &Dialer{Policy: strictPolicy(), Resolver: &hostileResolver{}}
	_, err := d.DialContext(context.Background(), "tcp", "10.0.0.1:443")
	if !errors.Is(err, ErrBlockedAddr) {
		t.Errorf("want ErrBlockedAddr, got %v", err)
	}
	_, err = d.DialContext(context.Background(), "tcp", "[::ffff:169.254.169.254]:443")
	if !errors.Is(err, ErrBlockedAddr) {
		t.Errorf("mapped literal: want ErrBlockedAddr, got %v", err)
	}
}

// ---- Endpoints (ENG-1419: MCP http/sse, ingress transports) ----------------

func TestCheckEndpointURL_RefusesPrivateSpaceUnlessOptedIn(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/admin", "http://192.168.1.10:8080/",
		"http://127.0.0.1:9000/", "http://localhost:3006/mcp", "http://[::1]/", "http://[fd00::1]/", "http://100.64.0.1/",
	} {
		if err := CheckEndpointURL(raw, false); !errors.Is(err, ErrBlockedAddr) {
			t.Errorf("%s: err = %v, want ErrBlockedAddr", raw, err)
		}
		if err := CheckEndpointURL(raw, true); err != nil {
			t.Errorf("%s with opt-in: %v", raw, err)
		}
	}
	for _, raw := range []string{"ftp://example.com/x", "http://user:pw@example.com/", "file:///etc/passwd", "http:///nohost"} {
		if err := CheckEndpointURL(raw, true); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	for _, raw := range []string{"https://api.example.com/v1", "http://mcp.example.com:8080/mcp"} {
		if err := CheckEndpointURL(raw, false); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://0.0.0.0/", "http://224.0.0.1/"} {
		if err := CheckEndpointURL(raw, true); !errors.Is(err, ErrBlockedAddr) {
			t.Errorf("%s with opt-in: err = %v, want ErrBlockedAddr", raw, err)
		}
	}
}

func TestNewClient_DialRefusesLoopbackUnlessOptedIn(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	if _, err := NewClient(EndpointPolicy(false), 5*time.Second, nil).Get(upstream.URL); !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("strict client reached loopback: err = %v", err)
	}
	resp, err := NewClient(EndpointPolicy(true), 0, nil).Get(upstream.URL)
	if err != nil {
		t.Fatalf("opted-in client: %v", err)
	}
	resp.Body.Close()
}

func TestNewClient_RedirectStripsCredentialAndRevalidates(t *testing.T) {
	t.Parallel()
	var sawAuth []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/to-private" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	}))
	defer redirector.Close()
	req, _ := http.NewRequest(http.MethodGet, redirector.URL+"/go", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := NewClient(EndpointPolicy(true), 5*time.Second, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(sawAuth) != 1 || sawAuth[0] != "" {
		t.Fatalf("credential crossed the redirect: %q", sawAuth)
	}
	loopbackOnly := NewClient(Policy{Version: "t", AllowHTTP: true, AllowAnyPort: true, AllowLoopback: true}, 5*time.Second, nil)
	if _, err := loopbackOnly.Get(redirector.URL + "/to-private"); !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("redirect into link-local space was followed: err = %v", err)
	}
}

func TestDialer_BeforeLookupCanRefuseAndOnDialRecords(t *testing.T) {
	t.Parallel()
	refused := errors.New("budget")
	d := &Dialer{Policy: strictPolicy(), Resolver: &hostileResolver{}, BeforeLookup: func() error { return refused }}
	if _, err := d.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the BeforeLookup refusal", err)
	}
	var gotHost string
	var gotAddr netip.Addr
	d = &Dialer{Policy: strictPolicy(), Resolver: &hostileResolver{},
		Dial:   func(_ context.Context, _, _ string) (net.Conn, error) { c, _ := net.Pipe(); return c, nil },
		OnDial: func(h string, a netip.Addr) { gotHost, gotAddr = h, a }}
	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if gotHost != "example.com" || gotAddr.String() != "93.184.216.34" {
		t.Fatalf("OnDial = %q %v", gotHost, gotAddr)
	}
}
