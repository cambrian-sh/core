package authz

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
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
//   - The credential is presented as `authorization: Bearer <token>`. For an
//     agent the kernel spawns it is the per-instance token minted at spawn and
//     held in kernel memory (ADR-0157 D4, instance_tokens.go); for an external
//     agent it is the named secret `agent:<agent_id>:token`
//     (AgentTokenSecretName). A kernel-spawned agent is refused on the stored
//     secret.
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

// AgentTokenSecretName is the ADR-0101 named secret an EXTERNAL agent (one the
// kernel does not spawn) authenticates with. One name shape, used by the
// verifier, boot-time minting and the ADR-0157 Q4 retirement of kernel-spawned
// agents' secrets (app/agent_credentials.go), so none of them can drift.
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
// interceptors. It accepts two credential kinds (ADR-0157 D4.5):
//
//   - (a) a LIVE per-instance token (InstanceTokens) whose agent id equals the
//     asserted x-agent-id — what every agent the kernel spawns presents;
//   - (b) the stored named secret `agent:<id>:token`, late-bound through the
//     resolver — ONLY for an agent the kernel does not spawn (an external SDK
//     agent, ADR-0130's hand-started case). A kernel-spawned agent is refused on
//     this path, so a leaked stored secret cannot impersonate a kernel-managed
//     agent once agents run sandboxed.
//
// The same InstanceTokens registry answers the SignalStream gate
// (agentmgr.InstanceManager.FindInstanceByToken), so both checks read one
// mint/revoke registry (D4.3) — there is no second token store.
type AgentCredentialVerifier struct {
	resolve func(name string) (string, bool)
	// instances is the kernel's in-memory registry of live per-instance tokens.
	instances *InstanceTokens
	// kernelSpawned reports whether the kernel spawns agentID itself (a
	// registered agent with a process runtime). It must fail CLOSED: when it
	// cannot tell, it answers true and the stored-secret path is refused.
	kernelSpawned func(ctx context.Context, agentID string) bool
	// waived is ADR-0130 D2: server.insecure_localhost=true disables agent
	// authentication along with the transport refusal it already disables.
	waived bool
}

// NewAgentCredentialVerifier builds the verifier. instances and kernelSpawned
// are REQUIRED (AGENTS.md §2.3): without the instance registry no spawned agent
// could authenticate, and without the predicate the stored-secret path could
// not refuse kernel-spawned agents — both are refused at construction rather
// than discovered as a silent hole. A nil resolver is legal and means no
// EXTERNAL agent can authenticate (the ADR-0101 store is off); kernel-spawned
// agents still authenticate with their instance tokens.
func NewAgentCredentialVerifier(resolve func(name string) (string, bool), instances *InstanceTokens,
	kernelSpawned func(ctx context.Context, agentID string) bool, waived bool) (*AgentCredentialVerifier, error) {
	if instances == nil {
		return nil, errors.New("agent credential verifier: instance token registry is required (ADR-0157 D4)")
	}
	if kernelSpawned == nil {
		return nil, errors.New("agent credential verifier: kernel-spawned predicate is required (ADR-0157 D4.5)")
	}
	return &AgentCredentialVerifier{resolve: resolve, instances: instances, kernelSpawned: kernelSpawned, waived: waived}, nil
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
	if v == nil || (v.waived && !routablePeer(ctx)) {
		// D2 waiver, SCOPED to the host it is named for (ADR-0142 K-2): it
		// applies only to a caller on a loopback address (or an in-process call
		// with no network peer). A caller from another machine on a routable bind
		// — which insecure_localhost is what permits — still needs the credential,
		// so the flag no longer means "anyone on the network is any agent".
		//
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
	instance, ok := v.verify(ctx, asserted, token)
	if !ok {
		return ctx, errAgentUnauthenticated
	}
	ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal(asserted))
	// ADR-0157 D5.2: the instance the token was minted for is the provenance of
	// what this call writes (UploadArtifact, out/ collection) — a kernel fact,
	// never a request field. "" on the stored-secret path (external agents).
	return domain.WithAgentInstance(ctx, instance), nil
}

// verify is the credential decision for one (asserted agent, presented token)
// pair (ADR-0157 D4.5). Every refusal is the same answer to the caller. On
// path (a) it also returns the instance the token belongs to.
func (v *AgentCredentialVerifier) verify(ctx context.Context, asserted, token string) (instance string, ok bool) {
	if id, found := v.instances.Lookup(token); found {
		// (a) A live instance token decides by itself: it names one agent, and
		// the header must name the same one. A token of another agent is refused
		// here, never retried against the stored secret.
		if id.AgentID != asserted {
			return "", false
		}
		return id.InstanceID, true
	}
	// (b) The stored named secret — only for an agent the kernel does not spawn.
	if v.kernelSpawned(ctx, asserted) {
		return "", false
	}
	return "", VerifyNamedSecret(v.resolve, AgentTokenSecretName(asserted), token)
}

// routablePeer reports whether the call came over the network from a
// NON-loopback address. An absent peer, or one whose address is not an IP
// (bufconn, a unix socket), is local by construction.
func routablePeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return false
	}
	host := p.Addr.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback()
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
