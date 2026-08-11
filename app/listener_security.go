package app

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/cambrian-sh/core/internal/config"
)

// Operator-plane transport security (SEC-03).
//
// The plane already authenticates — a bearer token, checked by a method-scoped
// interceptor (ADR-0047 D13). What it had no notion of was TRANSPORT: the server
// was built with no credentials at all and the listener bound ":port", which is
// 0.0.0.0. So the operator plane was reachable from any host on the network, in
// plaintext, and nothing in the config said so.
//
// Two changes, and the second is the one that matters. TLS becomes available; and
// plaintext becomes impossible to serve to a non-loopback address by accident.
// A token over an unencrypted link on a shared network is a token somebody else
// has, so authentication without transport security is a weaker guarantee than it
// appears — which is exactly the sort of claim this project has been careful not
// to make elsewhere.

// defaultBindAddress is LOOPBACK. The zero-config edge profile wants localhost,
// and a deployment that genuinely serves other machines should have to say so.
const defaultBindAddress = "127.0.0.1"

// listenAddress resolves the interface:port the gRPC plane binds.
func listenAddress(cfg *config.Config) string {
	bind := strings.TrimSpace(cfg.Server.BindAddress)
	if bind == "" {
		bind = defaultBindAddress
	}
	return net.JoinHostPort(bind, cfg.Server.Port)
}

// isLoopback reports whether an address is unreachable from another host.
//
// An empty bind or "0.0.0.0"/"::" is emphatically NOT loopback — those are every
// interface, which is the case this whole file exists to catch.
func isLoopback(bind string) bool {
	b := strings.TrimSpace(bind)
	switch b {
	case "":
		// Empty means the default, which is loopback.
		return true
	case "0.0.0.0", "::", "*":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(b); ip != nil {
		return ip.IsLoopback()
	}
	// A hostname we cannot classify is treated as routable: the fail-closed
	// direction. Being wrong here costs a config flag; being wrong the other way
	// costs an unencrypted operator plane.
	return false
}

// modeTLSPlusPlaintextLoopback: TLS is configured but the bind is loopback —
// the one port serves TLS and plaintext simultaneously via secureListener's
// first-byte demux, and gRPC itself runs credential-less.
const modeTLSPlusPlaintextLoopback = "tls+plaintext-loopback"

// transportCredentials returns the gRPC server options carrying TLS, and refuses
// a configuration that would serve plaintext to the network.
//
// The rules, in order:
//
//  1. Cert AND key set, loopback bind → listener-level TLS+plaintext demux
//     (modeTLSPlusPlaintextLoopback): local agents keep their insecure channel
//     while a TLS-only forwarder (cloudflared http2Origin) gets a TLS origin.
//  1b. Cert AND key set, routable bind → pure TLS. Whatever the bind is, this is safe.
//  2. Exactly one of them set → ERROR. A half-configured pair silently degrading
//     to plaintext is the failure mode that makes people believe they have TLS.
//  3. No TLS, loopback bind → plaintext, allowed. This is the edge profile and it
//     needs zero configuration.
//  4. No TLS, routable bind, InsecureLocalhost → plaintext, allowed, and said out
//     loud so it appears in the boot log of the deployment that chose it.
//  5. No TLS, routable bind, no opt-in → ERROR at boot.
func transportCredentials(cfg *config.Config) ([]grpc.ServerOption, string, error) {
	cert, key := strings.TrimSpace(cfg.Server.TLSCertFile), strings.TrimSpace(cfg.Server.TLSKeyFile)

	if (cert == "") != (key == "") {
		return nil, "", fmt.Errorf(
			"server: tls_cert_file and tls_key_file must be set together (got cert=%q key=%q); "+
				"one without the other would silently serve plaintext", cert, key)
	}

	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, "", fmt.Errorf("server: load TLS keypair: %w", err)
		}
		if isLoopback(cfg.Server.BindAddress) {
			// TLS on a LOOPBACK bind serves BOTH protocols via a listener-level
			// demux (secureListener), so no grpc.Creds here. Rationale: the cert
			// exists to satisfy a TLS-only local forwarder (cloudflared's
			// http2Origin has no h2c), but the Python agent plane dials the same
			// port with an insecure channel — a TLS-only listener silently
			// severs every agent's callback into the kernel. Loopback-only by
			// construction, so this can never relax a network-exposed listener.
			return nil, modeTLSPlusPlaintextLoopback, nil
		}
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		})
		return []grpc.ServerOption{grpc.Creds(creds)}, "tls", nil
	}

	if isLoopback(cfg.Server.BindAddress) {
		return nil, "plaintext-loopback", nil
	}

	if cfg.Server.InsecureLocalhost {
		return nil, "plaintext-insecure-optin", nil
	}

	return nil, "", fmt.Errorf(
		"server: refusing to serve the operator plane in PLAINTEXT on a non-loopback "+
			"address (bind_address=%q). The plane carries bearer tokens, and a token on an "+
			"unencrypted link is a token anyone on the path has. Fix one of: set "+
			"server.tls_cert_file + server.tls_key_file; bind 127.0.0.1; or, if this host "+
			"is genuinely trusted, set server.insecure_localhost=true to say so explicitly",
		cfg.Server.BindAddress)
}

// secureListener applies the transport mode to the raw listener. For
// modeTLSPlusPlaintextLoopback it returns a first-byte demultiplexer serving
// BOTH TLS and plaintext h2c on the one loopback port; every other mode
// returns the listener unchanged (pure TLS rides grpc.Creds instead).
//
// NextProtos advertises h2: gRPC-over-TLS clients (and cloudflared's
// http2Origin) select HTTP/2 via ALPN — without it they downgrade and die.
func secureListener(lis net.Listener, cfg *config.Config, mode string) (net.Listener, error) {
	if mode != modeTLSPlusPlaintextLoopback {
		return lis, nil
	}
	pair, err := tls.LoadX509KeyPair(
		strings.TrimSpace(cfg.Server.TLSCertFile), strings.TrimSpace(cfg.Server.TLSKeyFile))
	if err != nil {
		return nil, fmt.Errorf("server: load TLS keypair: %w", err)
	}
	return &demuxListener{Listener: lis, tls: &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2"},
	}}, nil
}

// demuxListener peeks one byte per accepted connection: 0x16 is a TLS
// handshake record → wrap in tls.Server; anything else (an HTTP/2 preface
// starts with 'P') is plaintext h2c and passes through untouched.
type demuxListener struct {
	net.Listener
	tls *tls.Config
}

func (l *demuxListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc := &peekConn{Conn: c, r: bufio.NewReader(c)}
	// Bound the peek so a connect-and-say-nothing client cannot wedge the
	// accept loop (it is serial). Loopback-only, so the bound is generous.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	first, perr := pc.r.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if perr != nil {
		// Unreadable connection: hand it to gRPC as-is — it fails there with
		// normal per-conn accounting instead of killing the accept loop.
		return pc, nil
	}
	if first[0] == 0x16 {
		return tls.Server(pc, l.tls), nil
	}
	return pc, nil
}

// peekConn replays the buffered peeked byte(s) ahead of the raw connection.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekConn) Read(b []byte) (int, error) { return p.r.Read(b) }
