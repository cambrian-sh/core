package domain

import "context"

// ADR-0139 D12: in-agent subagents. The kernel side lives in
// internal/substrate/network/subagent.go (Server.SpawnSubagent); these are the
// two seams other layers need without importing it.

// LeaseToolCeiling bounds the tools the holder of ONE lease may use, on top of
// every per-agent ceiling. A D12 child is often the same agent as its parent
// (an empty spawn target means "the parent's own profile"), so a per-agent
// ceiling cannot express "this child may use only web_fetch": the bound has to
// be keyed by the child's lease. ToolExecutor consults it wherever it has the
// session token (grantFor, ConferSkillGrants, AvailableToolsNamedFor) and the
// ListTools handler filters the menu with it.
//
// AllowsLease reports bound=false for a lease it knows nothing about (no limit),
// and for a bound lease whether tool is inside its ceiling.
type LeaseToolCeiling interface {
	AllowsLease(lease, tool string) (inside, bound bool)
}

type dedicatedInstanceKey struct{}

// WithDedicatedInstance marks ctx so the agent transport serves the next
// CallAgent on a FRESH instance of the agent, booted for that one call and
// evicted after it, instead of the agent's shared cached instance.
//
// Why a D12 child needs it: an agent process is single-threaded (the SDK's
// runtime serves one Execute at a time, ADR-0036 D2), and the parent is still
// inside its own Execute while it waits on SpawnSubagent. A same-profile child
// sent to the shared instance would queue behind its own parent and deadlock
// until the timeout. It is a ctx marker, set kernel-side only (it never crosses
// the wire), so the selection path (Dispatcher.Execute / CallAgent) is the one
// every other dispatch uses — only the instance the transport picks differs.
func WithDedicatedInstance(ctx context.Context) context.Context {
	return context.WithValue(ctx, dedicatedInstanceKey{}, true)
}

// DedicatedInstanceRequested reports whether ctx carries WithDedicatedInstance.
func DedicatedInstanceRequested(ctx context.Context) bool {
	v, _ := ctx.Value(dedicatedInstanceKey{}).(bool)
	return v
}
