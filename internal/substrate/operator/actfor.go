package operator

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

// Act-for conversations (T-20261106-conversations-cannot-be-opened-as-a-given).
//
// The operator plane authenticates operators, and before this every conversation
// was owned by whichever operator opened it — so a deployment with one bootstrap
// operator had exactly one person's memory notes, recall and credentials, and a
// client could not hold a conversation AS a given employee. An operator may now
// act FOR a principal: the conversation's owner is that principal, and because
// everything downstream keys on the owner (ADR-0139 D14 run principal: notes,
// skill grants, per-user credentials; the chat lane's read scope), none of it
// resolves to the operator.
//
// The checks, in order, all fail closed:
//  1. only a role CanActFor allows may name another principal;
//  2. a kernel with no PrincipalDirectory refuses act-for (it cannot tell a
//     person from a typo);
//  3. the principal must be KNOWN to that directory — an operator cannot mint
//     an identity by typing one.
//
// Every act-for call is audited with the operator as actor and the principal
// named in the entry (open/send/close).

// CanActFor reports whether an operator-plane role may act on behalf of another
// principal. Only the Operator role: a Viewer may not even send its own turns,
// and acting for someone is strictly more power than acting as oneself.
func CanActFor(r Role) bool { return r == RoleOperator }

// actingPrincipal resolves the principal a conversation call acts as: the caller
// itself, or — with on_behalf_of — the named principal after the checks above.
// The returned ctx carries that principal as the DOMAIN principal and the run
// principal, so anything downstream that authorizes or attributes sees the person
// the conversation is for, never the operator. The operator-plane private
// principal (PrincipalFromContext) is left as the operator: the audit actor stays
// the human who acted.
func (s *Service) actingPrincipal(ctx context.Context, onBehalfOf string) (context.Context, string, error) {
	actor, role, _ := PrincipalFromContext(ctx)
	p := strings.TrimSpace(onBehalfOf)
	if p == "" || p == actor {
		return ctx, actor, nil
	}
	if !CanActFor(role) {
		return ctx, "", status.Errorf(codes.PermissionDenied, "role %q may not act on behalf of another principal", role)
	}
	if s.principals == nil {
		return ctx, "", status.Error(codes.FailedPrecondition,
			"acting on behalf of a principal needs a principal directory (premium authz); none is installed")
	}
	known, err := s.principals.KnownPrincipal(ctx, p)
	if err != nil {
		slog.WarnContext(ctx, "act-for: principal directory unavailable; refusing", "principal", p, "err", err)
		return ctx, "", status.Error(codes.Unavailable, "the principal directory could not be consulted")
	}
	if !known {
		return ctx, "", status.Errorf(codes.NotFound, "unknown principal %q", p)
	}
	ctx = domain.WithPrincipal(ctx, domain.UserPrincipal(p))
	ctx = domain.WithRunPrincipal(ctx, p)
	// The acting operator rides along for AUDIT only (the chat turn's lease
	// carries it to the agent plane, where a memory-note write names both).
	ctx = domain.WithActForActor(ctx, actor)
	return ctx, p, nil
}

// actForNote is the audit annotation for an act-for call ("" when acting as self).
func actForNote(actor, principal string) string {
	if principal == "" || principal == actor {
		return ""
	}
	return "on_behalf_of=" + principal
}
