package app

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// agentTransport resolves how the kernel's OWN spawned agents reach the agent
// plane, from the same listener policy that binds it (ADR-0130 D3/D4;
// T-20261101-kernel-spawned-agents-dial-localhost-port-and).
//
// The metabolism stack used to hand every agent "localhost:<port>". That is
// right only on a loopback bind: on a routable bind nothing listens on
// localhost, and in pure-TLS mode a plaintext dial is refused. The rules, per
// transport mode (transportCredentials):
//
//   - loopback modes (plaintext-loopback, tls+plaintext-loopback): dial the
//     bound loopback address in PLAINTEXT. The demux keeps plaintext on
//     loopback precisely for local agents (ADR-0130 D2 keeps it), so no TLS
//     root is injected and nothing about local agents changes — except that
//     the exact bound address replaces "localhost", which on a dual-stack host
//     may resolve to the family the kernel did not bind.
//   - plaintext-insecure-optin (routable, no TLS, insecure_localhost): dial the
//     bound address; a wildcard bind is dialled on loopback, which it covers.
//   - tls (routable bind with cert+key): dial the bound address (loopback for
//     a wildcard bind) over TLS, injecting server.tls_cert_file as the root
//     (CAMBRIAN_TLS_CA) — the self-signed first-deployment case ADR-0130's
//     consequences name — and, when the certificate does not name the dialled
//     host, a server-name override taken from the certificate itself.
//
// The second return value is a warning for the boot log ("" when none).
func agentTransport(cfg *config.Config) (agentmgr.AgentTransport, string, error) {
	_, mode, err := transportCredentials(cfg)
	if err != nil {
		return agentmgr.AgentTransport{}, "", err
	}
	host := agentDialHost(cfg.Server.BindAddress)
	t := agentmgr.AgentTransport{Addr: net.JoinHostPort(host, cfg.Server.Port)}
	if mode != "tls" {
		return t, "", nil
	}
	certFile := strings.TrimSpace(cfg.Server.TLSCertFile)
	leaf, selfSigned, err := readLeafCert(certFile)
	if err != nil {
		return agentmgr.AgentTransport{}, "", fmt.Errorf("agent transport: %w", err)
	}
	t.TLSCAFile = certFile
	if leaf.VerifyHostname(host) != nil {
		t.TLSServerName = certServerName(leaf)
	}
	warn := ""
	if !selfSigned {
		// A CA-issued leaf is not a trust anchor: the SDK pins exactly the root
		// it is given, so verification needs the issuing root, which the kernel
		// has no config key for (follow-up ticket). Said at boot rather than
		// discovered as N agents failing their handshake.
		warn = "server.tls_cert_file is not self-signed; spawned agents are given it as their TLS root " +
			"and may fail verification unless the file also carries the issuing root"
	}
	return t, warn, nil
}

// agentDialHost maps the bind address to the host a local agent dials.
func agentDialHost(bind string) string {
	b := strings.TrimSpace(bind)
	switch b {
	case "", "localhost":
		// Empty is the loopback default (listenAddressOn); dial what was bound.
		return defaultBindAddress
	case "0.0.0.0", "*":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return b
}

// readLeafCert parses the first certificate in a PEM file and reports whether
// it is self-signed (its own issuer, verifiable by its own key).
func readLeafCert(path string) (*x509.Certificate, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, false, fmt.Errorf("%s: no PEM certificate", path)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	// CheckSignature, not CheckSignatureFrom: the latter also demands CA basic
	// constraints, which many self-signed server certs do not carry.
	selfSigned := bytes.Equal(leaf.RawIssuer, leaf.RawSubject) &&
		leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
	return leaf, selfSigned, nil
}

// certServerName picks a name the certificate actually covers, preferring a DNS
// SAN, then an IP SAN, then the legacy CN. "" when it names nothing usable.
func certServerName(c *x509.Certificate) string {
	for _, n := range c.DNSNames {
		if n != "" && !strings.HasPrefix(n, "*.") {
			return n
		}
	}
	if len(c.IPAddresses) > 0 {
		return c.IPAddresses[0].String()
	}
	return c.Subject.CommonName
}
