// Package ratelimit is the per-principal token-bucket limit on the kernel's
// gRPC planes (T-20261101-rate-limit-the-operator-and-agent-grpc-planes;
// ADR-0047 operator plane, ADR-0130 agent plane).
//
// The interceptors run LAST in the server chain — after the surface stamp, the
// agent credential check (ADR-0130 D1) and the operator bearer check (ADR-0047
// D13) — so the bucket key is the principal those established on the context
// (domain.PrincipalFromContext), never a value the caller asserts unverified.
// A request with no principal is keyed by its peer host.
//
// The limiter is a storm guard, not a quota: config defaults are generous (see
// config.RateLimitConfig). It is constructed by the composition root and passed
// to the interceptor constructors (AGENTS.md §2.3) — there is no setter.
//
// No background goroutine: idle buckets are swept lazily from Allow, at most
// once per sweep interval, so the package owns nothing a goleak TestMain would
// have to drain.
package ratelimit

import (
	"context"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

// Class selects a bucket shape.
type Class int

const (
	// ClassAgent covers agent and other non-human principals.
	ClassAgent Class = iota
	// ClassOperator covers human operator identities.
	ClassOperator
	// ClassAnonymous covers requests with no established principal.
	ClassAnonymous
)

// Limit is one bucket shape. RPS == 0 leaves the class unlimited.
type Limit struct {
	RPS   float64
	Burst int
}

// Config is the limiter's construction input, mapped by the composition root
// from config.RateLimitConfig so this package does not import internal/config.
type Config struct {
	Enabled   bool
	Agent     Limit
	Operator  Limit
	Anonymous Limit
	// IdleTTL is how long an untouched bucket survives. <= 0 disables sweeping.
	IdleTTL time.Duration
}

type bucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// Limiter holds one token bucket per (class, principal) key.
type Limiter struct {
	cfg Config
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// New builds a limiter. now may be nil (time.Now).
func New(cfg Config, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{cfg: cfg, now: now, buckets: make(map[string]*bucket), lastSweep: now()}
}

func (l *Limiter) limitFor(c Class) Limit {
	switch c {
	case ClassOperator:
		return l.cfg.Operator
	case ClassAnonymous:
		return l.cfg.Anonymous
	default:
		return l.cfg.Agent
	}
}

// Allow spends one token from key's bucket in class c and reports whether the
// request may proceed. A nil or disabled limiter, or an unlimited class, always
// allows.
func (l *Limiter) Allow(c Class, key string) bool {
	if l == nil || !l.cfg.Enabled {
		return true
	}
	lim := l.limitFor(c)
	if lim.RPS <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	k := classPrefix(c) + key
	b, ok := l.buckets[k]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(lim.RPS), lim.Burst)}
		l.buckets[k] = b
	}
	b.lastSeen = now
	return b.lim.AllowN(now, 1)
}

// sweepLocked drops buckets idle for longer than IdleTTL, at most once per
// IdleTTL. A bucket idle that long has refilled to its burst in any sane
// config, so dropping it loses nothing a client could notice.
func (l *Limiter) sweepLocked(now time.Time) {
	ttl := l.cfg.IdleTTL
	if ttl <= 0 || now.Sub(l.lastSweep) < ttl {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > ttl {
			delete(l.buckets, k)
		}
	}
}

// size is the live bucket count (tests).
func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func classPrefix(c Class) string {
	switch c {
	case ClassOperator:
		return "op|"
	case ClassAnonymous:
		return "anon|"
	default:
		return "agent|"
	}
}

// keyFor resolves the bucket for a request from the authenticated principal,
// falling back to the peer host. The port is dropped: it changes per
// connection and would hand every reconnect a fresh bucket.
func keyFor(ctx context.Context) (Class, string) {
	if p := domain.PrincipalFromContext(ctx); !p.IsZero() {
		if p.Kind == domain.PrincipalUser {
			return ClassOperator, p.ID
		}
		return ClassAgent, string(p.Kind) + ":" + p.ID
	}
	if pr, ok := peer.FromContext(ctx); ok && pr.Addr != nil {
		if host, _, err := net.SplitHostPort(pr.Addr.String()); err == nil && host != "" {
			return ClassAnonymous, host
		}
		return ClassAnonymous, pr.Addr.String()
	}
	return ClassAnonymous, "unknown"
}

func exhausted(method string) error {
	return status.Errorf(codes.ResourceExhausted,
		"rate limit exceeded for this principal on %s; back off and retry (server.rate_limit)", method)
}

// UnaryInterceptor meters every unary RPC. Place it after the authentication
// interceptors in grpc.ChainUnaryInterceptor.
func UnaryInterceptor(l *Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		c, key := keyFor(ctx)
		if !l.Allow(c, key) {
			return nil, exhausted(info.FullMethod)
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor meters stream OPENS: one token per stream. Messages on an
// established stream are not metered — a long-lived feed (StreamEvents,
// SignalStream) is one call, and metering its messages would throttle the
// kernel's own output.
func StreamInterceptor(l *Limiter) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		c, key := keyFor(ss.Context())
		if !l.Allow(c, key) {
			return exhausted(info.FullMethod)
		}
		return handler(srv, ss)
	}
}
