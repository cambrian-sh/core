package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// writeSelfSignedPair generates a loopback keypair on disk, as setup does.
func writeSelfSignedPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "kernel.crt")
	keyPath = filepath.Join(dir, "kernel.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func tlsLoopbackConfig(t *testing.T, bind string) *config.Config {
	t.Helper()
	cert, key := writeSelfSignedPair(t)
	cfg := &config.Config{}
	cfg.Server.BindAddress = bind
	cfg.Server.TLSCertFile = cert
	cfg.Server.TLSKeyFile = key
	return cfg
}

// TLS + loopback bind must select the demux mode with NO grpc.Creds; the same
// certs on a routable bind must stay pure-TLS via grpc.Creds.
func TestTransportCredentialsModeSelection(t *testing.T) {
	cfg := tlsLoopbackConfig(t, "127.0.0.1")
	opts, mode, err := transportCredentials(cfg)
	if err != nil {
		t.Fatalf("loopback+TLS: %v", err)
	}
	if mode != modeTLSPlusPlaintextLoopback || len(opts) != 0 {
		t.Errorf("loopback+TLS: mode=%q opts=%d; want demux mode with no creds", mode, len(opts))
	}

	cfg.Server.BindAddress = "0.0.0.0"
	opts, mode, err = transportCredentials(cfg)
	if err != nil {
		t.Fatalf("routable+TLS: %v", err)
	}
	if mode != "tls" || len(opts) == 0 {
		t.Errorf("routable+TLS: mode=%q opts=%d; want pure tls with creds", mode, len(opts))
	}
}

// One demux listener must serve a real TLS handshake AND a plaintext
// connection — the exact split the server deployment needs (cloudflared vs
// the Python agent plane).
func TestDemuxListenerServesTLSAndPlaintext(t *testing.T) {
	cfg := tlsLoopbackConfig(t, "127.0.0.1")
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lis, err := secureListener(raw, cfg, modeTLSPlusPlaintextLoopback)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	// Echo server: accept, read 5 bytes, write them back.
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 5)
				if _, err := c.Read(buf); err == nil {
					_, _ = c.Write(buf)
				}
			}(c)
		}
	}()

	addr := lis.Addr().String()

	// Plaintext leg.
	pc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_ = pc.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := pc.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := pc.Read(buf); err != nil || string(buf) != "hello" {
		t.Fatalf("plaintext echo failed: %q %v", buf, err)
	}

	// TLS leg (self-signed, so verification off — this tests the handshake).
	tc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}) //nolint:gosec // test dials a self-signed loopback listener
	if err != nil {
		t.Fatalf("tls handshake: %v", err)
	}
	defer tc.Close()
	if got := tc.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Errorf("ALPN = %q, want h2 (cloudflared's http2Origin requires it)", got)
	}
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := tc.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Read(buf); err != nil || string(buf) != "world" {
		t.Fatalf("tls echo failed: %q %v", buf, err)
	}
}
