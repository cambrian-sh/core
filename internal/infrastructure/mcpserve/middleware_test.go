package mcpserve

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// staticSecrets is a stand-in for the ADR-0101 named-secret seam.
func staticSecrets(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok && v != ""
	}
}

// capture records the context the wrapped handler was reached with, so a test
// can assert what the middleware established rather than what it logged.
type capture struct {
	reached   bool
	principal domain.PrincipalRef
	surface   domain.SurfaceRef
}

func (c *capture) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.reached = true
	c.principal = domain.PrincipalFromContext(r.Context())
	c.surface = domain.SurfaceFromContext(r.Context())
}

func testMiddleware(t *testing.T, next http.Handler, maxPerClient int) http.Handler {
	t.Helper()
	h, err := newAuthMiddleware(Options{
		Clients:                []string{"ci-bot"},
		ResolveSecret:          staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"}),
		MaxConcurrentPerClient: maxPerClient,
	}, next)
	if err != nil {
		t.Fatalf("newAuthMiddleware: %v", err)
	}
	return h
}

// THE case this exists to prevent. OSS ships a fail-OPEN authorizer, so an
// unauthenticated request that reached a handler would be an anonymous read of
// the whole corpus. The token is the only lock on this door.
func TestMiddleware_MissingOrMalformedCredentialIs401(t *testing.T) {
	for _, header := range []string{
		"",                   // no header at all
		"token-aaaa",         // the right token, no scheme
		"Basic token-aaaa",   // the wrong scheme
		"Bearer",             // scheme with nothing after it
		"Bearer    ",         // scheme with only whitespace
		"Bearer\ttoken-aaaa", // tab-separated: not the RFC 7235 form
	} {
		next := &capture{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		testMiddleware(t, next, 4).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", header, rec.Code)
		}
		if next.reached {
			t.Errorf("header %q: an unauthenticated request reached the handler", header)
		}
		if body := rec.Body.String(); body != "" {
			t.Errorf("header %q: 401 carried a body %q — it must not say which half was wrong", header, body)
		}
	}
}

// A wrong token is the SAME answer as an unknown one: anything else enumerates
// the client roster for whoever asks.
func TestMiddleware_WrongTokenIs401AndSaysNothing(t *testing.T) {
	for _, token := range []string{
		"token-bbbb",  // same length, different value — the constant-time path
		"token-aaa",   // a prefix of the real one
		"token-aaaab", // the real one plus a byte
		"",
	} {
		next := &capture{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		testMiddleware(t, next, 4).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized || next.reached {
			t.Errorf("token %q: status=%d reached=%v, want a silent 401", token, rec.Code, next.reached)
		}
	}
}

// The valid case establishes BOTH facts (ADR-0126 D4): who is asking and where
// they arrived from. Neither substitutes for the other — a policy targets the
// principal to name one client and the surface to name every outsider at once.
func TestMiddleware_ValidTokenEstablishesPrincipalAndSurface(t *testing.T) {
	next := &capture{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "bearer token-aaaa") // lower-case scheme is legal
	testMiddleware(t, next, 4).ServeHTTP(rec, req)

	if !next.reached {
		t.Fatalf("a valid credential did not reach the handler (status %d)", rec.Code)
	}
	if next.principal.ID != "mcp:ci-bot" {
		t.Errorf("principal = %q, want the named client mcp:ci-bot", next.principal.ID)
	}
	if next.principal.Kind != domain.PrincipalAgent {
		t.Errorf("principal kind = %q, want %q — an external coding agent is scoped by the same "+
			"machinery as an internal one", next.principal.Kind, domain.PrincipalAgent)
	}
	if next.surface.Kind != domain.SurfaceMCP {
		t.Errorf("surface = %+v, want kind %q", next.surface, domain.SurfaceMCP)
	}
	if next.surface.ID == "" {
		t.Error("surface carries no id; it should name the entry point, as the operator plane names its console")
	}
}

// Storm control (ADR-0126 Consequences): coding agents retry aggressively and the
// answer tools spend an LLM budget, so an over-cap request is REFUSED rather than
// queued — a queue turns a burst into a latency cliff the client reads as a hang.
func TestMiddleware_OverCapIs429(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var served atomic.Int64
	blocking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served.Add(1)
		entered <- struct{}{}
		<-release
	})
	h := testMiddleware(t, blocking, 1)
	authed := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer token-aaaa")
		return req
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(), authed())
	}()
	// Receiving from the unbuffered channel proves the first request is INSIDE
	// the handler, so the single slot is held. No sleeping, no spinning.
	<-entered

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent request status = %d, want 429", rec.Code)
	}
	if got := served.Load(); got != 1 {
		t.Errorf("handler ran %d times; an over-cap request must be refused before it, not queued", got)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After; a retrying client has nothing to pace against")
	}

	close(release)
	wg.Wait()

	// The slot is RELEASED, not spent: the cap bounds concurrency, not the
	// lifetime request count.
	after := &capture{}
	h2 := testMiddleware(t, after, 1)
	h2.ServeHTTP(httptest.NewRecorder(), authed())
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, authed())
	if !after.reached || rec2.Code == http.StatusTooManyRequests {
		t.Fatal("a sequential second request was refused; the cap must be on concurrency")
	}
}

// An endpoint that can authenticate nobody must not start: with no credential
// source every request is a 401, which is a broken deployment wearing the
// costume of a working one.
func TestNewAuthMiddleware_RefusesToStartWithoutCredentials(t *testing.T) {
	if _, err := newAuthMiddleware(Options{Clients: []string{"ci-bot"}}, &capture{}); err == nil {
		t.Error("a middleware with no credential source was accepted")
	}
	if _, err := newAuthMiddleware(Options{
		ResolveSecret: staticSecrets(nil),
	}, &capture{}); err == nil {
		t.Error("a middleware with no configured clients was accepted")
	}
}

// A configured client whose credential was never issued can never authenticate —
// and, in particular, an empty presented token must not match an empty stored
// one.
func TestMiddleware_ClientWithNoIssuedCredentialCannotAuthenticate(t *testing.T) {
	next := &capture{}
	h, err := newAuthMiddleware(Options{
		Clients:       []string{"never-issued"},
		ResolveSecret: staticSecrets(map[string]string{"mcp:client:never-issued": ""}),
	}, next)
	if err != nil {
		t.Fatalf("newAuthMiddleware: %v", err)
	}
	for _, token := range []string{"", "anything"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || next.reached {
			t.Fatalf("token %q authenticated against an unissued credential", token)
		}
	}
}
