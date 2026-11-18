package operator

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Role is the operator-plane authorization role (ADR-0047 D13), DERIVED from the
// person's role set (ADR-0142 D1) by RoleFromRoles.
type Role string

const (
	RoleOperator Role = "operator" // full surface, including mutating commands
	RoleViewer   Role = "viewer"   // read + feed only; commands denied
	// RoleMember is a signed-in person with no console role (ADR-0142): an OIDC user
	// the claim map gave no operator/viewer role, a local account created for chat.
	// They may use only their OWN surfaces on this plane — the canonical conversation
	// lane (owner-scoped, act-for refused), their own accounts, Logout — and nothing
	// that reads or changes the deployment. See memberMethods.
	RoleMember Role = "member"
)

// RoleFromRoles maps a person's role set to the operator-plane role: `operator`
// (or `admin`) wins, then `viewer`, else member. Plane access stays a role check;
// what the person may READ stays the Authorizer's decision.
func RoleFromRoles(roles []string) Role {
	has := func(r string) bool {
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}
	switch {
	case has(string(RoleOperator)) || has("admin"):
		return RoleOperator
	case has(string(RoleViewer)):
		return RoleViewer
	default:
		return RoleMember
	}
}

// operatorMethodPrefix scopes the interceptors: OperatorConsole RPCs are gated,
// and agent-facing Orchestrator/AgentService RPCs (UDS) pass through — except
// the operator-gated approval methods (authz.IsOperatorGatedAgentServiceMethod,
// ADR-0039 D10), which take the bearer like the console.
const operatorMethodPrefix = "/cambrian.OperatorConsole/"

// premiumMethodPrefix covers plugin-mounted planes (ADR-0073/ADR-0118 D3).
// Premium services are OPERATOR-plane by default and require the same bearer
// as the console — the pre-0118 gap where they passed through unauthenticated
// is exactly what three comments wrongly claimed did not exist. A premium
// service is exempt only when it was DECLARED agent-facing at registration
// (Registry.AddAgentGRPCService), in which case the agent plane's own
// semantics apply (x-agent-id principal, SurfaceAgent).
const premiumMethodPrefix = "/cambrian.premium."

// loginMethod and describeAuthMethod are the OperatorConsole RPCs reachable
// without a token: one signs in, the other says how to (ADR-0142 D1).
const (
	loginMethod        = operatorMethodPrefix + "Login"
	describeAuthMethod = operatorMethodPrefix + "DescribeAuth"
)

type principalCtxKey struct{}

type principalInfo struct {
	principal string
	role      Role
}

// PrincipalFromContext returns the authenticated principal and role, if any.
func PrincipalFromContext(ctx context.Context) (principal string, role Role, ok bool) {
	v, ok := ctx.Value(principalCtxKey{}).(principalInfo)
	if !ok {
		return "", "", false
	}
	return v.principal, v.role, true
}

// The operator token TTL (ADR-0130 1f, 12 h absolute) now lives with the backend
// that mints tokens: identity.SessionTTL.

// The V1 StaticIdentity (one env user, plaintext compare in memory) is gone:
// ADR-0142 D1 replaced it with domain.Authenticator, whose OSS default is
// internal/identity.Chain (local accounts with argon2id hashes, plus one optional
// OIDC issuer). The env bootstrap operator is now a local account re-asserted on
// every boot (app.buildAuthenticator), so a deployment upgrades with no change.

// bearerToken extracts the token from "authorization: Bearer <token>" metadata.
func bearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(vals[0], "Bearer "))
}

// serviceOf extracts "pkg.Service" from "/pkg.Service/Method".
func serviceOf(fullMethod string) string {
	rest := strings.TrimPrefix(fullMethod, "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// authorize is the shared gate for both interceptors. It returns the principal
// context for OperatorConsole and premium operator-plane methods, passing
// through the agent plane (core and declared-premium alike).
//
// roles are the premium plugins' declared method classes (MethodRoles); nil means
// none declared, and every premium method then follows the naming rule.
func authorize(ctx context.Context, fullMethod string, idp domain.Authenticator, agentPlane map[string]bool, roles MethodRoles) (context.Context, error) {
	// The approval surface is declared on Orchestrator but is operator-plane
	// (ADR-0039 D10): it takes the same bearer as the console. The list lives in
	// authz.IsOperatorGatedAgentServiceMethod so the surface stamp and this gate
	// cannot disagree about which methods it covers.
	operatorConsole := strings.HasPrefix(fullMethod, operatorMethodPrefix) ||
		authz.IsOperatorGatedAgentServiceMethod(fullMethod)
	premium := strings.HasPrefix(fullMethod, premiumMethodPrefix)
	// Agent-facing RPCs (core, and premium services DECLARED agent-facing) are
	// untouched: the agent plane's own semantics apply.
	if !operatorConsole && !premium {
		return ctx, nil
	}
	if premium && agentPlane[serviceOf(fullMethod)] {
		return ctx, nil
	}
	// Login and DescribeAuth are the only unauthenticated operator RPCs.
	if fullMethod == loginMethod || fullMethod == describeAuthMethod {
		return ctx, nil
	}
	if idp == nil {
		return ctx, status.Error(codes.Unauthenticated, "no authenticator configured")
	}
	id, err := idp.Resolve(ctx, bearerToken(ctx))
	if err != nil || id.Principal.IsZero() {
		return ctx, ResolveRefusal(ctx, err)
	}
	principal, role := id.Principal.ID, RoleFromRoles(id.Roles)
	// A member reaches only its own surfaces (memberMethods, or a premium method its
	// plugin declared self-service / member-read), fail-closed.
	if role == RoleMember && !memberMayCall(fullMethod, roles) {
		return ctx, status.Errorf(codes.PermissionDenied, "role %q may not call %s", role, fullMethod)
	}
	// Viewer is denied mutating commands; reads/feed are allowed for any role.
	// Premium operator planes follow the same naming convention, fail-closed:
	// anything not spelled like a read is a command.
	if role == RoleViewer && isOperatorOnly(fullMethod, roles) {
		return ctx, status.Errorf(codes.PermissionDenied, "role %q may not call %s", role, fullMethod)
	}
	ctx = context.WithValue(ctx, principalCtxKey{}, principalInfo{principal: principal, role: role})
	// ADR-0142 D11: the approval hub authorises an answer against the person's
	// FULL role set (role:compliance, …), not just the plane role derived above.
	ctx = domain.WithPrincipalRoles(ctx, id.Roles)
	// And the DOMAIN principal, once, for every operator-plane RPC.
	//
	// The line above is an operator-plane private value; the rest of the kernel
	// reads domain.PrincipalFromContext. Setting only the private one meant every
	// operator RPC reached the domain as an anonymous caller one stack frame after
	// authenticating, so anything that authorizes, records ownership or writes
	// provenance saw `principal:<none>`.
	//
	// The failure was asymmetric, which is what let it survive: OSS fails OPEN on
	// an unknown principal, so every OSS test passed, while a premium deployment
	// fails CLOSED — the documented path was broken in exactly the deployments
	// that enforce access control. It was patched at one call site
	// (substrate/operator ingest) instead, which is a workaround, not the fix.
	//
	// ADR-0142 D3: the principal is the Authenticator's registry person (kind
	// user), not a bare username re-wrapped here.
	return domain.WithPrincipal(ctx, id.Principal), nil
}

// ResolveRefusal is the gRPC answer to a failed Authenticator.Resolve. Every
// branch REFUSES the call (fail closed); they differ only in what the client is
// told to do next:
//
//   - the token could not be checked because a store the check needs did not
//     answer (domain.ErrAuthUnavailable — Postgres down, the IdP unreachable):
//     Unavailable, which gRPC clients retry with backoff. Answering it as
//     Unauthenticated told every UI/SDK to drop a session whose token was good;
//     a 30 s Postgres stop logged everyone out
//     (T-20261118-operator-rpcs-answer-unauthenticated-invalid-or);
//   - the caller's own deadline or cancellation: the matching context code;
//   - anything else, including an unknown error from a plugin Authenticator:
//     one undetailed Unauthenticated for unknown, expired, badly signed,
//     wrong-audience and suspended alike; the backend logged which.
func ResolveRefusal(ctx context.Context, err error) error {
	switch {
	case err != nil && ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case errors.Is(err, domain.ErrAuthUnavailable):
		return status.Error(codes.Unavailable, "authentication temporarily unavailable; retry")
	default:
		return status.Error(codes.Unauthenticated, "invalid or expired token")
	}
}

// memberMethods are the operator-plane RPCs a RoleMember may call (ADR-0142).
// Each handler scopes to the caller: the conversation lane checks ownership and
// refuses act-for to every role but operator (CanActFor), and the accounts
// self-service methods name no principal. Matched on the FULL method name.
var memberMethods = map[string]bool{
	operatorMethodPrefix + "Logout": true,
	// Contract 0102: a person asks who the kernel thinks they are.
	operatorMethodPrefix + "WhoAmI":                   true,
	operatorMethodPrefix + "OpenConversation":         true,
	operatorMethodPrefix + "SendTurn":                 true,
	operatorMethodPrefix + "CloseConversation":        true,
	operatorMethodPrefix + "ListConversations":        true,
	operatorMethodPrefix + "ListConversationMessages": true,
	// ADR-0142 D11/D12: a person answers their OWN approvals, manages their own
	// standing approvals and shares their own private material. Each handler
	// scopes to the caller; the hub authorises every answer at answer time.
	operatorMethodPrefix + "WatchMyApprovals":       true,
	operatorMethodPrefix + "ListApprovals":          true,
	operatorMethodPrefix + "AnswerApproval":         true,
	operatorMethodPrefix + "ListStandingApprovals":  true,
	operatorMethodPrefix + "RevokeStandingApproval": true,
	operatorMethodPrefix + "ShareToTeam":            true,
	// Premium planes' member surfaces (their own accounts, their own reporting
	// chain) are DECLARED by the plugin that serves them (MethodRoles), not listed
	// here: core names no premium method.
}

// memberMayCall: an OperatorConsole member method, or a premium method its plugin
// declared self-service or member-read.
func memberMayCall(fullMethod string, roles MethodRoles) bool {
	if memberMethods[fullMethod] {
		return true
	}
	r, ok := roles.declared(fullMethod)
	return ok && (r == MethodSelfService || r == MethodMemberRead)
}

// isOperatorOnly reports whether an OperatorConsole method is a mutating command
// (Operator-only). Reads (StreamEvents/Snapshot/Login and Query* reads) are open
// to any authenticated role. Command RPCs are added by later slices; this list
// is the single gate point. ADR-0047 D13.
// operatorOnlyReads are READS that a Viewer must not make.
//
// The prefix rule below is fail-closed for unknown names but fail-OPEN for
// anything called Query*/List*/Get* — so a read simply named `QueryLogs` would
// inherit Viewer access by its spelling. That is the wrong default here: the log
// stream carries whatever the kernel happened to write, which routinely includes
// more than any policy would disclose, and it bypasses the access-policy plane
// entirely. Logs are operator-only until they can be filtered per principal.
var operatorOnlyReads = map[string]bool{
	"QueryLogs": true,
	"TailLogs":  true,
	// Every principal's recurring tasks (scheduled_tasks.go) — other people's
	// business, like ListAccounts.
	"ListScheduledTasks": true,
	// Memory erasure receipts (ADR-0137, contract 0105): content-free, but they list
	// every principal's erasures, and the compliance erase they record is operator-only
	// (owner decision 2026-11-16).
	"ListErasures": true,
	"GetErasure":   true,
}

// selfServiceMethods are the OperatorConsole methods open to every authenticated
// role although named like commands. No request field names a principal. The
// premium planes' self-service methods (connect / disconnect / confirm my
// account, read my org chain) are declared by their plugins (MethodRoles).
var selfServiceMethods = map[string]bool{
	// ADR-0142 D11/D12: answering one's own approvals, revoking one's own
	// standing approvals, sharing one's own material — the hub and the share
	// policy decide; the plane role does not.
	operatorMethodPrefix + "WatchMyApprovals":       true,
	operatorMethodPrefix + "AnswerApproval":         true,
	operatorMethodPrefix + "RevokeStandingApproval": true,
	operatorMethodPrefix + "ShareToTeam":            true,
}

// isOperatorOnly: the premium planes' operator-only READS (ListAccounts,
// ListScimConnections, GetOntologyDraft, ...) are declared by their plugins as
// MethodOperatorOnly and checked before the naming rule, like operatorOnlyReads.
func isOperatorOnly(fullMethod string, roles MethodRoles) bool {
	name := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		name = fullMethod[i+1:]
	}
	switch name {
	// Logout is open to every role: a Viewer must be able to end its own session,
	// and the token it revokes is the caller's own.
	// WhoAmI (contract 0102) is the caller reading its own identity.
	case "Login", "Logout", "WhoAmI", "StreamEvents", "Snapshot":
		return false
	}
	if selfServiceMethods[fullMethod] {
		return false
	}
	if r, ok := roles.declared(fullMethod); ok {
		switch r {
		case MethodSelfService:
			return false
		case MethodOperatorOnly:
			return true
		}
		// MethodMemberRead: an ordinary read for Operators and Viewers; the
		// naming rule below decides, as for any undeclared method.
	}
	// Checked BEFORE the naming convention, so a read cannot open itself to a
	// Viewer by being named like one.
	if operatorOnlyReads[name] {
		return true
	}
	// Read RPCs are conventionally named Query*/List*/Get*/Describe*; everything
	// else under OperatorConsole is treated as a mutating command (fail-closed).
	for _, readPrefix := range []string{"Query", "List", "Get", "Describe"} {
		if strings.HasPrefix(name, readPrefix) {
			return false
		}
	}
	return true
}

// UnaryAuthInterceptor gates unary OperatorConsole and premium operator-plane
// RPCs (ADR-0047 D13; ADR-0118 D3). agentPlane names the premium services
// DECLARED agent-facing at registration — exempt from the operator bearer.
// roles are the premium plugins' declared method classes (nil: none declared).
func UnaryAuthInterceptor(idp domain.Authenticator, agentPlane map[string]bool, roles MethodRoles) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := authorize(ctx, info.FullMethod, idp, agentPlane, roles)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor gates streaming OperatorConsole and premium
// operator-plane RPCs (e.g. StreamEvents).
func StreamAuthInterceptor(idp domain.Authenticator, agentPlane map[string]bool, roles MethodRoles) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := authorize(ss.Context(), info.FullMethod, idp, agentPlane, roles)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

// wrappedStream carries the principal context into the stream handler.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
