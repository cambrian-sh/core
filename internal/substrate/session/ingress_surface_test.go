package session

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeIngressRegistry stands in for the premium registry (ADR-0090 D2). The kernel
// only ever APPLIES a registration, so a map is a faithful stand-in.
type fakeIngressRegistry map[string]domain.IngressRegistration

func (f fakeIngressRegistry) ResolveIngress(_ context.Context, p domain.PrincipalRef) (domain.IngressRegistration, bool) {
	reg, ok := f[p.ID]
	return reg, ok
}

func ctxAs(agentID string) context.Context {
	return domain.WithPrincipal(context.Background(), domain.AgentPrincipal(agentID))
}

// The gap this closes: Session.Surface documented itself as "decided ONCE by the
// kernel", SessionSurface read it, ResolveSurface preferred it — and nothing ever
// wrote it, so the session-scoped clamp could not engage at all.
func TestCreateSession_StampsTheIngressSurface(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	m.SetIngressResolver(fakeIngressRegistry{
		"telegram_ingress": {
			AgentID: "telegram_ingress",
			Surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"},
		},
	})

	ses, err := m.CreateSession(ctxAs("telegram_ingress"), "book a flight", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := store.sessions[ses.ID].Surface; got.Kind != domain.SurfaceChat || got.ID != "telegram" {
		t.Fatalf("surface was not stamped: %+v", got)
	}

	// And it reads back, which is what ResolveSurface depends on.
	surface, ok := m.SessionSurface(context.Background(), ses.ID)
	if !ok || surface.ID != "telegram" {
		t.Errorf("SessionSurface = %+v ok=%v, want the registered ingress surface", surface, ok)
	}
}

// The subtle one. ResolveSurface prefers a session's stored surface over the
// transport surface, on the assumption the stored one is NARROWER. That is true
// for an outsider ingress and FALSE for an operator, whose surface is the widest
// there is — so stamping every session with whatever opened it would silently
// widen ordinary sessions. Only registered ingresses stamp.
func TestCreateSession_DoesNotStampAnOrdinaryCaller(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	m.SetIngressResolver(fakeIngressRegistry{
		"telegram_ingress": {AgentID: "telegram_ingress", Surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"}},
	})

	ses, err := m.CreateSession(ctxAs("planner_agent"), "do some work", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := store.sessions[ses.ID].Surface; got.Kind != "" || got.ID != "" {
		t.Errorf("an unregistered caller must not stamp a surface, got %+v", got)
	}
	if _, ok := m.SessionSurface(context.Background(), ses.ID); ok {
		t.Error("SessionSurface must report absent so resolution falls through to the transport")
	}
}

// With no registry wired — every OSS deployment — nothing is an ingress and
// behaviour is exactly what it was before this change.
func TestCreateSession_NoRegistryIsUnchangedBehaviour(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)

	ses, err := m.CreateSession(ctxAs("telegram_ingress"), "book a flight", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := store.sessions[ses.ID].Surface; got.Kind != "" || got.ID != "" {
		t.Errorf("without a registry no session may carry a surface, got %+v", got)
	}
}

// A registration with no surface is not a licence to stamp an empty one — an
// empty SurfaceRef would make SessionSurface report "absent" anyway, so treating
// it as a stamp would only be confusing.
func TestCreateSession_RegistrationWithoutASurfaceStampsNothing(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	m.SetIngressResolver(fakeIngressRegistry{"half_configured": {AgentID: "half_configured"}})

	ses, _ := m.CreateSession(ctxAs("half_configured"), "hello", "")
	if got := store.sessions[ses.ID].Surface; got.Kind != "" || got.ID != "" {
		t.Errorf("expected no stamp, got %+v", got)
	}
}

// The scoped constructor is the one the chat path uses, so it must stamp too —
// caller_scope and surface are both non-forgeable facts decided at open time.
func TestCreateScopedSession_StampsAndKeepsCallerScope(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	m.SetIngressResolver(fakeIngressRegistry{
		"telegram_ingress": {AgentID: "telegram_ingress", Surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"}},
	})

	caller := domain.TagSet{ForbiddenTags: []string{"secrets"}}
	ses, err := m.CreateScopedSession(ctxAs("telegram_ingress"), "book a flight", "", caller)
	if err != nil {
		t.Fatalf("CreateScopedSession: %v", err)
	}
	got := store.sessions[ses.ID]
	if got.Surface.ID != "telegram" {
		t.Errorf("surface not stamped: %+v", got.Surface)
	}
	if len(got.CallerScope.ForbiddenTags) != 1 {
		t.Errorf("caller_scope was clobbered: %+v", got.CallerScope)
	}
}

// A principal the kernel could not establish must never resolve to an ingress —
// otherwise an unauthenticated caller inherits an ingress's surface.
func TestCreateSession_ZeroPrincipalIsNeverAnIngress(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	m.SetIngressResolver(fakeIngressRegistry{
		"": {AgentID: "", Surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"}},
	})

	ses, _ := m.CreateSession(context.Background(), "no principal", "")
	if got := store.sessions[ses.ID].Surface; got.Kind != "" || got.ID != "" {
		t.Errorf("a zero principal must not inherit a surface, got %+v", got)
	}
}
