package mcpserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
)

// ADR-0142 D3 on the MCP door: a bound person is `user`, a suspended person is
// refused, and a worker machine's owner is the same `user` principal a task
// beneficiary resolves to (feasibility K-7).

func personMiddleware(t *testing.T, next http.Handler, res domain.IdentityResolver, reg domain.PrincipalRegistry,
	owner func(string) (string, bool)) http.Handler {
	t.Helper()
	h, err := newAuthMiddleware(Options{
		Clients: []string{"ci-bot", "laptop"},
		ResolveSecret: staticSecrets(map[string]string{
			"mcp:client:ci-bot": "token-aaaa", "mcp:client:laptop": "token-mmmm",
		}),
		Identity:    res,
		Principals:  reg,
		WorkerOwner: owner,
	}, next)
	if err != nil {
		t.Fatalf("newAuthMiddleware: %v", err)
	}
	return h
}

func boundTo(id string) *stubResolver {
	return &stubResolver{bound: true, binding: domain.IdentityBinding{
		Surface: domain.SurfaceMCP, ExternalID: "mcp:ci-bot", BoundToKind: domain.BindPrincipal, BoundToID: id,
	}}
}

func TestPersonKind_BoundPersonIsUserOnMCP(t *testing.T) {
	t.Parallel()
	next := &capture{}
	personMiddleware(t, next, boundTo("alice"), identity.NewMemRegistry(), nil).
		ServeHTTP(httptest.NewRecorder(), authedRequest())
	if !next.reached || next.principal != domain.UserPrincipal("alice") {
		t.Fatalf("principal = %v (reached %v), want user:alice", next.principal, next.reached)
	}
}

func TestPersonKind_SuspendedPersonRefusedOnMCP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetStatus(ctx, "alice", domain.PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	next := &capture{}
	rec := httptest.NewRecorder()
	personMiddleware(t, next, boundTo("alice"), reg, nil).ServeHTTP(rec, authedRequest())
	if next.reached || rec.Code != http.StatusForbidden {
		t.Fatalf("a suspended person must be refused at the door; status %d reached %v", rec.Code, next.reached)
	}
}

func TestPersonKind_WorkerOwnerIsUserAndSuspensionDarkensTheMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	owner := func(c string) (string, bool) {
		if c == "laptop" {
			return "alice", true
		}
		return "", false
	}
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Authorization", "Bearer token-mmmm")
		return r
	}
	next := &capture{}
	personMiddleware(t, next, nil, reg, owner).ServeHTTP(httptest.NewRecorder(), req())
	if next.workerOwner != domain.UserPrincipal("alice") {
		t.Fatalf("worker owner = %v, want user:alice (must equal the beneficiary kind)", next.workerOwner)
	}
	if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	_ = reg.SetStatus(ctx, "alice", domain.PrincipalDeprovisioned)
	next2 := &capture{}
	rec := httptest.NewRecorder()
	personMiddleware(t, next2, nil, reg, owner).ServeHTTP(rec, req())
	if next2.reached || rec.Code != http.StatusForbidden {
		t.Fatalf("a deprovisioned owner's machine must go dark; status %d", rec.Code)
	}
}

// An owner naming an unbound MCP client's own principal stays an agent, so a
// machine token issued to `mcp:<client>` before ADR-0142 keeps serving that
// agent's tasks.
func TestPersonKind_MCPClientOwnerStaysAgent(t *testing.T) {
	t.Parallel()
	if got := domain.PersonPrincipal("mcp:claude"); got != domain.AgentPrincipal("mcp:claude") {
		t.Fatalf("PersonPrincipal(mcp:claude) = %v", got)
	}
	if got := domain.PersonPrincipal("alice"); got != domain.UserPrincipal("alice") {
		t.Fatalf("PersonPrincipal(alice) = %v", got)
	}
}
