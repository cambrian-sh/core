package domain

import "context"

// planStepKey is the context key under which the plan executor hands the step it is
// dispatching to the per-step callback.
type planStepKey struct{}

// WithPlanStep returns ctx carrying the step the executor is about to dispatch, as it
// stands in the executor's CURRENT plan.
//
// Why a context value: the per-step callback (the server's stepFn) is built once per
// run around the plan as it was before the run, but the executor grows and rewrites
// that plan while it runs — fan-out appends one concrete child per item (ADR-0078
// R2) and a replan swaps the steps (ADR-0005). Indexing the captured plan then either
// misses (a fan-out child's index is past its end) or finds a DIFFERENT step (a
// replanned plan of the same length), and the step's routing contract —
// RequiredCapabilities, PreferredAgent/AgentPin, MaxEnergy, CheckpointAfter — is lost
// or wrong. Measured: R-20261106T033413Z, where every fan-out child of a "rename and
// move each receipt" step routed with no capability requirement and landed on an
// agent with no move tool (T-20261106-planner-routes-a-file-move-step-to-an-agent).
// The executor is the only party that knows the current step, so it says which one.
func WithPlanStep(ctx context.Context, s Step) context.Context {
	return context.WithValue(ctx, planStepKey{}, s)
}

// PlanStepFrom returns the step WithPlanStep put on ctx, and whether there was one.
func PlanStepFrom(ctx context.Context) (Step, bool) {
	s, ok := ctx.Value(planStepKey{}).(Step)
	return s, ok
}
