package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

func cfgWith(bind, cert, key string, insecure bool) *config.Config {
	c := &config.Config{}
	c.Server.Port = "50051"
	c.Server.BindAddress = bind
	c.Server.TLSCertFile = cert
	c.Server.TLSKeyFile = key
	c.Server.InsecureLocalhost = insecure
	return c
}

// THE case this exists to prevent: a routable bind with no TLS must not start.
//
// The operator plane carries bearer tokens. A token on an unencrypted link is a
// token anyone on the path has, so authentication without transport security is a
// weaker guarantee than it looks — and the old code bound ":port" (0.0.0.0) with
// no credentials at all and said nothing about it.
func TestTransportCredentials_RefusesPlaintextOnRoutableBind(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "192.168.1.10", "::", "example.internal"} {
		_, _, err := transportCredentials(cfgWith(bind, "", "", false))
		if err == nil {
			t.Fatalf("bind %q: plaintext on a routable address was allowed", bind)
		}
		if !strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("bind %q: error does not name the problem: %v", bind, err)
		}
		// The message must say what to DO, not merely that something is wrong.
		for _, remedy := range []string{"tls_cert_file", "127.0.0.1", "insecure_localhost"} {
			if !strings.Contains(err.Error(), remedy) {
				t.Errorf("bind %q: error omits the %q remedy: %v", bind, remedy, err)
			}
		}
	}
}

// Loopback keeps zero-config UX: the edge profile must not need a certificate.
func TestTransportCredentials_LoopbackPlaintextIsAllowed(t *testing.T) {
	for _, bind := range []string{"", "127.0.0.1", "localhost", "::1"} {
		opts, mode, err := transportCredentials(cfgWith(bind, "", "", false))
		if err != nil {
			t.Fatalf("bind %q: loopback plaintext refused: %v", bind, err)
		}
		if len(opts) != 0 {
			t.Errorf("bind %q: expected no TLS options", bind)
		}
		if mode != "plaintext-loopback" {
			t.Errorf("bind %q: mode = %q", bind, mode)
		}
	}
}

// An explicit opt-in is honoured — and reported, so it shows up in the boot log of
// the deployment that chose it rather than being invisible.
func TestTransportCredentials_ExplicitInsecureOptIn(t *testing.T) {
	_, mode, err := transportCredentials(cfgWith("0.0.0.0", "", "", true))
	if err != nil {
		t.Fatalf("explicit opt-in was refused: %v", err)
	}
	if mode != "plaintext-insecure-optin" {
		t.Errorf("mode = %q, want the opt-in to be distinguishable in the log", mode)
	}
}

// Half a TLS config is an ERROR, not a downgrade. Silently serving plaintext
// because one of two paths was missing is how people end up believing they have
// TLS when they do not.
func TestTransportCredentials_HalfConfiguredTLSIsAnError(t *testing.T) {
	for _, c := range []*config.Config{
		cfgWith("0.0.0.0", "/tmp/cert.pem", "", false),
		cfgWith("0.0.0.0", "", "/tmp/key.pem", false),
		// Even on loopback, where plaintext would otherwise be fine: the operator
		// asked for TLS and got silence.
		cfgWith("127.0.0.1", "/tmp/cert.pem", "", false),
	} {
		if _, _, err := transportCredentials(c); err == nil {
			t.Fatalf("half-configured TLS accepted (cert=%q key=%q)",
				c.Server.TLSCertFile, c.Server.TLSKeyFile)
		}
	}
}

// A real keypair yields real credentials, on any bind.
func TestTransportCredentials_TLSKeypairIsLoaded(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeSelfSigned(t, dir)

	for _, bind := range []string{"0.0.0.0", "127.0.0.1"} {
		opts, mode, err := transportCredentials(cfgWith(bind, cert, key, false))
		if err != nil {
			t.Fatalf("bind %q: %v", bind, err)
		}
		if mode != "tls" || len(opts) != 1 {
			t.Fatalf("bind %q: mode=%q opts=%d, want TLS", bind, mode, len(opts))
		}
	}
}

// An unreadable or malformed keypair FAILS rather than falling through to
// plaintext — the same fail-closed direction as everything else here.
func TestTransportCredentials_BadKeypairFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := transportCredentials(cfgWith("0.0.0.0", bad, bad, false)); err == nil {
		t.Fatal("a malformed keypair was accepted")
	}
}

// The default bind is LOOPBACK, not every interface. This is the behaviour change
// with the widest blast radius, so it is pinned.
func TestListenAddress_DefaultsToLoopback(t *testing.T) {
	if got := listenAddress(cfgWith("", "", "", false)); got != "127.0.0.1:50051" {
		t.Fatalf("default bind = %q, want loopback — \":port\" is 0.0.0.0 and exposes "+
			"the operator plane to the network", got)
	}
	if got := listenAddress(cfgWith("0.0.0.0", "", "", false)); got != "0.0.0.0:50051" {
		t.Fatalf("explicit bind = %q", got)
	}
}

// A hostname that cannot be classified is treated as ROUTABLE. Being wrong here
// costs a config flag; being wrong the other way costs an exposed plane.
func TestIsLoopback_UnknownHostIsTreatedAsRoutable(t *testing.T) {
	if isLoopback("some-host.internal") {
		t.Fatal("an unresolvable hostname was treated as loopback")
	}
	if isLoopback("0.0.0.0") || isLoopback("::") {
		t.Fatal("wildcard binds must not read as loopback — they are every interface")
	}
}

func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cambrian-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	cb := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, cb, 0o600); err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	kb := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
	if err := os.WriteFile(keyPath, kb, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
