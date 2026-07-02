package operator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Role is the operator-plane authorization role (ADR-0047 D13).
type Role string

const (
	RoleOperator Role = "operator" // full surface, including mutating commands
	RoleViewer   Role = "viewer"   // read + feed only; commands denied
)

// operatorMethodPrefix scopes the interceptors: only OperatorConsole RPCs are
// gated, so agent-facing Orchestrator/AgentService RPCs (UDS) pass through.
const operatorMethodPrefix = "/cambrian.OperatorConsole/"

// loginMethod is the one OperatorConsole RPC reachable without a token.
const loginMethod = operatorMethodPrefix + "Login"

// OperatorIdentity is the auth port (ADR-0047 D13). The production backend
// (OIDC/SSO/directory) is deferred behind it; V1 ships StaticIdentity. A single
// global shared secret is intentionally not modelled — every principal is named
// so the audit actor and role are attributable.
type OperatorIdentity interface {
	// Login verifies credentials and returns a token bound to {principal, role}.
	Login(username, password string) (token, principal string, role Role, err error)
	// Resolve maps a previously-issued token to its principal and role.
	Resolve(token string) (principal string, role Role, err error)
}

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

// StaticIdentity is the V1 OperatorIdentity stub: a fixed user table and an
// in-memory token store. Concurrency-safe. The production backend swaps in
// behind the OperatorIdentity port with no interceptor change.
type StaticIdentity struct {
	mu     sync.Mutex
	users  map[string]staticUser   // username → {password, role}
	tokens map[string]principalInfo // token → {principal, role}
}

type staticUser struct {
	password string
	role     Role
}

// NewStaticIdentity builds a StaticIdentity from a username→(password,role) table.
func NewStaticIdentity(users map[string]struct {
	Password string
	Role     Role
}) *StaticIdentity {
	m := make(map[string]staticUser, len(users))
	for u, c := range users {
		m[u] = staticUser{password: c.Password, role: c.Role}
	}
	return &StaticIdentity{users: m, tokens: make(map[string]principalInfo)}
}

// Login implements OperatorIdentity.
func (s *StaticIdentity) Login(username, password string) (string, string, Role, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	if !ok || u.password != password {
		return "", "", "", status.Error(codes.Unauthenticated, "invalid credentials")
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", status.Error(codes.Internal, "token generation failed")
	}
	token := hex.EncodeToString(b[:])
	s.tokens[token] = principalInfo{principal: username, role: u.role}
	return token, username, u.role, nil
}

// Resolve implements OperatorIdentity.
func (s *StaticIdentity) Resolve(token string) (string, Role, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.tokens[token]
	if !ok {
		return "", "", status.Error(codes.Unauthenticated, "invalid or expired token")
	}
	return v.principal, v.role, nil
}

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

// authorize is the shared gate for both interceptors. It returns the principal
// context for OperatorConsole methods, passing through everything else.
func authorize(ctx context.Context, fullMethod string, idp OperatorIdentity) (context.Context, error) {
	// Only gate the operator plane; agent-facing RPCs are untouched.
	if !strings.HasPrefix(fullMethod, operatorMethodPrefix) {
		return ctx, nil
	}
	// Login is the only unauthenticated operator RPC.
	if fullMethod == loginMethod {
		return ctx, nil
	}
	principal, role, err := idp.Resolve(bearerToken(ctx))
	if err != nil {
		return ctx, err // already an Unauthenticated status
	}
	// Viewer is denied mutating commands; reads/feed are allowed for any role.
	if isOperatorOnly(fullMethod) && role != RoleOperator {
		return ctx, status.Errorf(codes.PermissionDenied, "role %q may not call %s", role, fullMethod)
	}
	return context.WithValue(ctx, principalCtxKey{}, principalInfo{principal: principal, role: role}), nil
}

// isOperatorOnly reports whether an OperatorConsole method is a mutating command
// (Operator-only). Reads (StreamEvents/Snapshot/Login and Query* reads) are open
// to any authenticated role. Command RPCs are added by later slices; this list
// is the single gate point. ADR-0047 D13.
func isOperatorOnly(fullMethod string) bool {
	name := strings.TrimPrefix(fullMethod, operatorMethodPrefix)
	switch name {
	case "Login", "StreamEvents", "Snapshot":
		return false
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

// UnaryAuthInterceptor gates unary OperatorConsole RPCs (ADR-0047 D13).
func UnaryAuthInterceptor(idp OperatorIdentity) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := authorize(ctx, info.FullMethod, idp)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor gates streaming OperatorConsole RPCs (e.g. StreamEvents).
func StreamAuthInterceptor(idp OperatorIdentity) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := authorize(ss.Context(), info.FullMethod, idp)
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
