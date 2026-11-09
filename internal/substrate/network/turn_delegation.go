package network

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The lifetime of work a chat turn delegates (T-20261106-a-chat-turn-that-fails-
// deadline-exceeded-or, T-20261106-chat-stall-detection-cancels-a-turn-whose).
//
// A chat turn's session agent delegates to the planner by calling Execute with its
// turn lease. The plan therefore ran under the WORKER's Execute RPC context, and
// cancelling the turn (pool.Dispatch's ctx) never reached it: the 2026-11-06
// use-case runs (R-20261106T033413Z, R-20261106T040429Z) watched a plan keep
// dispatching steps and raising dangerous-tool approvals for minutes after the user
// had been told the request failed — one was approved under the NEXT job's policy.
// And because the chat stall watchdog could not see the plan's progress (its step
// leases are bound to the run, not the conversation), a turn whose plan was busy in
// a 90 s code-sandbox step read as silent and was cancelled.
//
// TurnDelegations is the kernel's one record of "which live work did which turn
// order". It answers both defects from the same facts:
//
//   - Liveness: ConversationActivity reports the latest progress of every live plan
//     a conversation's turn delegated, by reading each plan's EXISTING liveness probe
//     (plan_watchdog.go planLivenessProbe: step start/finish, thought and replan
//     calls, model/tool/recall work in flight on its step leases). It adds no second
//     liveness mechanism; it is a third consumer of the one the plan idle watchdog
//     and the chat stall watchdog already share.
//   - Lifetime: EndTurn, called by the chat lane on EVERY turn exit, cancels the
//     turn's live plans (context cancellation: the executor stops, each running
//     step's CallAgent context ends, and each step lease is Completed by its own
//     deferred Complete), revokes the turn's leases, and cancels every tool call in
//     flight on them — which is what withdraws a pending approval: the approval
//     controller's Request returns on ctx.Done (resolved as not approved). From then
//     on GuardToolCall refuses those leases and BeginRun refuses a late Execute
//     from that turn, so no approval for the turn's work can be raised after it ended.
//
// There is no "detached" or "background" delegation in the kernel (re-checked
// 2026-11-06): every delegated plan is the turn's, and ends with it.
type TurnDelegations struct {
	mu sync.Mutex
	// turns holds live work by the ordering turn's origin message id. An entry lives
	// while it has a run or a tool call in flight.
	turns map[string]*turnWork
	// runOrigin maps a live (or ended-turn) run to its ordering turn, so a step
	// lease — bound to the run, not the conversation — is attributed to the turn.
	runOrigin map[domain.RunID]string
	// ended and revoked are the tombstones that make the ordering guarantee hold
	// after the live records are gone. Bounded by age (tombstoneRetention).
	ended   map[string]time.Time
	revoked map[domain.LeaseID]time.Time
	now     func() time.Time
	// onTurnEnd runs once per EndTurn, after the turn's work is cancelled: the
	// run ledger's scratch cleanup (ADR-0145 D1, scratch lives for the turn). nil ⇒
	// nothing. Set once at the composition root (SetTurnEndHook), before serving.
	onTurnEnd func(conversationID, originMessageID string)
	// results holds the structured result (domain.ExecuteResult) of every run a
	// turn delegated directly, in completion order, until the turn ends: the chat
	// lane reads them kernel-side (TurnResults) for the reply check, the turn index
	// and ask-back (ADR-0145 D2/D5a/D8), instead of trusting the agent to relay them.
	results map[string][]domain.ExecuteResult
}

// TurnResults returns the structured results of the runs the turn delegated
// directly, in the order they finished (ADR-0145 D8). Nested plans a step agent
// delegated are not included. Read before EndTurn, which forgets them.
func (d *TurnDelegations) TurnResults(originMessageID string) []domain.ExecuteResult {
	if d == nil || originMessageID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]domain.ExecuteResult(nil), d.results[originMessageID]...)
}

// SetResult records the run's structured result, read from the Execute
// response metadata (domain.HandoffKeyExecuteResult). A nested run, a missing or
// undecodable result, and a run of an ended turn record nothing.
func (r *DelegatedRun) SetResult(meta map[string]string) {
	if r == nil || !r.direct || meta == nil || meta[domain.HandoffKeyExecuteResult] == "" {
		return
	}
	var res domain.ExecuteResult
	if err := json.Unmarshal([]byte(meta[domain.HandoffKeyExecuteResult]), &res); err != nil {
		slog.Warn("ADR-0145: delegated run result not decodable; the chat lane will not see it", "err", err)
		return
	}
	d := r.reg
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, gone := d.ended[r.origin]; gone {
		return
	}
	if d.results == nil {
		d.results = map[string][]domain.ExecuteResult{}
	}
	d.results[r.origin] = append(d.results[r.origin], res)
}

// SetTurnEndHook installs the turn-end hook (see onTurnEnd). Composition root only.
func (d *TurnDelegations) SetTurnEndHook(fn func(conversationID, originMessageID string)) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.onTurnEnd = fn
	d.mu.Unlock()
}

// tombstoneRetention bounds the ended-turn and revoked-lease tombstones. An hour
// is far beyond a turn timeout (execution.chat.chat_turn_timeout_seconds, default
// 240 s) and the plan safety cap's purpose here:
// an agent still acting on a dead turn's lease after an hour has nothing to raise
// approvals with anyway (the lease is long completed and its grants gone).
const tombstoneRetention = time.Hour

// ErrTurnEnded is the cause a delegated run is cancelled with, and the reason a
// late Execute or tool call from an ended turn is refused.
var ErrTurnEnded = errors.New("the chat turn that ordered this work has ended")

type turnWork struct {
	conversationID string
	runs           map[*DelegatedRun]struct{}
	leases         map[domain.LeaseID]struct{}
	guards         map[*toolGuard]struct{}
}

type toolGuard struct{ cancel context.CancelCauseFunc }

// DelegatedRun is one Execute registered under the turn that ordered it. All
// methods are nil-safe: Execute for work no chat turn ordered gets a nil run.
type DelegatedRun struct {
	reg    *TurnDelegations
	origin string
	cancel context.CancelCauseFunc
	// runID and probe are attached once the plan exists (Attach). Until then the run
	// is planning — an in-kernel planner call — and reads as active.
	runID domain.RunID
	probe func() time.Time
	// direct is set when the chat turn itself ordered the run (its lease names the
	// turn), not a step agent of one of the turn's plans.
	direct bool
}

// NewTurnDelegations constructs an empty registry.
func NewTurnDelegations() *TurnDelegations {
	return &TurnDelegations{
		turns:     map[string]*turnWork{},
		runOrigin: map[domain.RunID]string{},
		ended:     map[string]time.Time{},
		revoked:   map[domain.LeaseID]time.Time{},
		now:       time.Now,
	}
}

// turnLocked returns the live record for origin, creating it. Callers hold mu.
func (d *TurnDelegations) turnLocked(origin, conversationID string) *turnWork {
	tw := d.turns[origin]
	if tw == nil {
		tw = &turnWork{
			conversationID: conversationID,
			runs:           map[*DelegatedRun]struct{}{},
			leases:         map[domain.LeaseID]struct{}{},
			guards:         map[*toolGuard]struct{}{},
		}
		d.turns[origin] = tw
	}
	return tw
}

// dropIfIdleLocked forgets a live record with nothing running. Callers hold mu.
func (d *TurnDelegations) dropIfIdleLocked(origin string) {
	if tw := d.turns[origin]; tw != nil && len(tw.runs) == 0 && len(tw.guards) == 0 {
		delete(d.turns, origin)
	}
}

// BeginRun registers an Execute ordered by the chat turn named in binding (the
// caller's turn lease: ConversationID + OriginMessageID, stamped kernel-side by
// the chat lane). It returns the context the run must execute under — cancelled
// by EndTurn — and the run handle to Attach and End. Work no chat turn ordered
// (no origin) is returned unchanged with a nil run. A turn that has already
// ended is refused with ErrTurnEnded: its user has their answer, and the work it
// would start could only raise approvals nobody is waiting for.
func (d *TurnDelegations) BeginRun(ctx context.Context, binding domain.LeaseBinding) (context.Context, *DelegatedRun, error) {
	if d == nil {
		return ctx, nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	origin := binding.OriginMessageID
	if origin == "" && binding.RunID != "" {
		// A step agent of a turn's plan delegating again: the nested plan is the
		// same turn's work.
		origin = d.runOrigin[binding.RunID]
	}
	if origin == "" {
		return ctx, nil, nil
	}
	if _, gone := d.ended[origin]; gone {
		return ctx, nil, ErrTurnEnded
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	r := &DelegatedRun{reg: d, origin: origin, cancel: cancel, direct: binding.OriginMessageID != ""}
	tw := d.turnLocked(origin, binding.ConversationID)
	if tw.conversationID == "" {
		tw.conversationID = binding.ConversationID
	}
	tw.runs[r] = struct{}{}
	return runCtx, r, nil
}

// Attach names the run and its plan liveness probe once the plan exists.
func (r *DelegatedRun) Attach(runID domain.RunID, probe func() time.Time) {
	if r == nil {
		return
	}
	r.reg.mu.Lock()
	defer r.reg.mu.Unlock()
	r.runID = runID
	r.probe = probe
	if runID != "" {
		r.reg.runOrigin[runID] = r.origin
	}
}

// AddLease records a step lease minted for the run, so EndTurn revokes it even
// after the step has Completed it (an agent process can outlive its step's RPC).
// A lease minted after the turn ended is revoked at once.
func (r *DelegatedRun) AddLease(lease domain.LeaseID) {
	if r == nil || lease == "" {
		return
	}
	d := r.reg
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, gone := d.ended[r.origin]; gone {
		d.revoked[lease] = d.now()
		return
	}
	if tw := d.turns[r.origin]; tw != nil {
		tw.leases[lease] = struct{}{}
	}
}

// End deregisters the run (Execute returned) and releases its context.
func (r *DelegatedRun) End() {
	if r == nil {
		return
	}
	d := r.reg
	d.mu.Lock()
	if tw := d.turns[r.origin]; tw != nil {
		delete(tw.runs, r)
	}
	// An ended turn keeps its run → origin mapping (pruned with the tombstones) so a
	// step lease of the run is still recognised as the dead turn's.
	if _, gone := d.ended[r.origin]; !gone && r.runID != "" {
		delete(d.runOrigin, r.runID)
	}
	d.dropIfIdleLocked(r.origin)
	d.mu.Unlock()
	r.cancel(nil)
}

// ConversationActivity is the delegated-work half of the chat liveness probe: the
// latest progress of any live plan a turn of conversationID delegated, "now" for a
// run still planning, zero when there is none. Composed with the progress holder
// and the gateway's ConversationActivity by chat.LatestActivity (app.go).
func (d *TurnDelegations) ConversationActivity(conversationID string) time.Time {
	if d == nil || conversationID == "" {
		return time.Time{}
	}
	d.mu.Lock()
	var probes []func() time.Time
	planning := false
	for _, tw := range d.turns {
		if tw.conversationID != conversationID {
			continue
		}
		for r := range tw.runs {
			if r.probe == nil {
				planning = true
				continue
			}
			probes = append(probes, r.probe)
		}
	}
	d.mu.Unlock()
	if planning {
		return d.now()
	}
	// The probes run outside the lock: they read the gateway and the progress holder.
	return latestOf(probes...)()
}

// EndTurn ends everything the turn ordered: its live runs are cancelled with
// ErrTurnEnded, its leases (the turn lease and every step lease its runs minted)
// are revoked, and every tool call in flight on them is cancelled, withdrawing any
// approval it is waiting on. Idempotent. Called by the chat lane on every exit of
// a turn — success included: by then a synchronous delegation has returned, and
// anything still running has nobody to deliver to.
func (d *TurnDelegations) EndTurn(conversationID, originMessageID string, turnLease domain.LeaseID, cause error) {
	if d == nil || originMessageID == "" {
		return
	}
	if cause == nil {
		cause = ErrTurnEnded
	} else {
		cause = errors.Join(ErrTurnEnded, cause)
	}
	d.mu.Lock()
	now := d.now()
	d.pruneLocked(now)
	d.ended[originMessageID] = now
	if turnLease != "" {
		d.revoked[turnLease] = now
	}
	tw := d.turns[originMessageID]
	delete(d.turns, originMessageID)
	delete(d.results, originMessageID)
	hook := d.onTurnEnd
	var cancels []context.CancelCauseFunc
	runs := 0
	if tw != nil {
		for r := range tw.runs {
			cancels = append(cancels, r.cancel)
			runs++
		}
		for g := range tw.guards {
			cancels = append(cancels, g.cancel)
		}
		for l := range tw.leases {
			d.revoked[l] = now
		}
	}
	d.mu.Unlock()
	for _, c := range cancels {
		c(cause)
	}
	if hook != nil {
		hook(conversationID, originMessageID)
	}
	if runs > 0 || len(cancels) > 0 {
		slog.Warn("chat turn ended; cancelling the work it delegated",
			"conversation", conversationID, "origin_message", originMessageID,
			"runs", runs, "cancelled", len(cancels), "cause", cause)
	}
}

// pruneLocked drops tombstones older than tombstoneRetention. Callers hold mu.
func (d *TurnDelegations) pruneLocked(now time.Time) {
	for o, at := range d.ended {
		if now.Sub(at) > tombstoneRetention {
			delete(d.ended, o)
			for rid, ro := range d.runOrigin {
				if ro == o {
					delete(d.runOrigin, rid)
				}
			}
		}
	}
	for l, at := range d.revoked {
		if now.Sub(at) > tombstoneRetention {
			delete(d.revoked, l)
		}
	}
}

// GuardToolCall is ExecuteTool's admission for turn-ordered work. lease is the
// presented lease; binding/known its resolution for this caller. It refuses
// (ok=false) a lease of a turn that has ended — before any authorisation or
// approval runs — and otherwise returns a context that EndTurn cancels, plus the
// func to release it when the call returns. Work no chat turn ordered passes
// through untouched.
func (d *TurnDelegations) GuardToolCall(ctx context.Context, lease domain.LeaseID, binding domain.LeaseBinding, known bool) (context.Context, func(), bool) {
	noop := func() {}
	if d == nil || lease == "" {
		return ctx, noop, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dead := d.revoked[lease]; dead {
		return ctx, noop, false
	}
	if !known {
		return ctx, noop, true
	}
	origin := binding.OriginMessageID
	if origin == "" && binding.RunID != "" {
		origin = d.runOrigin[binding.RunID]
	}
	if origin == "" {
		return ctx, noop, true
	}
	if _, gone := d.ended[origin]; gone {
		return ctx, noop, false
	}
	gctx, cancel := context.WithCancelCause(ctx)
	g := &toolGuard{cancel: cancel}
	tw := d.turnLocked(origin, binding.ConversationID)
	tw.guards[g] = struct{}{}
	tw.leases[lease] = struct{}{}
	return gctx, func() {
		d.mu.Lock()
		if tw := d.turns[origin]; tw != nil {
			delete(tw.guards, g)
		}
		d.dropIfIdleLocked(origin)
		d.mu.Unlock()
		cancel(nil)
	}, true
}
