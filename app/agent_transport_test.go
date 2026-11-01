package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert writes a cert/key pair. issuerKey nil ⇒ self-signed; otherwise the
// cert is signed by a throwaway CA (not self-signed).
func writeCert(t *testing.T, dir string, dns []string, ips []net.IP, caSigned bool) (string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kernel"},
		DNSNames:     dns,
		IPAddresses:  ips,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	parent, signer := tmpl, priv
	if caSigned {
		caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		parent = &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
		signer = caKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &priv.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// Loopback modes keep local agents on plaintext loopback (ADR-0130 D2 demux);
// only the exact bound address replaces "localhost".
func TestAgentTransport_LoopbackStaysPlaintext(t *testing.T) {
	cert, key := writeCert(t, t.TempDir(), []string{"kernel.lan"}, nil, false)
	for _, c := range []struct{ bind, cert, key, want string }{
		{"", "", "", "127.0.0.1:50051"},
		{"127.0.0.1", "", "", "127.0.0.1:50051"},
		{"::1", "", "", "[::1]:50051"},
		{"127.0.0.1", cert, key, "127.0.0.1:50051"}, // TLS+plaintext demux
	} {
		tr, warn, err := agentTransport(cfgWith(c.bind, c.cert, c.key, false))
		if err != nil {
			t.Fatalf("bind %q: %v", c.bind, err)
		}
		if tr.Addr != c.want || tr.TLSCAFile != "" || warn != "" {
			t.Errorf("bind %q cert=%v: got %+v warn=%q, want plaintext %s", c.bind, c.cert != "", tr, warn, c.want)
		}
	}
}

// THE bug: a TLS-only routable bind. Agents get the bound address and the
// root; a wildcard bind is dialled on loopback with a server-name override the
// certificate actually covers.
func TestAgentTransport_TLSRoutableBindInjectsRootAndReachableAddr(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, dir, []string{"kernel.lan"}, []net.IP{net.ParseIP("192.168.1.10")}, false)

	tr, warn, err := agentTransport(cfgWith("192.168.1.10", cert, key, false))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Addr != "192.168.1.10:50051" || tr.TLSCAFile != cert || tr.TLSServerName != "" || warn != "" {
		t.Fatalf("specific bind: %+v warn=%q", tr, warn)
	}

	tr, _, err = agentTransport(cfgWith("0.0.0.0", cert, key, false))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Addr != "127.0.0.1:50051" || tr.TLSCAFile != cert || tr.TLSServerName != "kernel.lan" {
		t.Fatalf("wildcard bind: %+v (want loopback dial, root, override kernel.lan)", tr)
	}
}

// A CA-issued leaf is still injected, with a boot warning that the SDK pins
// the root it is given.
func TestAgentTransport_CASignedCertWarns(t *testing.T) {
	cert, key := writeCert(t, t.TempDir(), []string{"kernel.lan"}, nil, true)
	tr, warn, err := agentTransport(cfgWith("10.0.0.5", cert, key, false))
	if err != nil {
		t.Fatal(err)
	}
	if tr.TLSCAFile != cert || warn == "" || tr.TLSServerName != "kernel.lan" {
		t.Fatalf("CA-signed: %+v warn=%q", tr, warn)
	}
}

// Routable plaintext under the opt-in: the bound address, no TLS.
func TestAgentTransport_InsecureOptIn(t *testing.T) {
	tr, _, err := agentTransport(cfgWith("10.0.0.5", "", "", true))
	if err != nil || tr.Addr != "10.0.0.5:50051" || tr.TLSCAFile != "" {
		t.Fatalf("insecure opt-in: %+v err=%v", tr, err)
	}
}
