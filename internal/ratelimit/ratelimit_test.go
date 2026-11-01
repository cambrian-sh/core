package ratelimit

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func cfg(rps float64, burst int) Config {
	l := Limit{RPS: rps, Burst: burst}
	return Config{Enabled: true, Agent: l, Operator: l, Anonymous: l, IdleTTL: time.Minute}
}

// The limit holds: burst requests pass, the next is refused, and refill
// restores it.
func TestLimiter_BurstThenRefuseThenRefill(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1750000000, 0)}
	l := New(cfg(2, 3), clk.now)
	for i := 0; i < 3; i++ {
		if !l.Allow(ClassAgent, "a") {
			t.Fatalf("request %d within burst refused", i)
		}
	}
	if l.Allow(ClassAgent, "a") {
		t.Fatal("request beyond burst allowed")
	}
	clk.t = clk.t.Add(500 * time.Millisecond) // 2 rps: one token
	if !l.Allow(ClassAgent, "a") {
		t.Fatal("refilled token refused")
	}
}

// Per-principal isolation: one principal exhausting its bucket does not touch
// another's, nor the same id in a different class.
func TestLimiter_PerPrincipalIsolation(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1750000000, 0)}
	l := New(cfg(1, 1), clk.now)
	if !l.Allow(ClassAgent, "a") || l.Allow(ClassAgent, "a") {
		t.Fatal("agent a should get exactly one")
	}
	if !l.Allow(ClassAgent, "b") {
		t.Fatal("agent b throttled by agent a's usage")
	}
	if !l.Allow(ClassOperator, "a") {
		t.Fatal("operator a shares a bucket with agent a")
	}
}

func TestLimiter_DisabledAndUnlimitedAllow(t *testing.T) {
	off := cfg(1, 1)
	off.Enabled = false
	l := New(off, nil)
	for i := 0; i < 10; i++ {
		if !l.Allow(ClassAgent, "a") {
			t.Fatal("disabled limiter refused")
		}
	}
	l = New(cfg(0, 0), nil)
	for i := 0; i < 10; i++ {
		if !l.Allow(ClassOperator, "a") {
			t.Fatal("unlimited class refused")
		}
	}
	var nilL *Limiter
	if !nilL.Allow(ClassAgent, "a") {
		t.Fatal("nil limiter refused")
	}
}

func TestLimiter_IdleBucketsAreSwept(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1750000000, 0)}
	l := New(cfg(10, 10), clk.now)
	for _, id := range []string{"p1", "p2", "p3"} {
		l.Allow(ClassAnonymous, id)
	}
	if l.size() != 3 {
		t.Fatalf("want 3 buckets, got %d", l.size())
	}
	clk.t = clk.t.Add(2 * time.Minute)
	l.Allow(ClassAnonymous, "p4")
	if l.size() != 1 {
		t.Fatalf("idle buckets not swept: %d remain", l.size())
	}
}

func TestKeyFor(t *testing.T) {
	ctx := domain.WithPrincipal(context.Background(), domain.UserPrincipal("alice"))
	if c, k := keyFor(ctx); c != ClassOperator || k != "alice" {
		t.Fatalf("user principal: %v %q", c, k)
	}
	ctx = domain.WithPrincipal(context.Background(), domain.AgentPrincipal("scout"))
	if c, k := keyFor(ctx); c != ClassAgent || k != "agent:scout" {
		t.Fatalf("agent principal: %v %q", c, k)
	}
	ctx = peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 5555}})
	if c, k := keyFor(ctx); c != ClassAnonymous || k != "10.1.2.3" {
		t.Fatalf("anonymous peer: %v %q", c, k)
	}
}

// Over the interceptor: overflow is ResourceExhausted and the handler is not
// reached; a different principal still passes.
func TestUnaryInterceptor_ResourceExhaustedPerPrincipal(t *testing.T) {
	l := New(cfg(0.001, 2), nil)
	ic := UnaryInterceptor(l)
	info := &grpc.UnaryServerInfo{FullMethod: "/cambrian.Orchestrator/QueryMemory"}
	calls := 0
	h := func(ctx context.Context, req any) (any, error) { calls++; return "ok", nil }
	a := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("a"))
	b := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("b"))
	for i := 0; i < 2; i++ {
		if _, err := ic(a, nil, info, h); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := ic(a, nil, info, h)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("handler reached on a refused call: %d calls", calls)
	}
	if _, err := ic(b, nil, info, h); err != nil {
		t.Fatalf("principal b throttled by a: %v", err)
	}
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

func TestStreamInterceptor_MetersOpens(t *testing.T) {
	l := New(cfg(0.001, 1), nil)
	ic := StreamInterceptor(l)
	info := &grpc.StreamServerInfo{FullMethod: "/cambrian.OperatorConsole/StreamEvents"}
	ss := ctxStream{ctx: domain.WithPrincipal(context.Background(), domain.UserPrincipal("op"))}
	h := func(srv any, s grpc.ServerStream) error { return nil }
	if err := ic(nil, ss, info, h); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := ic(nil, ss, info, h); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second open: want ResourceExhausted, got %v", err)
	}
}
