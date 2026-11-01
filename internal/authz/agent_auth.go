package authz

import (
	"context"
	"crypto/subtle"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Agent-plane client authentication (ADR-0130 D1/D2 — the SEC-03 that was
// deferred by ADR-0085, ADR-0118 and ADR-0126 and never written).
//
// Before this file the agent plane's principal was whatever the caller wrote in
// the x-agent-id header. On a routable bind that let any host on the network
// read an agent's scope, run its granted tools and ingest as it; on loopback,
// any local process could. The MCP endpoint (ADR-0126) already had the right
// shape — a per-client named secret in the ADR-0101 store, presented as a
// bearer token and matched constant-time — so the owner chose D1(b): the same
// pattern, per agent.
//
// The rules:
//
//   - The credential is the named secret `agent:<agent_id>:token`
//     (AgentTokenSecretName), presented as `authorization: Bearer <secret>`.
//   - `x-agent-id` is an ASSERTION, never the source of identity. It names
//     which stored secret to compare against, and the compare is what decides.
//     A presented token that matches a different agent's secret is refused:
//     the header and the credential must name one principal.
//   - D2: on a deployment with server.insecure_localhost=true the requirement is
//     WAIVED — the flag already means "this host is trusted" for the transport,
//     and the benchmark harness and the loopback tests run under it. The waiver
//     keeps the pre-ADR-0130 behaviour exactly (x-agent-id is the principal).
//   - Enforcement is constructor-injected (AGENTS.md §2.3): the verifier is a
//     required argument of the interceptors, and nothing attaches it later.
//
// The principal is established HERE, once, for every agent-plane method —
// core Orchestrator RPCs and premium services declared agent-facing (ADR-0118
// D3) alike — so a handler never reads its own identity claims (INV-5).
// ─────────────────────────────────────────────────────────────────────────────

// AgentTokenSecretName is the ADR-0101 named secret an agent authenticates
// with. One name shape, used by the verifier, the agent manager (which injects
// the value into the agent's process environment at spawn) and `setup` (which
// mints it), so none of the three can drift.
func AgentTokenSecretName(agentID string) string {
	return "agent:" + agentID + ":token"
}

// VerifyNamedSecret compares a presented token against ONE named stored
// secret. THE compare for every bearer-shaped credential the kernel accepts:
// the MCP endpoint's client match and machine-token check (mcpserve) and the
// agent plane (this file) all call it, so there is one constant-time
// comparison in the tree rather than three that could drift.
//
// Fail-closed on every absence: no resolver, no stored value (never issued or
// revoked), an empty presented token, a mismatch. An empty stored secret is
// never matched by an empty token — it is skipped, not compared.
func VerifyNamedSecret(resolve func(name string) (string, bool), name, token string) bool {
	if resolve == nil || name == "" || token == "" {
		return false
	}
	secret, ok := resolve(name)
	if !ok || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

// AgentCredentialVerifier decides whether an agent-plane request is
// authenticated. Built once at the composition root and handed to the
// interceptors; it holds the late-bound named-secret resolver, so a credential
// minted or rotated while the kernel runs is honoured on the next request.
type AgentCredentialVerifier struct {
	resolve func(name string) (string, bool)
	// waived is ADR-0130 D2: server.insecure_localhost=true disables agent
	// authentication along with the transport refusal it already disables.
	waived bool
}

// NewAgentCredentialVerifier builds the verifier. A nil resolver is legal and
// means NO agent can ever authenticate (the ADR-0101 store is off): every
// agent-plane call is refused unless waived. That is the fail-closed direction;
// the composition root says so at boot rather than this constructor failing,
// because a kernel that cannot authenticate agents is still a kernel the
// operator plane can reach to fix the configuration.
func NewAgentCredentialVerifier(resolve func(name string) (string, bool), waived bool) *AgentCredentialVerifier {
	return &AgentCredentialVerifier{resolve: resolve, waived: waived}
}

// Waived reports whether ADR-0130 D2 disabled agent authentication.
func (v *AgentCredentialVerifier) Waived() bool { return v != nil && v.waived }

// IsAgentPlaneMethod reports whether fullMethod is authenticated by the agent
// credential: a core Orchestrator RPC that is not operator-gated (those take the
// operator bearer, ADR-0039 D10), or a premium service declared agent-facing.
func IsAgentPlaneMethod(fullMethod string, agentPlane map[string]bool) bool {
	switch {
	case operatorGatedAgentServiceMethods[fullMethod]:
		return false
	case strings.HasPrefix(fullMethod, agentServicePrefix):
		return true
	case strings.HasPrefix(fullMethod, premiumServicePrefix):
		return agentPlane[serviceNameOf(fullMethod)]
	}
	return false
}

// errAgentUnauthenticated is the ONE answer for a missing, malformed, unknown
// or mismatched agent credential. "Unknown agent" and "wrong token for a known
// agent" must be the same answer, or the plane enumerates the agent roster for
// anyone who asks.
var errAgentUnauthenticated = status.Error(codes.Unauthenticated,
	"agent credential required: present `authorization: Bearer <token>` with `x-agent-id` naming the agent it was issued to (ADR-0130)")

// Authenticate establishes the agent principal for an agent-plane method, or
// refuses the call. Non-agent-plane methods pass through untouched.
func (v *AgentCredentialVerifier) Authenticate(ctx context.Context, fullMethod string, agentPlane map[string]bool) (context.Context, error) {
	if !IsAgentPlaneMethod(fullMethod, agentPlane) {
		return ctx, nil
	}
	asserted := incomingMetadataValue(ctx, "x-agent-id")
	if v == nil || v.waived {
		// D2 waiver: the header is the principal, exactly as before ADR-0130. An
		// absent header leaves the context anonymous, which the handlers that
		// read x-agent-id themselves already fail closed on.
		if asserted != "" {
			return domain.WithPrincipal(ctx, domain.AgentPrincipal(asserted)), nil
		}
		return ctx, nil
	}
	token := bearerFromMetadata(ctx)
	if token == "" || asserted == "" {
		// A token with no x-agent-id cannot be checked (the secret is named by
		// the agent) and an x-agent-id with no token is the pre-ADR-0130 hole.
		return ctx, errAgentUnauthenticated
	}
	if !VerifyNamedSecret(v.resolve, AgentTokenSecretName(asserted), token) {
		return ctx, errAgentUnauthenticated
	}
	return domain.WithPrincipal(ctx, domain.AgentPrincipal(asserted)), nil
}

// UnaryAgentAuthInterceptor authenticates unary agent-plane RPCs (ADR-0130 D1).
// It runs AFTER the surface interceptor (the surface is a fact about the
// transport, established before any credential is examined) and BEFORE the
// operator auth interceptor, which gates the other planes.
func UnaryAgentAuthInterceptor(v *AgentCredentialVerifier, agentPlane map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := v.Authenticate(ctx, info.FullMethod, agentPlane)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAgentAuthInterceptor is the streaming counterpart (SignalStream, the
// generation stream).
func StreamAgentAuthInterceptor(v *AgentCredentialVerifier, agentPlane map[string]bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := v.Authenticate(ss.Context(), info.FullMethod, agentPlane)
		if err != nil {
			return err
		}
		return handler(srv, &surfaceStream{ServerStream: ss, ctx: ctx})
	}
}

// incomingMetadataValue returns the first value of a metadata key, or "".
func incomingMetadataValue(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get(key); len(vals) > 0 {
		return strings.TrimSpace(vals[0])
	}
	return ""
}

// bearerFromMetadata extracts the credential from `authorization: Bearer
// <token>` metadata. The scheme is matched case-insensitively (RFC 7235).
func bearerFromMetadata(ctx context.Context) string {
	scheme, rest, found := strings.Cut(strings.TrimSpace(incomingMetadataValue(ctx, "authorization")), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}
