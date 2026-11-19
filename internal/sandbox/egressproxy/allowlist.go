// Package egressproxy is the kernel-owned egress allowlist proxy for
// contained MCP servers (ENG-688, ADR-0139 D8; owner decision 2026-11-19:
// "proxy for all runners").
//
// A server that declares network: egress with a host list runs with no
// network of its own (docker: a --network none namespace; bubblewrap:
// --unshare-net). Inside that namespace a forwarder listens on loopback and
// relays every connection, over one framed multiplex (mux.go) carried by a
// pipe the kernel holds, to a Session of this package's Broker. The Session
// speaks HTTP CONNECT and admits only the server's declared hosts: anything
// else — an undeclared name, an IP literal, a name that resolves to a
// loopback, link-local or unspecified address — is refused. There is no TCP
// listener on the host: nothing but the launch that owns the pipe can reach
// the proxy.
//
// Imports: the standard library only (internal/sandbox imports it; so does the
// in-sandbox forwarder, which is the kernel binary re-executed).
package egressproxy

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultPort is the port an entry without one admits (HTTPS).
const DefaultPort = 443

// entry is one parsed allowlist entry.
type entry struct {
	host     string // lower-case, no trailing dot; the suffix (without "*.") for a wildcard
	wildcard bool   // "*.example.com": any name strictly below example.com
	ip       net.IP // an IP literal entry (admits exactly that address)
	port     int
}

// Allowlist is a parsed host list. The zero value admits nothing.
type Allowlist struct {
	entries []entry
	raw     []string
}

// Classify reports what a declared host list means for enforcement:
//
//   - enforceable: every entry is a host pattern ParseAllowlist accepts;
//   - any: an entry is "*" (the server needs arbitrary hosts — fetch,
//     playwright): there is nothing to filter;
//   - placeholder: an entry is free text standing for a configured value
//     ("the configured database host"): the list cannot be enforced until the
//     value is resolved (named in the result).
func Classify(hosts []string) (enforceable bool, any bool, placeholder string) {
	if len(hosts) == 0 {
		return false, false, ""
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "*" {
			return false, true, ""
		}
		if _, err := parseEntry(h); err != nil {
			return false, false, h
		}
	}
	return true, false, ""
}

// ParseAllowlist parses a declared host list. Accepted entries:
//
//	api.example.com          exactly that name, port 443
//	api.example.com:8443     exactly that name and port
//	*.example.com            any name strictly below example.com (one leading label only)
//	203.0.113.7[:port]       exactly that IPv4 address
//	[2001:db8::1][:port]     exactly that IPv6 address
//
// "*" alone and free text are errors (Classify says why a list is not
// enforceable).
func ParseAllowlist(hosts []string) (*Allowlist, error) {
	a := &Allowlist{}
	for _, h := range hosts {
		e, err := parseEntry(strings.TrimSpace(h))
		if err != nil {
			return nil, err
		}
		a.entries = append(a.entries, e)
		a.raw = append(a.raw, strings.TrimSpace(h))
	}
	return a, nil
}

// Hosts is the list as declared.
func (a *Allowlist) Hosts() []string { return append([]string(nil), a.raw...) }

func parseEntry(s string) (entry, error) {
	if s == "" {
		return entry{}, fmt.Errorf("egress host entry is empty")
	}
	host, port := s, DefaultPort
	if strings.HasPrefix(s, "[") {
		// [v6] or [v6]:port
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return entry{}, fmt.Errorf("egress host %q: unterminated [", s)
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return entry{}, fmt.Errorf("egress host %q: junk after ]", s)
			}
			p, err := parsePort(rest[1:])
			if err != nil {
				return entry{}, fmt.Errorf("egress host %q: %w", s, err)
			}
			port = p
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() != nil {
			return entry{}, fmt.Errorf("egress host %q: not an IPv6 address", s)
		}
		return entry{ip: ip, port: port}, nil
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		if strings.Count(s, ":") > 1 {
			// A bare IPv6 literal without brackets.
			if ip := net.ParseIP(s); ip != nil {
				return entry{ip: ip, port: DefaultPort}, nil
			}
			return entry{}, fmt.Errorf("egress host %q: write an IPv6 address as [addr]:port", s)
		}
		p, err := parsePort(s[i+1:])
		if err != nil {
			return entry{}, fmt.Errorf("egress host %q: %w", s, err)
		}
		host, port = s[:i], p
	}
	if ip := net.ParseIP(host); ip != nil {
		return entry{ip: ip, port: port}, nil
	}
	wild := false
	if strings.HasPrefix(host, "*.") {
		wild, host = true, host[2:]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !validName(host) {
		return entry{}, fmt.Errorf("egress host %q is not a host name, *.domain, or IP address", s)
	}
	if wild && !strings.Contains(host, ".") {
		// "*.com" would admit a whole top-level domain.
		return entry{}, fmt.Errorf("egress host %q: a wildcard needs at least two labels after *.", s)
	}
	return entry{host: host, wildcard: wild, port: port}, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("bad port %q", s)
	}
	return p, nil
}

// validName: dot-separated labels of [a-z0-9-], each 1–63 long, not starting
// or ending with '-', at most 253 in all.
func validName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// match reports whether host:port is admitted, and whether it was admitted as
// an IP literal entry (the dial then goes to exactly that address).
func (a *Allowlist) match(host string, port int) (admitted bool, literal net.IP) {
	if a == nil {
		return false, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		// An IP literal is admitted only by an entry naming that address: a
		// name entry never admits its own resolved address given as a literal
		// (the client would then pick the address, the DNS rebinding bypass).
		for _, e := range a.entries {
			if e.ip != nil && e.ip.Equal(ip) && e.port == port {
				return true, e.ip
			}
		}
		return false, nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !validName(host) {
		return false, nil
	}
	for _, e := range a.entries {
		if e.ip != nil || e.port != port {
			continue
		}
		if e.wildcard {
			if strings.HasSuffix(host, "."+e.host) {
				return true, nil
			}
			continue
		}
		if host == e.host {
			return true, nil
		}
	}
	return false, nil
}
