package network

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// newLeaseServer builds a Server with a real gateway so lease IDs are minted the same way
// production mints them.
func newLeaseServer(t *testing.T) (*Server, *SubstrateLLMGateway) {
	t.Helper()
	gw := NewLLMGateway(config.ExecutionConfig{
		LLMGatewayMaxConcurrency:  4,
		SessionTokenTTLMultiplier: 5,
	})
	return &Server{LLMGateway: gw}, gw
}

func ctxWithMD(pairs ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// A lease presented on the NEW header resolves to the session the kernel bound to it —
// never to the lease ID itself. This is the core of the Phase-0 fix.
func TestResolveCallerSession_LeaseHeader_ResolvesToBoundSession(t *testing.T) {
	s, gw := newLeaseServer(t)
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "task-session-1", RunID: "run-9", StepIndex: 3})

	if got := s.resolveCallerSession(ctxWithMD(leaseHeader, string(lease))); got != "task-session-1" {
		t.Errorf("expected bound session %q, got %q", "task-session-1", got)
	}
}

// A lease the kernel never issued must NOT be reinterpreted as a session ID. Otherwise an
// agent could name any session simply by putting a string in the header.
func TestResolveCallerSession_UnknownLease_YieldsNoSession(t *testing.T) {
	s, _ := newLeaseServer(t)
	if got := s.resolveCallerSession(ctxWithMD(leaseHeader, "sess-forged-12345")); got != "" {
		t.Errorf("an unknown lease must resolve to no session, got %q", got)
	}
}

// A live but UNBOUND lease is known, so it resolves to no session — not to the lease ID.
// This is the scout/retrieval/bypass case: real credential, no task session behind it.
func TestResolveCallerSession_KnownButUnboundLease_YieldsNoSession(t *testing.T) {
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)

	if got := s.resolveCallerSession(ctxWithMD(leaseHeader, string(lease))); got != "" {
		t.Errorf("an unbound lease must resolve to no session, got %q", got)
	}
}

// The regression this phase exists for: a stale SDK puts its LEASE in the legacy
// x-session-id header. The kernel must resolve it as a lease, never treat the lease ID as
// a session ID — which is what silently disabled ADR-0034 D13, ADR-0048 D4 and D1.
func TestResolveCallerSession_LegacyHeaderCarryingLease_ResolvesToSession(t *testing.T) {
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "task-session-2"})

	got := s.resolveCallerSession(ctxWithMD(sessionHeader, string(lease)))
	if got == domain.SessionID(lease) {
		t.Fatalf("legacy header carrying a lease was treated as a session ID (%q) — the Phase-0 bug", got)
	}
	if got != "task-session-2" {
		t.Errorf("expected bound session %q, got %q", "task-session-2", got)
	}
}

// The operator plane keeps working: it sends a REAL session ID on the legacy header, which
// is not a known lease and must pass through untouched.
func TestResolveCallerSession_LegacyHeaderCarryingSessionID_PassesThrough(t *testing.T) {
	s, _ := newLeaseServer(t)
	if got := s.resolveCallerSession(ctxWithMD(sessionHeader, "operator-session-7")); got != "operator-session-7" {
		t.Errorf("operator session ID must pass through, got %q", got)
	}
}

// Sending both headers (the SDK's transition posture) must agree with the lease header.
func TestResolveCallerSession_BothHeaders_PrefersLease(t *testing.T) {
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "task-session-3"})

	ctx := ctxWithMD(leaseHeader, string(lease), sessionHeader, string(lease))
	if got := s.resolveCallerSession(ctx); got != "task-session-3" {
		t.Errorf("expected %q, got %q", "task-session-3", got)
	}
}

func TestResolveCallerSession_NoMetadata_IsEmpty(t *testing.T) {
	s, _ := newLeaseServer(t)
	if got := s.resolveCallerSession(context.Background()); got != "" {
		t.Errorf("expected empty session with no metadata, got %q", got)
	}
}

// A gateway that does not implement domain.LeaseResolver must not panic and must leave the
// legacy path working.
func TestResolveCallerSession_NoResolver_FallsBackToLegacyHeader(t *testing.T) {
	s := &Server{}
	if got := s.resolveCallerSession(ctxWithMD(sessionHeader, "operator-session-8")); got != "operator-session-8" {
		t.Errorf("expected legacy pass-through without a resolver, got %q", got)
	}
	if got := s.resolveCallerSession(ctxWithMD(leaseHeader, "sess-1")); got != "" {
		t.Errorf("a lease cannot be resolved without a resolver, got %q", got)
	}
}

// withCallerSession must leave ctx untouched when the caller has no session, so
// "no session" stays distinguishable from "session with an empty ID".
func TestWithCallerSession_NoSession_LeavesCtxUnseeded(t *testing.T) {
	s, _ := newLeaseServer(t)
	out := s.withCallerSession(context.Background())
	if sid, ok := domain.SessionIDFromContext(out); ok {
		t.Errorf("expected no session seeded in ctx, got %q", sid)
	}
}

// After Complete, the lease is gone from the registry, so it stops resolving. A completed
// lease must not keep granting access to its session.
func TestResolveCallerSession_CompletedLease_StopsResolving(t *testing.T) {
	s, gw := newLeaseServer(t)
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "task-session-4"})
	if _, err := gw.Complete(context.Background(), lease); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got := s.resolveCallerSession(ctxWithMD(leaseHeader, string(lease))); got != "" {
		t.Errorf("a completed lease must not resolve, got %q", got)
	}
}

// The invariant the phase is named for: a lease ID is not a session ID, and the session
// store must never recognise one. If this ever passes, the two namespaces have merged
// again.
func TestLeaseIDIsNeverASession(t *testing.T) {
	gw := NewLLMGateway(config.ExecutionConfig{LLMGatewayMaxConcurrency: 1, SessionTokenTTLMultiplier: 5})
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Second)

	mgr := session.New(newMemSessionStore())
	// Phase 1: passing `lease` here without the cast is now a COMPILE error —
	// domain.LeaseID and domain.SessionID are distinct types. The explicit conversion
	// below is what an attacker (or a careless refactor) would have to write on purpose,
	// and it still must not resolve.
	ses, err := mgr.GetSession(context.Background(), domain.SessionID(lease))
	if err == nil && ses != nil {
		t.Fatalf("a BudgetLease ID resolved as a task session (%q) — the namespaces have merged", lease)
	}

	// And the real thing still resolves, so the test above is not passing vacuously.
	created, err := mgr.CreateSession(context.Background(), "goal", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := mgr.GetSession(context.Background(), created.ID)
	if err != nil || got == nil {
		t.Fatalf("a real session must resolve: %v", err)
	}
}

// memSessionStore is a minimal in-memory session.SessionRepository.
type memSessionStore struct {
	m map[domain.SessionID]domain.Session
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{m: map[domain.SessionID]domain.Session{}}
}

func (s *memSessionStore) SaveSession(_ context.Context, ses domain.Session) error {
	s.m[ses.ID] = ses
	return nil
}

func (s *memSessionStore) GetSession(_ context.Context, id domain.SessionID) (*domain.Session, error) {
	ses, ok := s.m[id]
	if !ok {
		return nil, nil
	}
	return &ses, nil
}

func (s *memSessionStore) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	var out []domain.Session
	for _, ses := range s.m {
		if status == "" || ses.Status == status {
			out = append(out, ses)
		}
	}
	return out, nil
}
