package domain

import "context"

// PrincipalDirectory answers "is this a principal this deployment knows?"
// (T-20261106-conversations-cannot-be-opened-as-a-given).
//
// There is no principal table in Cambrian: principals are REFERENCED — by an
// identity binding (a person on an entry point), by a scope row the authorizer
// consults, by group membership, by a policy link. The premium policy plane is
// the only component that sees all of those, so it implements this port and the
// kernel asks it before acting FOR someone (an operator opening a conversation
// on a principal's behalf).
//
// Fail closed by construction: a kernel with no directory installed refuses
// act-for outright rather than accepting any string as a person. "Known" is not
// "authorized to read anything" — the policy plane still decides what a known
// principal may see; this only stops an operator from minting an identity by
// typing it.
type PrincipalDirectory interface {
	// KnownPrincipal reports whether id names a principal the deployment has
	// referenced (bound, scoped, grouped or policy-linked). An error means the
	// directory could not be consulted, which callers treat as unknown.
	KnownPrincipal(ctx context.Context, id string) (bool, error)
}
