package operator

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/identity"
)

func limiterAt(now *time.Time) *loginLimiter {
	l := newLoginLimiter()
	l.now = func() time.Time { return *now }
	return l
}

// Five failures inside the window lock a source out; the sixth is refused
// without reaching the credential; the window slides, and a success clears.
func TestLoginLimiter_FiveFailuresInFifteenMinutes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	l := limiterAt(&now)
	for i := 0; i < loginMaxFailures-1; i++ {
		if ok, _ := l.Allow("10.0.0.9"); !ok {
			t.Fatalf("locked out after %d failures", i)
		}
		l.Fail("10.0.0.9")
	}
	if ok, _ := l.Allow("10.0.0.9"); !ok {
		t.Fatal("locked out before the fifth failure")
	}
	l.Fail("10.0.0.9")
	ok, wait := l.Allow("10.0.0.9")
	if ok {
		t.Fatal("fifth failure did not lock the source out")
	}
	if wait <= 0 || wait > loginFailureWindow {
		t.Fatalf("wait = %v", wait)
	}
	// Another source is unaffected.
	if ok, _ := l.Allow("10.0.0.10"); !ok {
		t.Fatal("a different source was locked out")
	}
	// The oldest failure ages out of the window and the source may try again.
	now = now.Add(loginFailureWindow + time.Second)
	if ok, _ := l.Allow("10.0.0.9"); !ok {
		t.Fatal("lockout did not expire with the window")
	}
	l.Fail("10.0.0.9")
	l.Reset("10.0.0.9")
	if n := l.Fail("10.0.0.9"); n != 1 {
		t.Fatalf("after Reset the count restarted at %d, want 1", n)
	}
}

type recordingLoginObserver struct{ seen []LoginAttempt }

func (r *recordingLoginObserver) ObserveLogin(a LoginAttempt) { r.seen = append(r.seen, a) }

func peerCtx(host string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(host), Port: 4321}})
}

// The Login RPC: source is the gRPC peer, the limiter runs BEFORE the
// credential, the refusal is ResourceExhausted, every attempt is observed.
func TestLogin_PerSourceLockoutAndObservation(t *testing.T) {
	t.Parallel()
	svc := NewService(NewSpool(SpoolConfig{}))
	reg := identity.NewMemRegistry()
	la := identity.NewLocalAccounts(reg, reg)
	if _, err := la.EnsureAccount(context.Background(), "op", "pw", []string{"operator"}); err != nil {
		t.Fatal(err)
	}
	svc.SetIdentity(la)
	obs := &recordingLoginObserver{}
	svc.SetLoginObserver(obs)

	for i := 0; i < loginMaxFailures; i++ {
		_, err := svc.Login(peerCtx("192.168.1.7"), &pb.LoginRequest{Username: "op", Password: "wrong"})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Sixth: refused by the limiter even with the RIGHT password.
	_, err := svc.Login(peerCtx("192.168.1.7"), &pb.LoginRequest{Username: "op", Password: "pw"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("locked-out source got %v, want ResourceExhausted", err)
	}
	// A different source logs in fine.
	if _, err := svc.Login(peerCtx("192.168.1.8"), &pb.LoginRequest{Username: "op", Password: "pw"}); err != nil {
		t.Fatalf("other source refused: %v", err)
	}
	if len(obs.seen) != loginMaxFailures+2 {
		t.Fatalf("observed %d attempts, want %d", len(obs.seen), loginMaxFailures+2)
	}
	if last := obs.seen[loginMaxFailures]; !last.LockedOut || last.Source != "192.168.1.7" || last.Username != "op" {
		t.Fatalf("lockout attempt = %+v", last)
	}
	if ok := obs.seen[loginMaxFailures+1]; !ok.Success || ok.Source != "192.168.1.8" {
		t.Fatalf("success attempt = %+v", ok)
	}
	for _, a := range obs.seen {
		if a.Username == "pw" || a.Source == "pw" {
			t.Fatal("a password reached the observer")
		}
	}
}

// The ADR-0130 1f token TTL moved with the token minting to the local-accounts
// backend (ADR-0142 D1); identity.TestLocalAccounts pins it.
