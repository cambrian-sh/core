package domain

// The person a delegated plan step acts for, on the step's handoff
// (T-20261107-delegated-steps-name-the-conversation-owner-in).
//
// A chat front desk asked "summarise my unread email" by its owner delegated
// "access <owner's full name>'s email"; the worker, whose mail tools reach only
// "the currently authenticated user's own mailbox", read that as another person's
// mailbox and refused. Nothing in the step told it that the person named IS the
// principal its tools authenticate as.
//
// These keys carry that fact. They are a PROJECTION of the run principal — the
// same lease → principal resolution the kernel uses to pick a per_user
// integration's credential at ExecuteTool (ADR-0140 D4; conversation owner, then
// task beneficiary, then the configured default) — never a second identity
// source, and never read back by the kernel for any decision. Kernel-authored:
// the step dispatcher always overwrites or deletes them, so an Execute caller's
// metadata cannot assert whom a step acts for. The display name comes from the
// ADR-0142 principal registry and is for the model to recognise a name in the
// task text; it decides nothing.
const (
	// HandoffKeyActingFor is the run principal's id.
	HandoffKeyActingFor = "_acting_for"
	// HandoffKeyActingForName is that principal's registry display name, "" when
	// the registry does not know one.
	HandoffKeyActingForName = "_acting_for_name"
)

// StampActingFor writes the acting-for keys onto a step's handoff context, or
// removes them when principalID is empty, so a stale or caller-supplied value
// never survives. ctx must be non-nil.
func StampActingFor(ctx map[string]string, principalID, displayName string) {
	if principalID == "" {
		delete(ctx, HandoffKeyActingFor)
		delete(ctx, HandoffKeyActingForName)
		return
	}
	ctx[HandoffKeyActingFor] = principalID
	if displayName != "" && displayName != principalID {
		ctx[HandoffKeyActingForName] = displayName
	} else {
		delete(ctx, HandoffKeyActingForName)
	}
}
