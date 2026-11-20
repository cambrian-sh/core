package domain

import "context"

// KernelTool is a tool the kernel serves itself, in process, through the same
// ExecuteTool call every agent already makes (ADR-0145: the ledger tools, D1; the
// calculation tools, D4). It is the kernel's own read view over material the
// caller may already read, so it is not a registry tool: it needs no grant, it is
// always offered (ListTools appends it to every menu), and it runs with the
// caller's resolved read predicate on ctx (domain.WithScope) and the caller's
// lease-resolved provenance (LedgerProvenanceFromContext). A kernel tool's result
// is never itself recorded to the ledger (it returns entries already there).
type KernelTool interface {
	// Spec is the tool's menu entry: name, description and argument schema.
	Spec() SystemTool
	// Call runs the tool. An error is a value the caller's model sees and can
	// correct (bad arguments, an unknown scope), never a crash.
	Call(ctx context.Context, call KernelToolCall) ([]byte, error)
}

// KernelToolOffer is implemented by a kernel tool that is not on every menu: it
// is listed only for the leases OfferedTo admits (ToolExecutor.KernelToolSpecsFor).
// The tool still refuses a call from any other lease itself. ctx is the menu
// request's (an offer may read the caller's registry entry, AGENTS.md §2.7).
type KernelToolOffer interface {
	OfferedTo(ctx context.Context, lease LeaseID) bool
}

// KernelToolCall is one invocation of a kernel tool. The principal comes from the
// authenticated caller, never from the arguments.
type KernelToolCall struct {
	AgentID  string
	ArgsJSON []byte
	// SessionTokenID is the caller's lease (kernel-resolved, as on every tool
	// call). run_subtasks (ADR-0150) needs it to find the parent's run and depth.
	SessionTokenID LeaseID
}
