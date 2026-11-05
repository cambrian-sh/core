package network

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// The progress-aware plan ceiling (owner decision 2026-11-05,
// T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
//
// It replaced a fixed wall-clock cap (plan_timeout_ms, 120 s), which failed
// healthy multi-step plans on slow or proxied models: the 2026-11-05 E2E (E15)
// spent ~96 s of real tool rounds in step 1, and step 2 died 200 ms after it was
// dispatched because the WHOLE plan had used its 120 s. A plan is now cut for one
// of two reasons only, each with its own cause:
//
//   - it went IDLE: nothing in it moved for execution.plan.plan_idle_timeout_ms —
//     no step started or finished, no model or tool call was in flight on one of
//     its step leases, no in-kernel thought/replan call ran, no progress event
//     reached its conversation;
//   - it ran past execution.plan.plan_max_duration_ms, the absolute safety cap.
//
// Per-step deadlines (the dispatcher's ADR-0001 D5 stepTimeout with its floor)
// still apply inside a step: they bound one hung call, which the idle watchdog
// deliberately treats as activity.
//
// Liveness is not a second mechanism: the gateway's per-lease in-flight counter
// (BudgetLeaseState.InFlightCalls, added for the chat stall watchdog) answers it,
// read through RunActivity; this file adds only what the gateway cannot see —
// step boundaries and in-kernel model calls that run on no step lease.

// planActivity is one plan's own liveness record: the last step boundary, and
// the in-kernel model calls (thought steps, replans) running for it right now.
type planActivity struct {
	last     atomic.Int64 // unix nanos
	inFlight atomic.Int32
}

func newPlanActivity() *planActivity {
	a := &planActivity{}
	a.touch()
	return a
}

func (a *planActivity) touch() { a.last.Store(time.Now().UnixNano()) }

// begin marks work as in flight until the returned func runs; both ends count as
// activity.
func (a *planActivity) begin() func() {
	a.inFlight.Add(1)
	a.touch()
	return func() {
		a.inFlight.Add(-1)
		a.touch()
	}
}

// at is now while work is in flight, else the last boundary.
func (a *planActivity) at() time.Time {
	if a.inFlight.Load() > 0 {
		return time.Now()
	}
	return time.Unix(0, a.last.Load())
}

// latestOf composes liveness probes: the most recent of their answers.
func latestOf(probes ...func() time.Time) func() time.Time {
	return func() time.Time {
		var last time.Time
		for _, p := range probes {
			if p == nil {
				continue
			}
			if t := p(); t.After(last) {
				last = t
			}
		}
		return last
	}
}

// planLimits resolves the plan's two limits from config. A hand-built config
// that leaves the cap at zero (tests, embedders) gets the default cap rather than
// a context that is already dead; an idle timeout of zero disables the watchdog,
// as documented on the key.
func planLimits(c config.PlanConfig) (maxDuration, idle time.Duration) {
	maxDuration = time.Duration(c.PlanMaxDurationMs) * time.Millisecond
	if maxDuration <= 0 {
		maxDuration = time.Duration(config.DefaultPlanMaxDurationMs) * time.Millisecond
	}
	if c.PlanIdleTimeoutMs > 0 {
		idle = time.Duration(c.PlanIdleTimeoutMs) * time.Millisecond
	}
	return maxDuration, idle
}

// errPlanMaxDuration and errPlanIdle are the two causes. Both wrap
// context.DeadlineExceeded, so every consumer that asks "was this a timeout?"
// (SelfHealer, Classify, Execute's response) answers yes, and both name their key
// so the operator knows which setting fired.
func errPlanMaxDuration(maxDuration time.Duration) error {
	return fmt.Errorf("plan exceeded max duration %s (execution.plan.plan_max_duration_ms): %w",
		maxDuration, context.DeadlineExceeded)
}

func errPlanIdle(silent, idle time.Duration) error {
	return fmt.Errorf("plan idle for %s, limit %s (execution.plan.plan_idle_timeout_ms): %w",
		silent.Round(time.Millisecond), idle, context.DeadlineExceeded)
}

// startPlanCeiling derives the plan context: the absolute cap as a deadline, and
// — when idle > 0 — one watchdog goroutine that cancels it once probe() is older
// than idle. stop releases both and waits for the watchdog to exit (one owner,
// drained on stop: AGENTS.md §3.3).
func startPlanCeiling(parent context.Context, maxDuration, idle time.Duration, probe func() time.Time) (context.Context, func()) {
	capped, cancelCap := context.WithTimeoutCause(parent, maxDuration, errPlanMaxDuration(maxDuration))
	if idle <= 0 || probe == nil {
		return capped, cancelCap
	}
	planCtx, cancelIdle := context.WithCancelCause(capped)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchPlanIdle(planCtx, cancelIdle, idle, probe)
	}()
	return planCtx, func() {
		cancelIdle(nil)
		cancelCap()
		<-done
	}
}

// watchPlanIdle polls probe a few times per idle window — a read of a few
// counters, not a per-event goroutine — and cancels with the idle cause when the
// plan has been silent for the whole window.
func watchPlanIdle(ctx context.Context, cancel context.CancelCauseFunc, idle time.Duration, probe func() time.Time) {
	interval := idle / 10
	if interval < 5*time.Millisecond {
		interval = 5 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if silent := time.Since(probe()); silent >= idle {
				cancel(errPlanIdle(silent, idle))
				return
			}
		}
	}
}

// runActivitySource is the gateway's plan-liveness read (SubstrateLLMGateway
// .RunActivity). Optional, like domain.LeaseResolver: a gateway without it (a test
// fake) contributes nothing and the plan is judged on its own record alone.
type runActivitySource interface {
	RunActivity(runID domain.RunID) time.Time
}

// leaseWorkTracker marks kernel-side work on a caller's lease as in flight
// (SubstrateLLMGateway.TrackLeaseWork).
type leaseWorkTracker interface {
	TrackLeaseWork(leaseID domain.LeaseID) func()
}

// conversationActivitySource is the progress holder's per-conversation liveness
// (app.progressHolder.LastActivity): an ADR-0098 progress event for the plan's
// conversation counts as plan activity.
type conversationActivitySource interface {
	LastActivity(conversationID string) time.Time
}

// trackLeaseWork marks a tool call or recall as in flight on the caller's lease
// for the plan idle watchdog and the chat stall watchdog. Always returns a
// callable func.
func (s *Server) trackLeaseWork(lease domain.LeaseID) func() {
	if s == nil || lease == "" || s.LLMGateway == nil {
		return func() {}
	}
	if t, ok := s.LLMGateway.(leaseWorkTracker); ok {
		return t.TrackLeaseWork(lease)
	}
	return func() {}
}

// planLivenessProbe composes everything that counts as progress for one plan:
// its own record (step boundaries, thought/replan calls), the gateway's view of
// its step leases, and progress events for the conversation that ordered it.
func (s *Server) planLivenessProbe(ctx context.Context, own *planActivity, runID domain.RunID, sessionID domain.SessionID) func() time.Time {
	probes := []func() time.Time{own.at}
	if src, ok := s.LLMGateway.(runActivitySource); ok {
		probes = append(probes, func() time.Time { return src.RunActivity(runID) })
	}
	if src, ok := s.Progress.(conversationActivitySource); ok && s.SessionMgr != nil && sessionID != "" {
		if ses, err := s.SessionMgr.GetSession(ctx, sessionID); err == nil && ses != nil && ses.ConversationID != "" {
			conv := ses.ConversationID
			probes = append(probes, func() time.Time { return src.LastActivity(conv) })
		}
	}
	return latestOf(probes...)
}

// activityStepFunc wraps a plan step so its start and finish count as activity.
// The step's run in between is NOT activity by itself (owner definition): a step
// is live while it makes calls, which the gateway sees.
func activityStepFunc[F ~func(context.Context, int, *domain.Handoff) (*domain.Handoff, error)](own *planActivity, fn F) F {
	return func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		own.touch()
		defer own.touch()
		return fn(ctx, i, h)
	}
}

// inFlightStepFunc wraps an in-kernel model step (a thought step, ADR-0139 D2):
// it runs on no step lease, so the plan marks it in flight itself.
func inFlightStepFunc[F ~func(context.Context, int, *domain.Handoff) (*domain.Handoff, error)](own *planActivity, fn F) F {
	return func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		defer own.begin()()
		return fn(ctx, i, h)
	}
}

// inFlightReplanner marks a replan's planner call (an in-kernel model call on no
// step lease) as plan activity while it runs.
type inFlightReplanner struct {
	inner executer.ReplanHandler
	own   *planActivity
}

func (r inFlightReplanner) Replan(ctx context.Context, failedStep int, err error,
	partialContext map[string]string, originalPlan *domain.ExecutionPlan) (*domain.ExecutionPlan, error) {
	defer r.own.begin()()
	return r.inner.Replan(ctx, failedStep, err, partialContext, originalPlan)
}

// Execute response metadata for a plan its ceiling cut (partial-plan responses).
const (
	// PlanTimedOutKey is "true" when the plan idle watchdog or the max-duration
	// cap ended the plan.
	PlanTimedOutKey = "_plan_timed_out"
	// PlanTimeoutCauseKey carries the cause, naming the limit and its config key.
	PlanTimeoutCauseKey = "_plan_timeout_cause"
)

// planCeilingCause is the plan ceiling's verdict once the plan has returned: the
// idle or max-duration cause when one of them ended planCtx, nil otherwise —
// including when the CALLER's ctx ended first, which is the caller giving up, not
// the plan timing out.
func planCeilingCause(parent, planCtx context.Context) error {
	if parent.Err() != nil || planCtx.Err() == nil {
		return nil
	}
	if cause := context.Cause(planCtx); errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return nil
}
