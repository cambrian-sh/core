package app

import (
	"context"
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
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// writeCAIssued writes a CA root (ca.pem) and a kernel certificate it issued
// (cert.pem/key.pem) naming dns — the CA-issued deployment of ENG-299.
func writeCAIssued(t *testing.T, dir string, dns ...string) (cert, key, ca string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "internal-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kernel"}, DNSNames: dns,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, ca = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "ca.pem")
	for path, block := range map[string]*pem.Block{
		cert: {Type: "CERTIFICATE", Bytes: leafDER},
		key:  {Type: "EC PRIVATE KEY", Bytes: kd},
		ca:   {Type: "CERTIFICATE", Bytes: caDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cert, key, ca
}

// ENG-299 (owner decision 2026-11-19): a CA-issued certificate's spawned agents
// get the configured issuing root, not the leaf, and no warning.
func TestAgentTransport_AgentCAFileIsTheInjectedRoot(t *testing.T) {
	cert, key, ca := writeCAIssued(t, t.TempDir(), "kernel.lan")
	cfg := cfgWith("10.0.0.5", cert, key, false)
	cfg.Server.TLSAgentCAFile = ca
	tr, warn, err := agentTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TLSCAFile != ca || tr.TLSServerName != "kernel.lan" || warn != "" {
		t.Fatalf("agent CA: %+v warn=%q", tr, warn)
	}
	// Without the key the old behaviour stands, and the warning names the key.
	cfg.Server.TLSAgentCAFile = ""
	tr, warn, err = agentTransport(cfg)
	if err != nil || tr.TLSCAFile != cert || !strings.Contains(warn, "tls_agent_ca_file") {
		t.Fatalf("no agent CA: %+v warn=%q err=%v", tr, warn, err)
	}
}

// A root the certificate does not chain to refuses the boot, as does an
// unreadable one; in a loopback mode the key is unused and said so.
func TestAgentTransport_AgentCAFileIsCheckedAtBoot(t *testing.T) {
	cert, key, _ := writeCAIssued(t, t.TempDir(), "kernel.lan")
	_, _, otherCA := writeCAIssued(t, t.TempDir(), "kernel.lan")
	cfg := cfgWith("10.0.0.5", cert, key, false)
	cfg.Server.TLSAgentCAFile = otherCA
	if _, _, err := agentTransport(cfg); err == nil || !strings.Contains(err.Error(), "tls_agent_ca_file") {
		t.Fatalf("a root the certificate does not chain to must refuse the boot: %v", err)
	}
	cfg.Server.TLSAgentCAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, _, err := agentTransport(cfg); err == nil {
		t.Fatal("an unreadable agent CA must refuse the boot")
	}
	loop := cfgWith("127.0.0.1", cert, key, false)
	loop.Server.TLSAgentCAFile = otherCA
	tr, warn, err := agentTransport(loop)
	if err != nil || tr.TLSCAFile != "" || !strings.Contains(warn, "unused") {
		t.Fatalf("loopback mode: %+v warn=%q err=%v", tr, warn, err)
	}
}

// End to end: a TLS gRPC server with the CA-issued certificate, dialled the way
// a spawned agent dials (pinning exactly the injected root, with the injected
// server name): the injected agent CA completes the handshake. (The leaf the
// kernel used to inject fails in the SDK's BoringSSL handshake, which does not
// accept a non-CA leaf as a trust anchor; Go's verifier does, so this test
// cannot show that half.)
func TestAgentTransport_CAIssuedHandshakeWithTheInjectedRoot(t *testing.T) {
	cert, key, ca := writeCAIssued(t, t.TempDir(), "kernel.lan")
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&pair)))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	dial := func(root, serverName string) error {
		creds, err := credentials.NewClientTLSFromFile(root, serverName)
		if err != nil {
			return err
		}
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}

	cfg := cfgWith("10.0.0.5", cert, key, false)
	cfg.Server.TLSAgentCAFile = ca
	tr, _, err := agentTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(tr.TLSCAFile, tr.TLSServerName); err != nil {
		t.Fatalf("handshake with the injected agent CA: %v", err)
	}
}
