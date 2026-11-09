package domain

import "context"

// Two seams first built for ADR-0139 D12 subagents (spawn_subagent was removed by
// ADR-0145 D14) and still used: the per-lease tool ceiling (ADR-0145 D12 composed
// leases) and the dedicated agent instance (parallel fan-out children).

// LeaseToolCeiling bounds the tools the holder of ONE lease may use, on top of
// every per-agent ceiling. The ADR-0145 D12 composed runtime is ONE agent for
// every composed step, so a per-agent ceiling cannot express "this step may use
// only these tools": the bound has to be keyed by the step's lease
// (domain.ComposedLeases). ToolExecutor consults it wherever it has the
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
// Why: an agent process is single-threaded (the SDK's runtime serves one Execute
// at a time, ADR-0036 D2). Parallel fan-out children of one agent would queue on
// its one process, and a plan a WORKER delegated (ADR-0145 D14: a worker hands
// extra work to the planner through Execute while its own step is still running)
// would queue its steps behind that same worker and deadlock until the timeout.
// It is a ctx marker, set kernel-side only (it never crosses
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

type delegationDepthKey struct{}

// WithDelegationDepth carries how deep a plan run is in a chain of worker
// delegations (ADR-0145 D14): 0 for work a person, a chat turn or a pipeline
// ordered; n+1 for a plan a worker of a depth-n run delegated. The kernel binds
// it to every step lease of the run (LeaseBinding.DelegationDepth).
func WithDelegationDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, delegationDepthKey{}, depth)
}

// DelegationDepthFrom returns the run's delegation depth (0 when unset).
func DelegationDepthFrom(ctx context.Context) int {
	v, _ := ctx.Value(delegationDepthKey{}).(int)
	return v
}
