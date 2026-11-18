// Package dispatch implements capability-typed dispatch (ADR-0100): agent
// selection as a PURE FUNCTION over data the kernel already owns — zero RPCs,
// zero LLM calls, zero speculative process boots.
//
// It replaces the auction, which solicited a bid from every candidate (booting
// each one to do it) and received a constant from a hand-written keyword table.
// The three decisions the auction conflated are separated here:
//
//	Eligibility  — Gatekeeper L1: required_capabilities ⊆ manifest.Capabilities
//	Ranking      — Gatekeeper L3 merit (per-capability, ADR-0069/0076)
//	Recovery     — cascade on verifier failure (ADR-0100 P5; not in P0)
//
// Only the WINNER is booted. FindCandidates already returns a merit-ranked
// slate, so dispatch is largely the auction with the bid round removed.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// AgentCaller invokes one agent and returns its response.
//
// It is an interface rather than an owned implementation because the dial /
// boot / connection-pool machinery lives on `internal/agentplane.Transport`,
// which the privileged system organs also call directly. Sharing it avoids a
// second gRPC connection pool.
//
// The original note here said this machinery would move into this package once
// the auction was deleted (P3). P3 has happened and it deliberately did not
// move: the transport is now the agent plane's own component and its other
// callers are organs that must NOT depend on a selection mechanism. Keeping the
// port is what lets invocation and selection stay separable.
type AgentCaller interface {
	CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error)
}

// ManifestReader reads a registered agent's manifest.
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// BootCounter reports the process-lifetime agent spawn tally. A delta across a
// selection measures what that decision cost in cold starts (ADR-0100 P2).
// Satisfied by *agentmgr.AgentManager; optional and nil-safe.
type BootCounter interface{ BootCount() uint64 }

// ProfileReader reads an agent's performance profile. Optional: a nil reader
// disables the cheapest-competent branch and every step takes merit-argmax.
type ProfileReader interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// Selection reasons, recorded on the emitted event so a routing decision can be
// explained after the fact without re-deriving it.
const (
	ReasonArgmaxMerit       = "argmax-merit"
	ReasonCheapestCompetent = "cheapest-competent"
	ReasonExploration       = "exploration"
	ReasonSoleCandidate     = "sole-candidate"
	// ReasonComposed: the step runs on a worker the kernel composed for it
	// (ADR-0145 D12); the selected profession is recorded in the composition.
	ReasonComposed = "composed"
)

// WorkerComposer composes the worker a step runs on (ADR-0145 D12,
// internal/metabolism/compose). ok=false leaves the step to the selected winner.
//
// A non-nil error refuses the dispatch: a yielded sub-goal whose capability hint
// the parent step's composition does not cover (domain.CapabilityMissingError) is
// not handed to a worker with the same tools.
type WorkerComposer interface {
	Compose(ctx context.Context, task *domain.DispatchTask, winner domain.ScoredCandidate,
		scored []domain.ScoredCandidate, in *domain.Handoff) (domain.ScoredCandidate, domain.WorkerComposition, bool, error)
}

// Dispatcher selects and invokes the executor for one step. It satisfies
// domain.StepDispatcher so it is a drop-in at the DAG call site.
type Dispatcher struct {
	Gatekeeper domain.Gatekeeper
	Caller     AgentCaller
	Manifests  ManifestReader

	// Profiles supplies latency/cost for the cheapest-competent branch. nil ⇒
	// always argmax (cold-start safe).
	Profiles ProfileReader

	// Boots measures agent cold starts attributable to a selection (ADR-0100 P2).
	// nil ⇒ the metric is reported as 0.
	Boots BootCounter

	ExecCfg  config.ExecutionConfig
	EventBus domain.EventBus // may be nil

	// ExplorationRate is the probability of picking a random eligible candidate
	// instead of the policy winner. Carried over from the auction (ADR-0100 D9:
	// exploration attaches to a ranking as well as it did to a market).
	ExplorationRate float64

	// Rand is injectable so tests are deterministic. nil ⇒ package rand.
	Rand *rand.Rand

	// Leases binds the step lease on the handoff to the agent about to receive it
	// (ADR-0139 D8). The lease is minted before selection, so this is the first point
	// the executing agent is known. nil ⇒ the lease stays agent-unbound (tests), and an
	// agent-unbound lease is refused to every agent (Server.resolveLeaseFor).
	Leases domain.LeaseAgentBinder

	// Composer, when set (execution.workers.composed), runs a plan step on a worker
	// composed for it instead of on the selected profile agent (ADR-0145 D12). A
	// step whose winner is a code or service agent is dispatched to it unchanged.
	// nil ⇒ the legacy agent-per-step path.
	Composer WorkerComposer

	// Delegations reports the step leases whose worker is waiting on its own
	// delegation to the planner (ADR-0145 D14). Such a step is never re-dispatched,
	// and its per-step deadline does not fire while the delegation runs (it restarts
	// when the delegation returns). nil ⇒ plain per-step deadlines
	// (T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent).
	Delegations *domain.ActiveDelegations

	// StepFailures is the tool executor's per-step record of identical failed
	// calls (execution.tools.repeated_failure_stop). A step it stopped is not
	// dispatched again on its lease, and a worker returning from a stopped step
	// ends the step with the domain.StepStuckError, whatever it answered. nil ⇒ off.
	StepFailures *domain.StepFailures
}

// outcomeError turns what a worker returned into the step's error when the step
// did not end in an answer: the step was stopped for a repeated identical tool
// failure (domain.StepStuckError), or the worker reported that it lacks a
// capability the step needs (domain.CapabilityMissingError). Both are class
// needs_replan: the re-planner re-plans the step, and the kernel composes the new
// step's worker from the principal's grants
// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under, owner 2026-11-12).
func (d *Dispatcher) outcomeError(taskID string, in, resp *domain.Handoff) error {
	if stuck := d.StepFailures.Stuck(leaseOf(in)); stuck != nil {
		return stuck
	}
	if cm := domain.CapabilityMissingFromHandoff(taskID, resp); cm != nil {
		if in != nil && in.Context != nil {
			cm.Composition = in.Context[handoffCompositionIDKey]
		}
		slog.Info("dispatch: worker reported a missing capability; the step goes to the re-planner",
			"task", taskID, "needed", cm.Needed, "outside_step", cm.OutsideStep)
		return cm
	}
	return nil
}

// handoffCompositionIDKey is compose.HandoffKeyCompositionID (the composition id
// the composer stamps on the step's handoff).
const handoffCompositionIDKey = "_composition_id"

// ErrStepDelegating refuses to dispatch a step whose worker is still waiting on
// its own delegation: the step is in progress, and handing its lease to another
// agent would cut the waiting worker off from its own step.
var ErrStepDelegating = errors.New("dispatch: the step's worker is waiting on its own delegation to the planner; the step is not re-dispatched")

// handoffLeaseKey is the handoff context key that carries the step's BudgetLease to the
// agent (kernel-internal; the wire name predates the lease rename, see domain/llm_gateway.go).
const handoffLeaseKey = "_session_token_id"

// bindLease hands the handoff's lease to agentID immediately before it is called. Every
// CallAgent this dispatcher makes goes through here, so the winner, a runner-up fallback
// and a yield delegate or resume each find the lease bound to themselves — and any other
// agent presenting it is refused by Server.resolveLeaseFor.
func (d *Dispatcher) bindLease(agentID string, h *domain.Handoff) {
	if d.Leases == nil || h == nil || h.Context == nil {
		return
	}
	if lease := h.Context[handoffLeaseKey]; lease != "" {
		d.Leases.BindLeaseAgent(domain.LeaseID(lease), agentID)
	}
}

func (d *Dispatcher) randFloat() float64 {
	if d.Rand != nil {
		return d.Rand.Float64()
	}
	return rand.Float64()
}

func (d *Dispatcher) randIntn(n int) int {
	if d.Rand != nil {
		return d.Rand.Intn(n)
	}
	return rand.Intn(n)
}

func (d *Dispatcher) emit(ctx context.Context, ev domain.SelectionEventPayload) {
	if d.EventBus == nil {
		return
	}
	_ = d.EventBus.Publish(ctx, ev)
}

// Execute runs the full dispatch pipeline: Gatekeeper → select → CallAgent.
func (d *Dispatcher) Execute(ctx context.Context, task *domain.DispatchTask, in *domain.Handoff) (*domain.DispatchResult, error) {
	// ADR-0100 P2 instrumentation: the selection decision only — the winner's
	// execution is deliberately outside the window.
	selStart := time.Now()
	bootsBefore := uint64(0)
	if d.Boots != nil {
		bootsBefore = d.Boots.BootCount()
	}
	selectionCost := func() (int32, int32) {
		ms := int32(time.Since(selStart).Milliseconds())
		boots := int32(0)
		if d.Boots != nil {
			boots = int32(d.Boots.BootCount() - bootsBefore)
		}
		return ms, boots
	}

	// A step stopped for a repeated identical tool failure runs nothing more on its
	// lease: not a heal retry, not a yielded sub-goal.
	if stuck := d.StepFailures.Stuck(leaseOf(in)); stuck != nil {
		return nil, stuck
	}

	scored, err := d.Gatekeeper.FindCandidates(ctx, task)
	if err != nil {
		// D5 rung 4: the requirements named something the fleet cannot satisfy and
		// no generalist exists. Surface it on the feed as well as to the caller —
		// this is the failure mode that used to die as a generic "no candidates".
		var noMatch *domain.NoCapabilityMatchError
		var noEffect *domain.NoEffectMatchError
		if errors.As(err, &noMatch) || errors.As(err, &noEffect) {
			// The effect-check refusal is surfaced the same way: the feed's ErrorMsg
			// carries the required effects, each agent's derived effects and the
			// (empty) eligible set, so the console can show why the step was refused.
			d.emit(ctx, domain.SelectionEventPayload{
				TaskID:   task.ID,
				TaskDesc: task.Description,
				Status:   "failed",
				ErrorMsg: err.Error(),
				Funnel:   task.Funnel,
			})
			return nil, err
		}
		return nil, fmt.Errorf("gatekeeper failed: %w", err)
	}

	d.emit(ctx, domain.SelectionEventPayload{
		TaskID:   task.ID,
		TaskDesc: task.Description,
		Status:   "started",
	})

	if len(scored) == 0 {
		// No eligible executor. Under the auction this surfaced as "no valid
		// proposals"; here it is named for what it is. The D5 resolution ladder
		// (alias map → generalist tier) lands in P1 — until then this is a hard,
		// loud failure rather than a silent misroute.
		d.emit(ctx, domain.SelectionEventPayload{
			TaskID:   task.ID,
			TaskDesc: task.Description,
			Status:   "failed",
			ErrorMsg: "no eligible candidates: no registered agent declares the step's required capabilities",
			Funnel:   task.Funnel,
		})
		return nil, fmt.Errorf("dispatch: no eligible candidates for task %s (required capabilities: %v)",
			task.ID, task.RequiredCapabilities)
	}

	// ADR-0145 D14: a step whose worker is waiting on its own delegation is in
	// progress; a second dispatch (a heal retry, a runner-up) would re-bind its
	// lease and cut the waiting worker off.
	if lease := leaseOf(in); lease != "" && d.Delegations.Active(lease) {
		slog.Warn("dispatch: step not re-dispatched; its worker is waiting on its own delegation",
			"task", task.ID, "lease", lease)
		return nil, ErrStepDelegating
	}

	winner, reason := d.selectWinner(ctx, task, scored)
	// ADR-0145 D12: the eligible professions become one composed worker. Only a
	// plan step (the kernel stamps _step_index) is composed; the selected agent is
	// kept in `selected` for the event's rationale.
	composed := false
	compositionID := ""
	if d.Composer != nil && in != nil && in.Context != nil && in.Context["_step_index"] != "" {
		rt, comp, ok, cerr := d.Composer.Compose(ctx, task, winner, scored, in)
		if cerr != nil {
			d.emit(ctx, domain.SelectionEventPayload{
				TaskID:   task.ID,
				TaskDesc: task.Description,
				Status:   "failed",
				ErrorMsg: cerr.Error(),
				Funnel:   task.Funnel,
			})
			return nil, cerr
		}
		if ok {
			slog.Debug("dispatch: step composed", "task", task.ID, "selected", winner.Agent.ID, "runtime", rt.Agent.ID)
			winner, reason, composed, compositionID = rt, ReasonComposed, true, comp.ID()
		}
	}

	// The candidate slate is reported on the same event the auction used, so the
	// orchestration suite scores routing accuracy identically across both arms.
	// Confidence carries the MERIT score, not a bid — labelled in Rationale so
	// the two arms are never confused when the numbers are compared.
	bids := make([]domain.CandidateEntry, 0, len(scored))
	for _, sc := range scored {
		// IsTool is deliberately left false: it recorded the static-bidder path
		// (the "tool" trait is deleted, ENG-1351), and dispatch solicits no bids at all.
		rationale := "merit-rank (dispatch: no bid solicited)"
		if ec := task.EffectCheck; ec != nil && ec.Outcome == domain.EffectCheckEligible {
			// Why this agent was eligible at all: its tools cover the step's effects.
			rationale += "; effects [" + strings.Join(ec.Required, ",") + "] ⊆ [" +
				strings.Join(ec.AgentEffects[sc.Agent.ID], ",") + "]"
		}
		if composed {
			rationale += "; profession of composition " + compositionID
		}
		bids = append(bids, domain.CandidateEntry{
			AgentID:    sc.Agent.ID,
			Confidence: float32(sc.Score),
			Rationale:  rationale,
		})
	}

	selMs, selBoots := selectionCost()
	d.emit(ctx, domain.SelectionEventPayload{
		TaskID:             task.ID,
		TaskDesc:           task.Description,
		Status:             "completed",
		WinnerID:           winner.Agent.ID,
		Bids:               bids,
		WinnerMargin:       meritMargin(scored, winner.Agent.ID),
		Funnel:             task.Funnel,
		SelectionLatencyMs: selMs,
		SelectionBoots:     selBoots,
	})

	slog.Debug("dispatch: winner selected",
		"task", task.ID, "winner", winner.Agent.ID,
		"reason", reason, "candidates", len(scored), "merit", winner.Score)

	runnerUps := make([]domain.ScoredCandidate, 0, len(scored))
	for _, sc := range scored {
		// A composed step has no runner-ups: falling back to a pre-built agent
		// would run the step outside its composition (ADR-0145 D12).
		if composed {
			break
		}
		if sc.Agent.ID != winner.Agent.ID {
			runnerUps = append(runnerUps, sc)
		}
	}

	// ADR-0023 Fix 2: inject the winning capability so the SDK's _dispatch_execute
	// routes to the right handler without text-matching fallback.
	if in.Context == nil {
		in.Context = make(map[string]string)
	}
	if d.Manifests != nil {
		if m, mErr := d.Manifests.GetManifest(ctx, winner.Agent.ID); mErr == nil && m != nil && len(m.Tools) > 0 {
			in.Context["_capability"] = m.Tools[0]
		}
	}
	in.Context["_selection_reason"] = reason

	// NOTE (ADR-0100 D9): the auction's requirement sub-negotiation fired on
	// bestProposal.Requirements. With no bids there are no requirements, so that
	// recursive path is inert here by construction. Re-homing it as a step-level
	// declaration is tracked in the ADR; it must not be dropped by omission.

	// ADR-0001 D5: per-step timeout = (latency * step_timeout_multiplier) +
	// step_timeout_base_buffer_ms, bounded above by the plan ceiling the caller's
	// ctx already carries. See stepTimeout for where the latency term comes from
	// and why a winner without one gets no per-step deadline.
	callCtx := ctx
	var stepDeadline time.Time
	if timeout := d.stepTimeout(ctx, winner); timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel, stepDeadline = d.withStepDeadline(ctx, timeout, leaseOf(in))
		defer cancel()
	}
	// The task's Deadline is the one the step actually runs under — the per-step
	// deadline when there is one, else the plan ceiling the caller's ctx carries,
	// else none — so whatever reads it (yield's sub-tasks inherit it) is told the
	// truth. It used to be a hardcoded `now + 20 s` set by the server that nothing
	// enforced (T-20261105-slow-model-reliability-hardcoded-20-s).
	if dl, ok := callCtx.Deadline(); ok {
		task.Deadline = dl
	} else {
		task.Deadline = stepDeadline
	}

	d.bindLease(winner.Agent.ID, in)
	callStart := time.Now()
	resp, err := d.Caller.CallAgent(callCtx, winner.Agent.ID, in, "")
	callMs := time.Since(callStart).Milliseconds()
	if err != nil {
		in.Context["_winning_agent_id"] = winner.Agent.ID
		// Name the step deadline when it is what fired, so a timed-out step reads
		// as "timed out" in the log rather than as the agent's generic failure.
		// The parent still being alive is what tells the two apart.
		if errors.Is(context.Cause(callCtx), context.DeadlineExceeded) && ctx.Err() == nil {
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("dispatch: step timed out (agent %s): %w", winner.Agent.ID, err)
			} else {
				// A delegation-aware deadline cancels with a cause; the call itself
				// reports context.Canceled.
				err = fmt.Errorf("dispatch: step timed out (agent %s): %w (%w)", winner.Agent.ID, err, context.DeadlineExceeded)
			}
		}
		return &domain.DispatchResult{Confidence: winner.Score, RunnerUps: runnerUps}, err
	}

	// ADR-0139 D6: stamp the dispatched definition's identity on the response so the
	// step TaskEvent lands on the profile routing reads (agent.ID, agent.SourceHash).
	// Overwrites any agent-echoed value — the kernel, not the agent, knows which
	// version it dispatched. See domain.HandoffKeyWinnerSourceHash.
	if resp != nil {
		if resp.Context == nil {
			resp.Context = make(map[string]string)
		}
		resp.Context[domain.HandoffKeyWinnerAgentID] = winner.Agent.ID
		resp.Context[domain.HandoffKeyWinnerSourceHash] = winner.Agent.SourceHash
		// The wall time of the winner's call, the step TaskEvent's latency
		// (T-20261104-step-taskevents-carry-no-latency-so-the-merit). A sub-ms call
		// rounds up to 1: 0 means "unknown" to the aggregator.
		if callMs < 1 {
			callMs = 1
		}
		resp.Context[domain.HandoffKeyWinnerLatencyMs] = strconv.FormatInt(callMs, 10)
	}

	if oerr := d.outcomeError(task.ID, in, resp); oerr != nil {
		in.Context["_winning_agent_id"] = winner.Agent.ID
		return &domain.DispatchResult{Confidence: winner.Score, RunnerUps: runnerUps}, oerr
	}

	return &domain.DispatchResult{
		Handoff:        resp,
		Confidence:     winner.Score,
		RunnerUps:      runnerUps,
		StepAllocation: d.selectModelCandidates(ctx, winner.Agent.ID),
	}, nil
}

// leaseOf is the step lease a handoff carries ("" when none).
func leaseOf(in *domain.Handoff) domain.LeaseID {
	if in == nil || in.Context == nil {
		return ""
	}
	return domain.LeaseID(in.Context[handoffLeaseKey])
}

// delegationRecheck bounds how often an extended step deadline looks again at a
// delegation that is still running.
const delegationRecheck = 30 * time.Second

// withStepDeadline is the per-step deadline (ADR-0001 D5) that does not fire
// while the step's worker waits on its own delegation to the planner (ADR-0145
// D14): when the timer fires during a delegation it is re-armed, and once the
// delegation has returned the worker gets a full step timeout from that moment to
// finish. Without a tracker or a lease it is a plain context.WithTimeout. The
// cause of an expiry is context.DeadlineExceeded either way; the returned time is
// the deadline as first armed (for DispatchTask.Deadline).
func (d *Dispatcher) withStepDeadline(ctx context.Context, timeout time.Duration, lease domain.LeaseID) (context.Context, context.CancelFunc, time.Time) {
	first := time.Now().Add(timeout)
	if d.Delegations == nil || lease == "" {
		c, cancel := context.WithTimeout(ctx, timeout)
		return c, cancel, first
	}
	c, cancel := context.WithCancelCause(ctx)
	var mu sync.Mutex
	var timer *time.Timer
	var fire func()
	fire = func() {
		mu.Lock()
		defer mu.Unlock()
		if c.Err() != nil {
			return
		}
		if d.Delegations.Active(lease) {
			timer = time.AfterFunc(minDuration(timeout, delegationRecheck), fire)
			return
		}
		if ended := d.Delegations.LastEnded(lease); !ended.IsZero() {
			if left := timeout - time.Since(ended); left > 0 {
				timer = time.AfterFunc(left, fire)
				return
			}
		}
		cancel(context.DeadlineExceeded)
	}
	mu.Lock()
	timer = time.AfterFunc(timeout, fire)
	mu.Unlock()
	return c, func() {
		mu.Lock()
		timer.Stop()
		mu.Unlock()
		cancel(context.Canceled)
	}, first
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// stepTimeout is the ADR-0001 D5 per-step deadline for the winner:
//
//	timeout = (latency * execution.plan.step_timeout_multiplier) + step_timeout_base_buffer_ms
//
// ADR-0001 took `latency` from the winning bid (AgentProposal.Latency). Dispatch
// solicits no bids (ADR-0100), so the term is the winner's OBSERVED median latency
// from its profile (network + computation), the same figure cheapestCompetent
// already ranks on — observed, never self-reported.
//
// The result is never shorter than execution.plan.step_timeout_floor_ms (default
// 3 min): a short observed median must not cut off the first long generation the
// agent is asked for on a slow local or proxied model. The floor raises the
// deadline; it never creates one where the formula yields none.
//
// A winner with no profile, or a profile with no latency yet, gets NO per-step
// deadline (0) and runs under the plan ceiling alone. The formula has nothing to
// scale for such an agent, and collapsing it to the bare buffer (15 s at the
// defaults) would kill every first-run step on a slow local model — the exact
// failure the owner's 2026-08-11 defaults note (config.go, PlanConfig) warns
// about. Both settings <= 0 also disable it, so an operator can switch the step
// deadline off without touching the profile.
func (d *Dispatcher) stepTimeout(ctx context.Context, winner domain.ScoredCandidate) time.Duration {
	mult := d.ExecCfg.Plan.StepTimeoutMultiplier
	buffer := time.Duration(d.ExecCfg.Plan.StepTimeoutBaseBufferMs) * time.Millisecond
	if mult <= 0 && buffer <= 0 {
		return 0
	}
	if d.Profiles == nil {
		return 0
	}
	p, err := d.Profiles.GetProfile(ctx, winner.Agent.ID, winner.Agent.SourceHash)
	if err != nil || p == nil {
		return 0
	}
	latency := time.Duration(p.NetworkLatencyMedianMs+p.ComputationLatencyMedianMs) * time.Millisecond
	if latency <= 0 {
		return 0
	}
	timeout := time.Duration(float64(latency)*mult) + buffer
	if floor := time.Duration(d.ExecCfg.Plan.StepTimeoutFloorMs) * time.Millisecond; timeout < floor {
		timeout = floor
	}
	return timeout
}

// selectWinner implements the ADR-0100 D4 per-step dispatch policy.
//
// Pure argmax-on-merit has a rich-get-richer failure: the top-ranked agent
// accumulates all the evidence, every other agent stays cold, and the ranking
// freezes on early noise. Pure cheapest-first pays a re-execution whenever it
// guesses wrong. The step itself already says which regime it is in — a cheap
// step whose result will be verified can afford to try the cheap agent, because
// verification catches a bad answer and escalation is affordable.
//
// CAVEAT (ADR-0139 D10): "verified" was the mid-plan checkpoint gate, which has
// been deleted, so nothing verifies a checkpoint_after step mid-plan any more.
// The branch is OFF by default (dispatch_cheap_energy_max = 0) and must stay off
// until it is re-grounded on the async VerificationWorker —
// T-20261104-cheapest-competent-dispatch-branch-is-keyed-on.
func (d *Dispatcher) selectWinner(ctx context.Context, task *domain.DispatchTask, scored []domain.ScoredCandidate) (domain.ScoredCandidate, string) {
	if len(scored) == 1 {
		return scored[0], ReasonSoleCandidate
	}

	// Epsilon exploration (ADR-0100 D9) — except under the ADR-0139 D6 thompson
	// merit mode, where the Gatekeeper already ranked the slate by one posterior
	// sample per candidate: that IS the exploration policy (an uncertain agent's
	// draw is wide, so it wins in proportion to its chance of being best), and an
	// epsilon branch on top would add uniform-random picks the posterior did not
	// ask for. posterior_lcb keeps the epsilon branch: a lower bound is pessimistic
	// about uncertainty, so without it an agent with no evidence would rarely be
	// tried and never acquire any.
	explore := d.ExplorationRate > 0 && d.ExecCfg.Gatekeeper.ActiveMeritMode() != config.MeritModeThompson
	if explore && d.randFloat() < d.ExplorationRate {
		pick := d.randIntn(len(scored))
		return scored[pick], ReasonExploration
	}

	if d.cheapAndVerified(task) {
		if w, ok := d.cheapestCompetent(ctx, scored); ok {
			return w, ReasonCheapestCompetent
		}
	}

	// FindCandidates returns merit-ranked, highest first.
	return scored[0], ReasonArgmaxMerit
}

// cheapAndVerified reports whether the step is in the regime where trying the
// cheapest competent agent is the right bet: its output will be checked, and its
// energy budget marks it as cheap. A step with no declared budget (MaxEnergy 0)
// is NOT treated as cheap — absence of a budget is not a claim of cheapness.
func (d *Dispatcher) cheapAndVerified(task *domain.DispatchTask) bool {
	if !task.CheckpointAfter {
		return false
	}
	maxE := d.ExecCfg.Routing.DispatchCheapEnergyMax
	if maxE <= 0 {
		return false
	}
	return task.MaxEnergy > 0 && task.MaxEnergy <= maxE
}

// cheapestCompetent picks the lowest-cost candidate whose merit clears the floor.
// Returns ok=false when no profile data is available or nobody clears the floor,
// so the caller falls back to argmax.
func (d *Dispatcher) cheapestCompetent(ctx context.Context, scored []domain.ScoredCandidate) (domain.ScoredCandidate, bool) {
	if d.Profiles == nil {
		return domain.ScoredCandidate{}, false
	}
	floor := d.ExecCfg.Routing.DispatchMeritFloor

	var best domain.ScoredCandidate
	bestCost := 0.0
	found := false

	for _, sc := range scored {
		if sc.Score < floor {
			continue
		}
		p, err := d.Profiles.GetProfile(ctx, sc.Agent.ID, sc.Agent.SourceHash)
		if err != nil || p == nil {
			continue // no evidence of cost ⇒ not a basis for preferring it
		}
		cost := agentCost(p)
		if !found || cost < bestCost {
			best, bestCost, found = sc, cost, true
		}
	}
	return best, found
}

// agentCost is the cheapness ordering: money first when the profile records it,
// latency otherwise. Both are observed, never self-reported.
func agentCost(p *domain.AgentProfile) float64 {
	if p.ModelMetrics != nil && p.ModelMetrics.AvgCostPerTask > 0 {
		return p.ModelMetrics.AvgCostPerTask
	}
	return float64(p.NetworkLatencyMedianMs + p.ComputationLatencyMedianMs)
}

// meritMargin is the winner's merit minus the best non-winner's — the dispatch
// analogue of the auction's WinnerMargin. A near-zero margin flags a coin-flip.
func meritMargin(scored []domain.ScoredCandidate, winnerID string) float32 {
	var winner float64
	runnerUp := 0.0
	haveWinner, haveRunnerUp := false, false
	for _, sc := range scored {
		if sc.Agent.ID == winnerID && !haveWinner {
			winner, haveWinner = sc.Score, true
			continue
		}
		if !haveRunnerUp || sc.Score > runnerUp {
			runnerUp, haveRunnerUp = sc.Score, true
		}
	}
	if !haveWinner || !haveRunnerUp {
		return 0
	}
	return float32(winner - runnerUp)
}

// selectModelCandidates runs ADR-0018 TraitModel sub-selection for the winning
// agent. Behaviour-identical to the retired Auctioneer's, so model allocation is
// unchanged across the two arms. Returns nil when no TraitModel agents exist.
func (d *Dispatcher) selectModelCandidates(ctx context.Context, winnerAgentID string) *domain.StepAllocation {
	if d.Manifests == nil {
		return nil
	}
	manifest, err := d.Manifests.GetManifest(ctx, winnerAgentID)
	if err != nil || manifest == nil {
		return nil
	}
	models, err := d.Gatekeeper.FindModelCandidates(ctx, manifest.RequiredModelCapabilities)
	if err != nil || len(models) == 0 {
		return nil
	}
	sa := &domain.StepAllocation{Winner: models[0].Agent}
	if len(models) >= 2 {
		sa.Fallbacks[0] = models[1].Agent
	}
	if len(models) >= 3 {
		sa.Fallbacks[1] = models[2].Agent
	}
	return sa
}

// CallAgent satisfies domain.StepDispatcher by delegating to the shared caller.
// The handoff's lease is bound to agentID first, exactly as Execute does for its winner.
func (d *Dispatcher) CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
	d.bindLease(agentID, handoff)
	resp, err := d.Caller.CallAgent(ctx, agentID, handoff, excludeInstanceID)
	if err != nil {
		return resp, err
	}
	// The same outcome check as Execute: a resumed worker (yield) or a runner-up
	// may end the step stopped or with a missing capability.
	taskID := ""
	if handoff != nil && handoff.Context != nil {
		taskID = handoff.Context["task_id"]
	}
	if oerr := d.outcomeError(taskID, handoff, resp); oerr != nil {
		return nil, oerr
	}
	return resp, nil
}
