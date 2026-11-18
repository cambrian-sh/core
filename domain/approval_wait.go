package domain

import "context"

// Waiting on a person (T-20261118-a-chat-turn-waiting-on-a-human-approval-is).
//
// A chat turn whose tool call blocks on a human approval is not stuck: it is
// waiting for a person, for as long as the approval's own window allows. The
// chat lane's stall watchdog and turn deadline must be able to tell that apart
// from silence, and only the approval hub knows exactly when a requester starts
// and stops blocking on a person. So the hub reports that span to whoever put a
// hook on the requester's context — in production the turn admission of the
// agent plane (network.TurnDelegations.GuardToolCall), which attributes it to
// the turn that ordered the call. Context-carried rather than a hub dependency
// because the hub serves every requester (plans, schedules, skill access) and
// must not learn what a chat turn is.

type approvalWaitKey struct{}

// WithApprovalWaitHook returns ctx carrying hook: an approval requester that
// blocks on a person calls hook when it starts waiting and the returned func
// when it stops. A hook already on ctx keeps being called (hooks chain), so two
// layers that each care about the wait both see it.
func WithApprovalWaitHook(ctx context.Context, hook func() (done func())) context.Context {
	if hook == nil {
		return ctx
	}
	if parent, ok := ctx.Value(approvalWaitKey{}).(func() func()); ok {
		inner := hook
		hook = func() func() {
			pd, id := parent(), inner()
			return func() { id(); pd() }
		}
	}
	return context.WithValue(ctx, approvalWaitKey{}, hook)
}

// BeginApprovalWait reports that the requester on ctx starts blocking on a
// person; the returned func reports the end of the wait. Always safe to call:
// without a hook both are no-ops.
func BeginApprovalWait(ctx context.Context) (done func()) {
	if hook, ok := ctx.Value(approvalWaitKey{}).(func() func()); ok {
		if d := hook(); d != nil {
			return d
		}
	}
	return func() {}
}
