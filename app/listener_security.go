package app

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
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
//
// ADR-0130 D3 extended the same policy to every HTTP listener the kernel or a
// plugin opens (serveHTTP / ServeHTTPListener below), and D1 closed the other
// half of SEC-03 — client authentication on the agent plane
// (internal/authz/agent_auth.go).

// defaultBindAddress is LOOPBACK. The zero-config edge profile wants localhost,
// and a deployment that genuinely serves other machines should have to say so.
const defaultBindAddress = "127.0.0.1"

// listenAddress resolves the interface:port the gRPC plane binds.
func listenAddress(cfg *config.Config) string {
	return listenAddressOn(cfg, cfg.Server.Port)
}

// listenAddressOn resolves interface:port for a plane on its OWN port — the
// ADR-0126 MCP endpoint, the ADR-0028 ingestion server, a plugin's HTTP
// ingress. The interface is shared with the gRPC plane on purpose: a second
// listener that quietly bound a wider interface than the first would make the
// bind-address setting a half-truth.
func listenAddressOn(cfg *config.Config, port string) string {
	bind := strings.TrimSpace(cfg.Server.BindAddress)
	if bind == "" {
		bind = defaultBindAddress
	}
	return net.JoinHostPort(bind, port)
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
//     1b. Cert AND key set, routable bind → pure TLS. Whatever the bind is, this is safe.
//  2. Exactly one of them set → ERROR. A half-configured pair silently degrading
//     to plaintext is the failure mode that makes people believe they have TLS.
//  3. No TLS, loopback bind → plaintext, allowed. This is the edge profile and it
//     needs zero configuration.
//  4. No TLS, routable bind, InsecureLocalhost → plaintext, allowed, and said out
//     loud so it appears in the boot log of the deployment that chose it.
//  5. No TLS, routable bind, no opt-in → ERROR at boot.
//
// Note that TLS here is SERVER-side only: it protects the link, not the
// identity of the client. Who the client is, on the agent plane, is decided by
// the per-agent credential (ADR-0130 D1), which the loopback demux does not
// relax — only server.insecure_localhost does (D2).
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

// ─────────────────────────────────────────────────────────────────────────────
// One listener policy for HTTP as well (ADR-0130 D3; the operating rule
// decisions/all-listeners-share-the-grpc-listener-policy.md).
//
// The gRPC plane got the loopback default, the routable-bind refusal and TLS
// above. The three HTTP listeners the 2026-10-31 audit found — the ingestion
// and admin server, the premium chat ingress and the ingress-studio webhook
// receiver — each built their own http.Server from env or config and
// inherited none of it. Everything below exists so that a TCP listener,
// gRPC or HTTP, kernel or plugin, is built in exactly one place: the same
// bind address, the same transport decision, the same timeouts and the same
// body cap. An http.Server literal written anywhere else is a review blocker
// and app/listener_policy_test.go asserts it.
// ─────────────────────────────────────────────────────────────────────────────

const (
	// httpReadHeaderTimeout bounds how long a client may take to send its
	// headers — the Slowloris guard. Generous, because an agent on a loaded
	// host is not an attacker.
	httpReadHeaderTimeout = 10 * time.Second
	// httpIdleTimeout reclaims keep-alive connections nobody is using.
	httpIdleTimeout = 120 * time.Second
	// defaultHTTPMaxBodyBytes caps a request body on every policy-built HTTP
	// listener. 64 MiB matches the operator binary-ingest lane's gRPC limit,
	// so the documented upload path keeps working while a 1 GiB POST does not
	// get buffered.
	defaultHTTPMaxBodyBytes int64 = 64 << 20
)

// newHTTPServer is the ONE http.Server shape. No Addr: the listener is bound
// separately (listenAddressOn + secureListener), so a server can never bind an
// interface the policy did not resolve.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// limitBody wraps a handler so every request body reads through
// http.MaxBytesReader. A handler that decodes JSON straight from r.Body (as all
// three audited listeners did) otherwise has no cap at all.
func limitBody(next http.Handler, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		maxBytes = defaultHTTPMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// serveHTTP binds port on the kernel's bind address under the kernel's
// transport decision and serves handler on it, returning the running server
// (for shutdown) and the bound address (for the activation log line).
//
// The sequence is the gRPC plane's, in the same order: decide the transport
// BEFORE binding so a refusable configuration fails before the port is open;
// resolve the interface from server.bind_address; terminate TLS at the
// listener in the loopback demux mode, or with ServeTLS in pure-TLS mode.
// label names the listener in errors and logs.
func serveHTTP(cfg *config.Config, port string, handler http.Handler, label string) (*http.Server, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("%s: listener policy needs a config", label)
	}
	_, mode, err := transportCredentials(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", label, err)
	}
	addr := listenAddressOn(cfg, strings.TrimPrefix(strings.TrimSpace(port), ":"))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("%s (%s unavailable): %w", label, addr, err)
	}
	lis, err = secureListener(lis, cfg, mode)
	if err != nil {
		_ = lis.Close()
		return nil, "", fmt.Errorf("%s: %w", label, err)
	}
	// The BOUND address, not the requested one: a ":0" test port resolves here.
	addr = lis.Addr().String()
	srv := newHTTPServer(limitBody(handler, defaultHTTPMaxBodyBytes))
	go func() {
		var serveErr error
		switch mode {
		case "tls":
			// Pure TLS. On the gRPC plane this mode rides grpc.Creds and
			// secureListener hands the listener back untouched; for an HTTP
			// server ServeTLS is the equivalent, and it also negotiates ALPN for
			// both HTTP/1.1 and h2.
			serveErr = srv.ServeTLS(lis,
				strings.TrimSpace(cfg.Server.TLSCertFile), strings.TrimSpace(cfg.Server.TLSKeyFile))
		default:
			// Plaintext, or the loopback TLS+plaintext demux, which terminates
			// TLS at the listener. In the demux mode the listener is reachable
			// in PLAINTEXT on loopback by construction — the bind is loopback.
			serveErr = srv.Serve(lis)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("ADR-0130: listener stopped serving", "listener", label, "addr", addr, "err", serveErr)
		}
	}()
	slog.Info("ADR-0130: listener serving", "listener", label, "addr", addr, "transport", mode)
	return srv, addr, nil
}

// The plugin-facing seam. Premium plugins cannot import internal/config, so the
// policy is attached once from Run (setListenerPolicy) and reached through
// ServeHTTPListener — the same late-bound-holder shape as the named-secret
// seam (secret_seam.go), for the same reason: plugins build in bootstrapKernel
// and serve from their lifecycle Start, long after Run has the config.
var (
	listenerPolicyMu  sync.RWMutex
	listenerPolicyCfg *config.Config
)

// setListenerPolicy attaches the kernel config the HTTP listeners resolve
// their bind address and transport from. Called from Run; nil detaches.
func setListenerPolicy(cfg *config.Config) {
	listenerPolicyMu.Lock()
	defer listenerPolicyMu.Unlock()
	listenerPolicyCfg = cfg
}

// ErrListenerPolicyUnavailable is returned by ServeHTTPListener before Run has
// attached the kernel config (a plugin started outside a kernel, or a test).
var ErrListenerPolicyUnavailable = errors.New("listener policy unavailable: no kernel config attached (ADR-0130 D3)")

// ServeHTTPListener serves handler on port under the kernel's listener policy
// (ADR-0130 D3): bound on server.bind_address (loopback by default), refused
// in plaintext on a routable bind unless server.insecure_localhost=true, TLS
// when the kernel has it, ReadHeaderTimeout/IdleTimeout set, every body capped.
// It is the ONLY way a plugin may open an HTTP listener; the returned server is
// what the plugin's Stop drains. label names the listener in logs.
//
// A port is all a plugin may choose. A plugin that wants a different interface
// than the kernel's is asking to widen the deployment's exposure from a
// plugin's environment variable, which is exactly the half-truth the gRPC
// policy closed (see listenAddressOn).
func ServeHTTPListener(port string, handler http.Handler, label string) (*http.Server, error) {
	listenerPolicyMu.RLock()
	cfg := listenerPolicyCfg
	listenerPolicyMu.RUnlock()
	if cfg == nil {
		return nil, ErrListenerPolicyUnavailable
	}
	if handler == nil {
		return nil, fmt.Errorf("%s: nil handler", label)
	}
	srv, _, err := serveHTTP(cfg, port, handler, label)
	return srv, err
}
