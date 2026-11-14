// Package egress is the guarded outbound-connection layer: the one place that
// decides whether the kernel (or a premium plugin) may open a connection to a
// host an operator, a spec, a pipeline node, an MCP server entry or a model
// named — and that dials it so the decision cannot be bypassed (ADR-0153;
// decisions/single-guarded-egress-client.md).
//
// It moved here from cambrian-premium/sourcediscovery on the owner's decision
// of 2026-11-14 (ENG-1419): the OSS MCP http/sse transports needed the same
// guard and core cannot import premium, so the guard became an OSS package both
// modules use. It lives under pkg/ (not internal/) precisely so premium can
// import it.
//
// Consumers (keep this list true):
//   - core MCP http/sse transports, shared and per-user sessions
//     (internal/infrastructure/mcp: connector.go, peruser.go), under
//     EndpointPolicy with the server entry's allow_private_network opt-in;
//   - core tool URL policy (domain.ToolResourcePolicy.AllowsURL: CheckAddr for
//     IP literals — the one definition of a non-public address);
//   - premium sourcediscovery: the discovery prober (budgeted session, origin
//     set) and, through it, the ingress-studio poller, history backfill,
//     websocket transport and spec validation.
//
// The defence, in one line: resolve once, validate EVERY answer, dial the
// validated address by IP, verify the connected peer, re-validate every
// redirect hop and strip credentials on it, and never use an environment proxy
// (a proxy resolves the host itself, outside every check). Loopback, private,
// link-local, CGNAT and unique-local space is reachable only through an
// explicit per-endpoint opt-in (Policy.AllowPrivate).
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// Why address checks and not hostname allowlists.
//
// Arbitrary sources cannot be allowlisted — the operator names the source. So
// the defence is: resolve, validate every resulting address, then DIAL THE
// VALIDATED ADDRESS. "Resolve, check, then let the default client dial the
// hostname" re-resolves at connect time and leaves a DNS-rebinding window
// between the check and the connection. See dialer.go.

// Refusals. Callers match on these with errors.Is, never on message text.
var (
	ErrScheme      = errors.New("egress: scheme not permitted")
	ErrUserInfo    = errors.New("egress: credentials in the URL are not permitted")
	ErrPort        = errors.New("egress: port not permitted")
	ErrBlockedAddr = errors.New("egress: address is not a permitted destination")
	ErrNoAddress   = errors.New("egress: host resolved to no permitted address")
)

// Policy is a pinned, versioned set of rules a connection is made under.
type Policy struct {
	// Version identifies this policy for the record.
	Version string
	// AllowHTTP permits cleartext (http, ws). Default false.
	AllowHTTP bool
	// AllowedPorts, empty meaning the scheme defaults only (80, 443) unless
	// AllowAnyPort.
	AllowedPorts []int
	// AllowLoopback admits loopback only (tests, a deliberate sidecar). The most
	// dangerous single switch here; prefer AllowPrivate's explicit opt-in.
	AllowLoopback bool
	// AllowPrivate is the explicit per-endpoint opt-in (an ingress's
	// allow_private_ranges, an MCP server's allow_private_network): loopback,
	// RFC1918/unique-local, CGNAT and link-local targets become dialable for that
	// ONE endpoint. Unspecified, multicast, documentation and reserved space stay
	// refused — nothing legitimate lives there.
	AllowPrivate bool
	// AllowAnyPort lifts the 80/443 default: an operator's endpoint may well sit
	// on :8080, and the port was never the control that mattered — the address
	// check is.
	AllowAnyPort bool
}

// CheckURL validates a URL's shape before anything is resolved: scheme among
// http/https/ws/wss (cleartext only with AllowHTTP), no userinfo, a host, a
// permitted port. Shape first, because "scheme not permitted" beats a DNS
// failure for a file:// URL.
func (p Policy) CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	switch u.Scheme {
	case "https", "wss":
	case "http", "ws":
		if !p.AllowHTTP {
			return nil, fmt.Errorf("%w: %q requires an explicit operator decision to work in cleartext", ErrScheme, u.Scheme)
		}
	default:
		return nil, fmt.Errorf("%w: %q (only http, https, ws and wss are understood)", ErrScheme, u.Scheme)
	}
	// Userinfo carries a credential in a URL, where it would be recorded,
	// logged and shown to an operator. Credentials travel in headers or not at
	// all.
	if u.User != nil {
		return nil, ErrUserInfo
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress: url has no host")
	}
	if port := u.Port(); port != "" {
		if err := p.checkPort(port); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (p Policy) checkPort(port string) error {
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		return fmt.Errorf("%w: %q is not a port", ErrPort, port)
	}
	if p.AllowAnyPort {
		if n < 1 || n > 65535 {
			return fmt.Errorf("%w: %d", ErrPort, n)
		}
		return nil
	}
	if len(p.AllowedPorts) == 0 {
		if n == 80 || n == 443 {
			return nil
		}
		return fmt.Errorf("%w: %d (only 80 and 443 without an explicit port policy)", ErrPort, n)
	}
	for _, allowed := range p.AllowedPorts {
		if n == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: %d", ErrPort, n)
}

// CheckAddr decides whether one resolved address may be dialed.
//
// Every refused range has been used to reach something never meant to be
// reachable from a URL an outsider chose. The cloud metadata endpoint is the
// famous one, and it is why all of link-local is refused rather than just
// 169.254.169.254 — an allowlist of one address is a list somebody routes
// around.
func (p Policy) CheckAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlockedAddr)
	}
	// IPv4-mapped IPv6 (::ffff:169.254.169.254) is the classic bypass: it
	// answers false to the IPv4 predicates unless it is unmapped first.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	switch {
	case addr.IsLoopback():
		if p.AllowLoopback || p.AllowPrivate {
			return nil
		}
		return fmt.Errorf("%w: %s is loopback", ErrBlockedAddr, addr)
	case addr.IsUnspecified():
		return fmt.Errorf("%w: %s is unspecified", ErrBlockedAddr, addr)
	case addr.IsPrivate():
		if p.AllowPrivate {
			return nil
		}
		return fmt.Errorf("%w: %s is a private address", ErrBlockedAddr, addr)
	case addr.IsLinkLocalUnicast():
		// 169.254.0.0/16 and fe80::/10 — the metadata service lives here.
		if p.AllowPrivate {
			return nil
		}
		return fmt.Errorf("%w: %s is link-local", ErrBlockedAddr, addr)
	case addr.IsLinkLocalMulticast():
		return fmt.Errorf("%w: %s is link-local multicast", ErrBlockedAddr, addr)
	case addr.IsMulticast():
		return fmt.Errorf("%w: %s is multicast", ErrBlockedAddr, addr)
	case addr.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s is interface-local", ErrBlockedAddr, addr)
	}
	// Ranges the standard predicates miss. The private opt-in admits the ones
	// that are genuinely internal address space (CGNAT, unique-local); the rest
	// are documentation and reserved blocks nothing real answers from.
	for _, blocked := range extraBlocked {
		if blocked.Contains(addr) {
			if p.AllowPrivate && (blocked == cgnatPrefix || blocked == uniqueLocalPrefix) {
				return nil
			}
			return fmt.Errorf("%w: %s is in reserved range %s", ErrBlockedAddr, addr, blocked)
		}
	}
	return nil
}

var (
	cgnatPrefix       = netip.MustParsePrefix("100.64.0.0/10")
	uniqueLocalPrefix = netip.MustParsePrefix("fc00::/7")
)

// extraBlocked names ranges Go's predicates miss.
var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("::/128"),          // unspecified, belt and braces
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 — reaches IPv4 space
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("fc00::/7"),        // unique-local
}

// EndpointPolicyVersion identifies the endpoint egress rules for the record.
const EndpointPolicyVersion = "endpoint-egress-v1"

// EndpointPolicy is what a long-lived, operator-declared endpoint dials under —
// an ingress poller or websocket, an MCP http/sse server. Cleartext and any
// port are allowed (the operator typed the URL; legacy http endpoints on :8080
// are real); what is NOT allowed without the explicit allowPrivate opt-in is
// loopback, RFC1918, link-local (cloud metadata), CGNAT and unique-local space.
func EndpointPolicy(allowPrivate bool) Policy {
	return Policy{
		Version:      EndpointPolicyVersion,
		AllowHTTP:    true,
		AllowAnyPort: true,
		AllowPrivate: allowPrivate,
	}
}

// CheckEndpointURL is the CONFIG-VALIDATION half of the guard: the refusal an
// operator sees at save time, before the first dial. It checks shape and, when
// the host is a literal address or a loopback name, the address — so
// http://169.254.169.254/ and http://localhost:9000/ are refused without the
// opt-in. A hostname is NOT resolved here (a validator that does DNS fails on an
// offline laptop); the dial-time check refuses the resolved address anyway.
// Errors wrap ErrBlockedAddr / ErrScheme / ErrUserInfo / ErrPort; callers add
// their own opt-in hint.
func CheckEndpointURL(raw string, allowPrivate bool) error {
	p := EndpointPolicy(allowPrivate)
	u, err := p.CheckURL(raw)
	if err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if !allowPrivate {
			return fmt.Errorf("%w: %s is loopback", ErrBlockedAddr, host)
		}
		return nil
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return p.CheckAddr(ip)
	}
	return nil
}
