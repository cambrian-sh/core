package mcpserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0126 D4, phase 2: the IdentityResolver hop. Phase 1 bound the client name
// straight to a principal; a premium deployment resolves it through the binding
// registry instead, and this file pins BOTH shapes — because the OSS one is the
// honest open-core statement ("the surface is the identity") and a change to it
// would be a silent widening.

// stubResolver stands in for the premium binding registry.
type stubResolver struct {
	binding  domain.IdentityBinding
	bound    bool
	stranger domain.StrangerPolicy
	gotSurf  string
	gotProf  domain.SenderProfile
}

func (s *stubResolver) ResolveIdentity(_ context.Context, surface string, p domain.SenderProfile) (domain.IdentityBinding, bool) {
	s.gotSurf, s.gotProf = surface, p
	return s.binding, s.bound
}

func (s *stubResolver) StrangerPolicyFor(context.Context, string) domain.StrangerPolicy {
	return s.stranger
}

// identityMiddleware wraps next with a resolver installed (nil = the OSS shape).
func identityMiddleware(t *testing.T, next http.Handler, res domain.IdentityResolver) http.Handler {
	t.Helper()
	h, err := newAuthMiddleware(Options{
		Clients:       []string{"ci-bot"},
		ResolveSecret: staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"}),
		Identity:      res,
	}, next)
	if err != nil {
		t.Fatalf("newAuthMiddleware: %v", err)
	}
	return h
}

func authedRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer token-aaaa")
	return req
}

// No resolver installed: unchanged from phase 1. This is the OSS deployment, and
// its behaviour is a documented product statement, not an accident.
func TestIdentity_NoResolverBindsTheClientNameDirectly(t *testing.T) {
	next := &capture{}
	rec := httptest.NewRecorder()
	identityMiddleware(t, next, nil).ServeHTTP(rec, authedRequest())

	if !next.reached {
		t.Fatalf("the request did not reach the handler (status %d)", rec.Code)
	}
	if next.principal.ID != "mcp:ci-bot" || next.principal.Kind != domain.PrincipalAgent {
		t.Errorf("principal = %+v, want agent mcp:ci-bot", next.principal)
	}
}

// A resolver that binds the external id to a principal RE-POINTS the client: this
// is how an operator says "the ci-bot token speaks as the build-agent principal".
func TestIdentity_ResolverRepointsThePrincipal(t *testing.T) {
	res := &stubResolver{
		bound: true,
		binding: domain.IdentityBinding{
			Surface: domain.SurfaceMCP, ExternalID: "mcp:ci-bot",
			BoundToKind: domain.BindPrincipal, BoundToID: "build-agent",
		},
	}
	next := &capture{}
	identityMiddleware(t, next, res).ServeHTTP(httptest.NewRecorder(), authedRequest())

	if !next.reached {
		t.Fatal("a bound client was refused")
	}
	if next.principal.ID != "build-agent" {
		t.Errorf("principal = %q, want the bound principal build-agent", next.principal.ID)
	}
	// The kind is held STEADY across the hop: a binding changes who is asking, not
	// how the decision point evaluates them.
	if next.principal.Kind != domain.PrincipalAgent {
		t.Errorf("principal kind = %q, want %q", next.principal.Kind, domain.PrincipalAgent)
	}
	// Resolution is asked on the mcp surface, with the external id policy targets
	// and the client name as a label an operator can act on.
	if res.gotSurf != domain.SurfaceMCP {
		t.Errorf("resolver was asked about surface %q", res.gotSurf)
	}
	if res.gotProf.ExternalID != "mcp:ci-bot" || res.gotProf.Username != "ci-bot" {
		t.Errorf("profile = %+v", res.gotProf)
	}
}

// A GROUP binding does not turn the client into the group. The client keeps its
// own identity and the group's policy terms reach it through the decision point,
// exactly as they do for an internal agent.
func TestIdentity_GroupBindingKeepsTheClientsOwnPrincipal(t *testing.T) {
	for _, kind := range []string{domain.BindGroup, domain.BindRoomGroup} {
		next := &capture{}
		identityMiddleware(t, next, &stubResolver{
			bound: true,
			binding: domain.IdentityBinding{
				BoundToKind: kind, BoundToID: "coding-agents",
			},
		}).ServeHTTP(httptest.NewRecorder(), authedRequest())

		if !next.reached || next.principal.ID != "mcp:ci-bot" {
			t.Errorf("%s binding: principal = %+v, want mcp:ci-bot", kind, next.principal)
		}
	}
}

// A blocked sender is dropped AT THE DOOR — a stronger statement than "every
// policy denies them", and one that does not depend on the policy set being right.
// 403 with a reason, not the deliberately silent 401: the credential was valid, so
// the client's own logs must be able to tell a block from a wrong token.
func TestIdentity_BlockedClientIsRefusedAtTheDoor(t *testing.T) {
	next := &capture{}
	rec := httptest.NewRecorder()
	identityMiddleware(t, next, &stubResolver{
		bound:   true,
		binding: domain.IdentityBinding{Blocked: true, BoundToKind: domain.BindPrincipal, BoundToID: "whoever"},
	}).ServeHTTP(rec, authedRequest())

	if next.reached {
		t.Fatal("a blocked client reached a handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The stranger policy governs a client nobody bound — the same three modes every
// other surface has, with no MCP-specific vocabulary.
func TestIdentity_StrangerPolicyGovernsAnUnboundClient(t *testing.T) {
	cases := []struct {
		name      string
		policy    domain.StrangerPolicy
		wantAllow bool
		wantID    string
	}{
		{"surface default is the pre-binding behaviour",
			domain.StrangerPolicy{Mode: domain.StrangerSurfaceDefault}, true, "mcp:ci-bot"},
		{"no policy at all is the same",
			domain.StrangerPolicy{}, true, "mcp:ci-bot"},
		{"guest principal makes the reach deliberate",
			domain.StrangerPolicy{Mode: domain.StrangerGuestPrincipal, GuestPrincipalID: "guest"}, true, "guest"},
		{"guest mode naming no guest must not fall back to the client's own reach",
			domain.StrangerPolicy{Mode: domain.StrangerGuestPrincipal}, false, ""},
		{"refuse until bound",
			domain.StrangerPolicy{Mode: domain.StrangerRefuseUntilBound}, false, ""},
	}
	for _, tc := range cases {
		next := &capture{}
		rec := httptest.NewRecorder()
		identityMiddleware(t, next, &stubResolver{stranger: tc.policy}).ServeHTTP(rec, authedRequest())

		if next.reached != tc.wantAllow {
			t.Errorf("%s: reached=%v (status %d), want %v", tc.name, next.reached, rec.Code, tc.wantAllow)
			continue
		}
		if tc.wantAllow && next.principal.ID != tc.wantID {
			t.Errorf("%s: principal = %q, want %q", tc.name, next.principal.ID, tc.wantID)
		}
		if !tc.wantAllow && rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", tc.name, rec.Code)
		}
	}
}

// ── the week-6 half this exists for ──────────────────────────────────────────

// perPrincipalPDP denies exactly one principal, standing in for a real policy
// plane. It is the smallest thing that can prove the endpoint's decisions are
// per-CALLER rather than per-surface.
type perPrincipalPDP struct{ denied string }

func (p perPrincipalPDP) Authorize(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	if req.Principal.ID == p.denied {
		return domain.AccessDecision{
			Allowed: false, Resource: req.Resource, Principal: req.Principal, Surface: req.Surface,
			Reason: domain.ReasonNotAuthorized, Detail: "policy denies " + req.Principal.ID,
		}
	}
	return domain.AccessDecision{Allowed: true, Resource: req.Resource, Principal: req.Principal,
		Surface: req.Surface, Reason: domain.ReasonAllowed}
}

func (p perPrincipalPDP) Filter(context.Context, domain.PrincipalRef, domain.SurfaceRef, domain.ResourceKind, []domain.Taggable) ([]domain.Taggable, []domain.AccessDecision) {
	return nil, nil
}

func (p perPrincipalPDP) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return &domain.TagPredicate{}, domain.AccessDecision{Allowed: true}
}

func (p perPrincipalPDP) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: domain.ReasonNotAuthorized}
}

// "Two clients with different tokens receive DIFFERENT denials" — the ADR's own
// phase-2 exit clause, end to end over the wire.
func TestEndpoint_TwoClientsGetDifferentOutcomesForTheSameTool(t *testing.T) {
	tool := domain.PublishedToolEntry{
		Owner: "core",
		Tool: domain.PublishedTool{
			Name: "search_memory", Title: "Search memory", Description: "cheap retrieval",
			InputSchema: []byte(`{"type":"object"}`),
			Effects:     []domain.ToolEffect{domain.EffectRead}, ReadOnly: true,
		},
		Handler: &stubTool{result: domain.PublishedToolResult{Text: "allowed"}},
	}
	opts := Options{
		Surface: domain.PublishedToolSurface{tool},
		Clients: []string{"ci-bot", "reader"},
		ResolveSecret: staticSecrets(map[string]string{
			"mcp:client:ci-bot": "token-aaaa",
			"mcp:client:reader": "token-bbbb",
		}),
		Authorizer: perPrincipalPDP{denied: "mcp:ci-bot"},
	}

	// Denied client: the tool is absent from its menu (D8 listing filter) AND the
	// call is refused (D8 call side, which is what actually holds).
	denied := serve(t, opts, "token-aaaa")
	if list, err := denied.ListTools(t.Context(), nil); err != nil || len(list.Tools) != 0 {
		t.Fatalf("denied client's tools = %v (err %v), want none", list, err)
	}
	res, err := denied.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "search_memory"})
	if err != nil {
		t.Fatalf("a refusal must be a TOOL error, not a transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("the denied client's call succeeded")
	}

	// Allowed client, SAME tool, same server, different token.
	allowed := serve(t, opts, "token-bbbb")
	if list, err := allowed.ListTools(t.Context(), nil); err != nil || len(list.Tools) != 1 {
		t.Fatalf("allowed client's tools = %v (err %v), want the one tool", list, err)
	}
	ok, err := allowed.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "search_memory"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if ok.IsError {
		t.Fatalf("the allowed client was refused: %+v", ok.Content)
	}
}

// A plugin claiming a CORE tool's name is a boot error naming both owners.
// Registry.PublishTool rejects duplicates among plugins, but the composed surface
// is plugin tools appended to the core ones, so this is the only place that
// collision can be caught — and silently letting the second registration win
// would mean two answers to one question with no way to say which held.
func TestNewHandler_RefusesTwoOwnersForOneToolName(t *testing.T) {
	one := domain.PublishedTool{
		Name: "search_memory", InputSchema: []byte(`{"type":"object"}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
	_, err := NewHandler(Options{
		Clients:       []string{"ci-bot"},
		ResolveSecret: staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"}),
		Surface: domain.PublishedToolSurface{
			{Owner: "core", Tool: one, Handler: &stubTool{}},
			{Owner: "impostor", Tool: one, Handler: &stubTool{}},
		},
	})
	if err == nil {
		t.Fatal("two owners of one tool name composed successfully")
	}
	if !strings.Contains(err.Error(), "core") || !strings.Contains(err.Error(), "impostor") {
		t.Errorf("err = %v, want both claimants named", err)
	}
}

// ── D6, at the transport ─────────────────────────────────────────────────────

// correlationProbe reports the correlation handle it was invoked under, so a test
// can prove the endpoint seeded one BEFORE the handler ran.
type correlationProbe struct {
	ownRef string
	seen   string
	hops   []string
}

func (c *correlationProbe) Invoke(ctx context.Context, _ json.RawMessage) (domain.PublishedToolResult, error) {
	c.seen, _ = domain.QueryCorrelationFromContext(ctx)
	// Two hops, as a multi-hop retrieval would emit.
	for i := 0; i < 2; i++ {
		id, _ := domain.NextQueryCorrelationID(ctx)
		c.hops = append(c.hops, id)
	}
	return domain.PublishedToolResult{Text: "ok", ReceiptRef: c.ownRef}, nil
}

func correlationTool(h domain.PublishedToolHandler) domain.PublishedToolSurface {
	return domain.PublishedToolSurface{{
		Owner: "core",
		Tool: domain.PublishedTool{
			Name: "ask_memory", Title: "Ask memory", Description: "an answer",
			InputSchema: []byte(`{"type":"object"}`),
			Effects:     []domain.ToolEffect{domain.EffectRead}, ReadOnly: true,
		},
		Handler: h,
	}}
}

// Every tool call gets a handle, it reaches the handler on the context, the hops
// extend it, and it comes back on the result's `_meta` — which is the whole of
// what makes `get_receipt` reachable from a caller who only holds an answer.
func TestEndpoint_MintsACorrelationHandleAndReturnsIt(t *testing.T) {
	probe := &correlationProbe{}
	session := serve(t, Options{Surface: correlationTool(probe)}, "token-aaaa")

	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "ask_memory"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	ref, _ := res.Meta[ReceiptMetaKey].(string)
	if ref == "" {
		t.Fatal("no receipt handle on the result's _meta")
	}
	if probe.seen != ref {
		t.Errorf("handler saw correlation %q, caller was told %q — they must be the same handle", probe.seen, ref)
	}
	if len(probe.hops) != 2 || probe.hops[0] != ref+"-h1" || probe.hops[1] != ref+"-h2" {
		t.Errorf("hops = %v, want %s-h1 and %s-h2", probe.hops, ref, ref)
	}

	// Two calls are two handles: a caller fetching one call's receipts must not
	// get another's.
	second, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "ask_memory"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if ref2, _ := second.Meta[ReceiptMetaKey].(string); ref2 == ref {
		t.Errorf("two calls shared the handle %q", ref)
	}
}

// A handler that produced its own handle keeps it: get_receipt answers about a
// handle the CALLER supplied, and overwriting it would make the tool report on
// the wrong call.
func TestEndpoint_HandlerSuppliedReceiptRefIsPreserved(t *testing.T) {
	probe := &correlationProbe{ownRef: "mcp-supplied-by-the-handler"}
	session := serve(t, Options{Surface: correlationTool(probe)}, "token-aaaa")

	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "ask_memory"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if ref, _ := res.Meta[ReceiptMetaKey].(string); ref != "mcp-supplied-by-the-handler" {
		t.Errorf("receipt ref = %q, want the handler's own", ref)
	}
	// It still ran under a seeded correlation — the handler's choice of what to
	// REPORT does not change what the call is recorded under.
	if probe.seen == "" {
		t.Error("no correlation was seeded for a handler that supplies its own ref")
	}
}
