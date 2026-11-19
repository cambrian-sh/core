package domain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type stubResolver struct {
	reg   domain.IngressRegistration
	found bool
	calls int
}

func (s *stubResolver) ResolveIngress(context.Context, domain.PrincipalRef) (domain.IngressRegistration, bool) {
	s.calls++
	return s.reg, s.found
}

// The namespace bound (ADR-0090, borrowed from Matrix application services): an
// ingress may only speak for identities inside its own namespace, so a
// compromised Telegram bridge cannot inject signals claiming to be a Slack user.
func TestMaySpeakFor(t *testing.T) {
	t.Parallel()
	tg := domain.IngressRegistration{AgentID: "telegram_ingress", Namespace: []string{"tg:"}}

	if !tg.MaySpeakFor("tg:12345") {
		t.Error("an id inside the namespace must be allowed")
	}
	if tg.MaySpeakFor("slack:U42") {
		t.Error("an id outside the namespace must be refused — this is the impersonation guard")
	}
	// "no sender" is not an identity. Allowing it would let an ingress open
	// conversations that can never be delivered to.
	if tg.MaySpeakFor("") || tg.MaySpeakFor("   ") {
		t.Error("an empty external id must never be accepted")
	}

	// Empty namespace = unrestricted, the honest default for a single-ingress
	// deployment: there is nobody to impersonate.
	open := domain.IngressRegistration{AgentID: "only_ingress"}
	if !open.MaySpeakFor("anything") {
		t.Error("an empty namespace should permit any non-empty id")
	}
	if open.MaySpeakFor("") {
		t.Error("even unrestricted must refuse an empty id")
	}

	// An empty prefix inside a namespace must not silently mean "allow all".
	sloppy := domain.IngressRegistration{AgentID: "x", Namespace: []string{""}}
	if sloppy.MaySpeakFor("slack:U42") {
		t.Error("an empty prefix must not act as a wildcard")
	}
}

// nil resolver is the OSS default and must be a no-op rather than a panic.
func TestIngressSurface_NilResolverIsNotAnIngress(t *testing.T) {
	t.Parallel()
	if _, ok := domain.IngressSurface(context.Background(), nil, domain.AgentPrincipal("a")); ok {
		t.Error("no registry means nothing is an ingress")
	}
}

// A principal the kernel could not establish is never looked up at all — the
// resolver must not even be consulted, so an empty id cannot match an empty key.
func TestIngressSurface_ZeroPrincipalShortCircuits(t *testing.T) {
	t.Parallel()
	r := &stubResolver{reg: domain.IngressRegistration{AgentID: "x", Surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"}}, found: true}
	if _, ok := domain.IngressSurface(context.Background(), r, domain.PrincipalRef{}); ok {
		t.Error("a zero principal must never resolve to an ingress")
	}
	if r.calls != 0 {
		t.Errorf("the registry should not be consulted for a zero principal, got %d calls", r.calls)
	}
}

// Registered, but with no surface configured: not a stamp. An empty SurfaceRef
// reads as "absent" downstream anyway, so returning it as found would be a lie.
func TestIngressSurface_RegistrationWithoutSurface(t *testing.T) {
	t.Parallel()
	r := &stubResolver{reg: domain.IngressRegistration{AgentID: "half"}, found: true}
	if _, ok := domain.IngressSurface(context.Background(), r, domain.AgentPrincipal("half")); ok {
		t.Error("a registration carrying no surface must not report one")
	}
}

func TestIngressSurface_ResolvesARegisteredIngress(t *testing.T) {
	t.Parallel()
	want := domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"}
	r := &stubResolver{reg: domain.IngressRegistration{AgentID: "telegram_ingress", Surface: want}, found: true}

	got, ok := domain.IngressSurface(context.Background(), r, domain.AgentPrincipal("telegram_ingress"))
	if !ok || got != want {
		t.Fatalf("IngressSurface = %+v ok=%v, want %+v", got, ok, want)
	}
}

// "not registered" and "registered as nothing" must both mean not an ingress.
func TestIngressSurface_NotFoundOrZero(t *testing.T) {
	t.Parallel()
	notFound := &stubResolver{found: false}
	if _, ok := domain.IngressSurface(context.Background(), notFound, domain.AgentPrincipal("a")); ok {
		t.Error("an unregistered principal is not an ingress")
	}
	zero := &stubResolver{reg: domain.IngressRegistration{}, found: true}
	if _, ok := domain.IngressSurface(context.Background(), zero, domain.AgentPrincipal("a")); ok {
		t.Error("a zero registration is not an ingress")
	}
}

// ADR-0155 D9 option a: the kernel's built-in chat head is the fixed pipeline
// ingress → run_turn. It always handles the turn, with exactly one call:
// run(ctx, conversationID, msg.Text) — the contract premium's parity test holds
// its default chat graph to.
func TestFixedTurnHead_RunsOneTurnWithTheMessageText(t *testing.T) {
	type call struct{ conv, text string }
	var got []call
	run := func(_ context.Context, conv, text string) error {
		got = append(got, call{conv, text})
		return nil
	}
	msg := domain.TurnMessage{Text: "where is my order?", SenderExternalID: "tg:1", Username: "ayse", EvidenceID: "ev-1"}
	handled, err := domain.FixedTurnHead{}.RouteTurn(context.Background(), "telegram_ingress", "conv-1", msg, run)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v, want handled with no error", handled, err)
	}
	if len(got) != 1 || got[0] != (call{"conv-1", "where is my order?"}) {
		t.Fatalf("turns = %+v, want exactly one (conv-1, the message text)", got)
	}
}

// The turn's failure is the head's failure, still reported as handled: no other
// head may run the turn a second time.
func TestFixedTurnHead_ReportsTheTurnsError(t *testing.T) {
	boom := errors.New("worker pool full")
	handled, err := domain.FixedTurnHead{}.RouteTurn(context.Background(), "i", "c",
		domain.TurnMessage{Text: "x"}, func(context.Context, string, string) error { return boom })
	if !handled || !errors.Is(err, boom) {
		t.Fatalf("handled=%v err=%v, want handled with the turn's error", handled, err)
	}
	if handled, err := (domain.FixedTurnHead{}).RouteTurn(context.Background(), "i", "c", domain.TurnMessage{Text: "x"}, nil); !handled || err == nil {
		t.Fatalf("a nil turn function must be refused, got handled=%v err=%v", handled, err)
	}
}
