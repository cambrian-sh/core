package operator

import (
	"fmt"
	"strings"
)

// MethodRole is how a premium operator-plane method is classified for the role
// gate, DECLARED by the plugin that serves it (app.Registry.DeclareOperatorMethodRoles,
// owner decision 2026-11-14 on T-20261113-core-operator-authz-hardcodes-premium-plane).
//
// Core keeps the enforcement point (authorize, ADR-0085); the plugin supplies the
// classification. A premium method nobody classified falls under the fail-closed
// naming rule in isOperatorOnly, exactly like an OperatorConsole method: a
// Query*/List*/Get*/Describe* name is a read open to Viewers, anything else is an
// Operator command, and Members reach nothing.
//
// Until 2026-11-14 core named the accounts, org-directory and substrate premium
// services and their exceptional methods itself, so every new premium
// self-service method needed a core change and a core release (ADR-0082 Gap 2).
type MethodRole string

const (
	// MethodSelfService: open to every authenticated role, Member included, even
	// when the name reads like a command — the handler acts only on the caller's
	// own data and no request field names another principal (connect my account,
	// read my reporting chain).
	MethodSelfService MethodRole = "self_service"
	// MethodMemberRead: a read a Member may make (their own accounts list). For
	// Operators and Viewers it is an ordinary read under the naming rule.
	MethodMemberRead MethodRole = "member_read"
	// MethodOperatorOnly: a read that is other people's business (who connected
	// which account, the SCIM connections, a full ontology draft). Viewers and
	// Members are refused even though the name reads like a read.
	MethodOperatorOnly MethodRole = "operator_only"
)

// Valid reports whether r is one of the declared classes.
func (r MethodRole) Valid() bool {
	switch r {
	case MethodSelfService, MethodMemberRead, MethodOperatorOnly:
		return true
	}
	return false
}

// MethodRoles maps a FULL premium method name ("/cambrian.premium.pkg.Service/Method")
// to its declared class. Matching on the full name means an OperatorConsole RPC of
// the same short name can never inherit a premium plane's exception.
type MethodRoles map[string]MethodRole

// FullMethodName builds the gRPC full method name of service/method and checks
// that it may carry a declared role: the service must be a premium plane
// (proto package cambrian.premium.*) — OperatorConsole's own roles are core's
// decision and cannot be reclassified by a plugin — and the method a bare name.
func FullMethodName(service, method string) (string, error) {
	service = strings.TrimSpace(service)
	if !strings.HasPrefix("/"+service+"/", premiumMethodPrefix) || strings.ContainsAny(service, "/ ") {
		return "", fmt.Errorf("operator method roles: service %q is not a premium operator-plane service (package cambrian.premium.*)", service)
	}
	if method == "" || strings.ContainsAny(method, "/ ") {
		return "", fmt.Errorf("operator method roles: invalid method name %q on %s", method, service)
	}
	return "/" + service + "/" + method, nil
}

// declared returns the class a plugin declared for fullMethod. Only premium
// methods are consulted, so a mis-built map can never touch an OperatorConsole RPC.
func (m MethodRoles) declared(fullMethod string) (MethodRole, bool) {
	if len(m) == 0 || !strings.HasPrefix(fullMethod, premiumMethodPrefix) {
		return "", false
	}
	r, ok := m[fullMethod]
	return r, ok
}
