package mcpserve

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"github.com/cambrian-sh/core/internal/config"
)

// The D4 middleware — one pass that establishes everything before any protocol
// handling runs: credential → client → principal → surface → decision point,
// under a per-credential concurrency bound.
//
// THE TOKEN IS NEVER OPTIONAL, on loopback or anywhere else. TLS may be waived
// on a loopback bind; authentication may not, and the asymmetry is not
// stylistic: OSS ships domain.AllowAllAuthorizer, which fails OPEN by design.
// An unauthenticated endpoint on top of a fail-open decision point is an
// anonymous read of the whole corpus, so the bearer token is the only lock on
// this door in an OSS build.

// authMiddleware wraps the SDK handler.
type authMiddleware struct {
	next     http.Handler
	clients  []string
	resolve  func(name string) (string, bool)
	authz    domain.Authorizer
	identity domain.IdentityResolver
	// sem holds one buffered channel per configured client, built ONCE at
	// construction so the request path only ever sends and receives — a map
	// grown per request would be a data race in the one place that must not
	// have one.
	sem map[string]chan struct{}
}

// newAuthMiddleware validates the options and wraps next.
func newAuthMiddleware(opts Options, next http.Handler) (http.Handler, error) {
	if next == nil {
		return nil, fmt.Errorf("mcpserve: middleware needs a handler to wrap")
	}
	if opts.ResolveSecret == nil {
		return nil, fmt.Errorf("mcpserve: no credential source; the endpoint cannot authenticate anyone " +
			"and an endpoint that authenticates nobody must not start")
	}
	if len(opts.Clients) == 0 {
		return nil, fmt.Errorf("mcpserve: no clients configured; set server.mcp.clients")
	}
	perClient := opts.MaxConcurrentPerClient
	if perClient <= 0 {
		perClient = config.DefaultMCPMaxConcurrentPerClient
	}
	m := &authMiddleware{
		next:     next,
		clients:  append([]string(nil), opts.Clients...),
		resolve:  opts.ResolveSecret,
		authz:    opts.Authorizer,
		identity: opts.Identity,
		sem:      make(map[string]chan struct{}, len(opts.Clients)),
	}
	for _, c := range m.clients {
		m.sem[c] = make(chan struct{}, perClient)
	}
	return m, nil
}

func (m *authMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		unauthorized(w)
		return
	}
	client, ok := m.match(token)
	if !ok {
		unauthorized(w)
		return
	}
	release, ok := m.acquire(client)
	if !ok {
		// 429, not a queue. ask_memory spends an LLM budget under a 90 s
		// deadline and coding agents retry aggressively; a queue would convert
		// a burst into a latency cliff the client reads as a hang.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many concurrent requests for this client", http.StatusTooManyRequests)
		return
	}
	defer release()

	// Two facts, neither substituting for the other (ADR-0126 D4): WHO is asking
	// (this named client) and WHERE they arrived from (the mcp surface). A policy
	// can target either — "external agents never read finance/*" is a surface
	// rule; "mcp:ci-bot is read-only" is a principal rule.
	principal, ok := m.principalFor(r.Context(), client)
	if !ok {
		// The credential was VALID and the sender is still refused, so this is a
		// 403 with a reason rather than the deliberately silent 401 above: an
		// operator who blocked this client, or a surface set to refuse anyone
		// unbound, needs the client's own logs to say which — otherwise a
		// correctly-issued token looks like a wrong one.
		http.Error(w, "this client is not permitted on the mcp surface", http.StatusForbidden)
		return
	}
	ctx := domain.WithPrincipal(r.Context(), principal)
	ctx = domain.WithSurface(ctx, domain.SurfaceRef{Kind: domain.SurfaceMCP, ID: surfaceID})
	if m.authz != nil {
		// Carried on the context rather than closed over, so every seam
		// downstream reads the same decision point through the one fallback
		// chain (domain.AuthorizerFromContext).
		ctx = domain.WithAuthorizer(ctx, m.authz)
	}
	m.next.ServeHTTP(w, r.WithContext(ctx))
}

// principalFor turns the authenticated client name into the principal every
// downstream seam is scoped by — the D4 identity hop. The boolean is FALSE when
// this sender may not speak on this surface at all.
//
// Two rules make it safe to run in front of a fail-open OSS authorizer:
//
//   - Without a resolver the answer is unchanged from phase 1: the client's own
//     name IS the principal. That is the honest open-core position — the surface
//     is the identity, every token holder has identical reach — and installing a
//     binding registry is what makes them different senders.
//   - With a resolver the answer only ever gets NARROWER or is re-pointed by an
//     operator's explicit act. Nothing here can widen reach on its own.
//
// The kind stays PrincipalAgent across both paths, including for a re-pointed
// binding. An external coding agent IS an agent, the ADR requires a registered
// client to be scoped by the same agent_scopes machinery as an internal one, and
// holding the kind steady means a binding changes WHO is asking without silently
// changing HOW the decision point evaluates them — an agent principal the scope
// store has never heard of fails closed, which is the direction a re-point should
// fail in.
func (m *authMiddleware) principalFor(ctx context.Context, client string) (domain.PrincipalRef, bool) {
	externalID := principalPrefix + client
	self := domain.AgentPrincipal(externalID)
	if m.identity == nil {
		return self, true
	}
	// The profile carries the id resolution matches on plus the client name as a
	// LABEL, because resolution is also the only moment the system learns this
	// sender exists: an operator cannot bind a client they were never told about,
	// and a worklist of bare ids is one they cannot act on.
	//
	// The surface key is the bare KIND, `mcp`, where the chat ingresses pass
	// "chat:<ingress>". They differ because the referent differs: chat has many
	// registered entry points and a binding has to say which one, while a kernel
	// serves exactly one MCP endpoint — so "mcp:endpoint" would be a distinction
	// with nothing on the other side of it, and `mcp` is already the name a policy
	// author writes for this surface.
	profile := domain.SenderProfile{ExternalID: externalID, Username: client, DisplayName: client}
	if binding, bound := m.identity.ResolveIdentity(ctx, domain.SurfaceMCP, profile); bound {
		if binding.Blocked {
			// Dropped at the door, which is a stronger statement than "every
			// policy denies them" and does not depend on the policy set being right.
			return domain.PrincipalRef{}, false
		}
		// Only a `principal` binding names a principal. `group` and `room_group`
		// bind to a GROUP — a different container, whose reach this client gets by
		// membership rather than by impersonating it — so the client keeps its own
		// identity and the group's policy terms reach it through the decision
		// point, exactly as they do for an internal agent.
		if binding.BoundToKind == domain.BindPrincipal && binding.BoundToID != "" {
			return domain.AgentPrincipal(binding.BoundToID), true
		}
		return self, true
	}
	switch pol := m.identity.StrangerPolicyFor(ctx, domain.SurfaceMCP); pol.Mode {
	case domain.StrangerRefuseUntilBound:
		return domain.PrincipalRef{}, false
	case domain.StrangerGuestPrincipal:
		if pol.GuestPrincipalID != "" {
			return domain.AgentPrincipal(pol.GuestPrincipalID), true
		}
		// A guest mode with no guest named would otherwise silently fall back to
		// the client's own reach, which is the opposite of what was configured.
		return domain.PrincipalRef{}, false
	default:
		// StrangerSurfaceDefault, and anything a future mode adds: the surface is
		// the identity, which is precisely the pre-binding behaviour.
		return self, true
	}
}

// match resolves a presented token to the client that owns it.
//
// Constant-time comparison, and no early exit on a hit: returning as soon as a
// credential matched would make the response time report WHICH configured
// client the token belongs to, which is a smaller leak than the token itself but
// a leak that costs nothing to close.
func (m *authMiddleware) match(token string) (string, bool) {
	presented := []byte(token)
	name, found := "", false
	for _, c := range m.clients {
		secret, ok := m.resolve(ClientSecretName(c))
		if !ok || secret == "" {
			// No credential issued for this name. Skipped rather than compared,
			// so an empty stored secret can never be matched by an empty token.
			continue
		}
		if subtle.ConstantTimeCompare(presented, []byte(secret)) == 1 {
			name, found = c, true
		}
	}
	return name, found
}

// acquire takes one of the client's concurrency slots, or reports that they are
// all taken. The returned release is safe to call exactly once.
func (m *authMiddleware) acquire(client string) (func(), bool) {
	ch, known := m.sem[client]
	if !known {
		return func() {}, true
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is matched case-insensitively (RFC 7235); everything after it is the token.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}

// unauthorized answers a missing, malformed or unknown credential.
//
// No body and no detail: "unknown client" and "wrong token for a known client"
// must be the same answer, or the endpoint enumerates its own client roster for
// anyone who asks.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
}
