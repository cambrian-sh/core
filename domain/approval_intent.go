package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Approvals that outlive the kernel run that raised them
// (T-20261118-a-sync-approval-orphaned-by-a-kernel-restart; owner decision
// 2026-11-18, option A: an approval approved after a kernel restart carries out
// its action).
//
// A blocking (sync) approval used to keep everything needed to act in its
// requester's goroutine: the tool call, its arguments, the person it was for.
// The item itself was durable (ADR-0142 D11), so after a restart it was still
// listed as pending — and an approve recorded "approved" while nothing could run
// it any more. Now the requester stores its INTENT on the item: what to run and
// for whom, as kernel-held facts. When a pending sync item is answered and no
// requester of THIS process waits on it, the hub hands the approved intent to
// the recoverer registered for it, exactly once, under the original person's
// principal and the access rules in force now. An intent that can no longer be
// carried out safely is refused with the reason — never "approved" with nothing
// run. An item without an intent, or with no recoverer, is refused the same way.
//
// A withdrawn item (its chat turn ended in the same kernel run, ENG-2119) is
// terminal: Answer refuses it before any of this runs.

// Intent executors: which recoverer carries out an item's intent.
const (
	// ApprovalIntentTool is a dangerous (or hook-narrowed) tool call raised at
	// the ToolExecutor's approval gate.
	ApprovalIntentTool = "tool"
	// ApprovalIntentSchedule is an assistant's recurring-task proposal
	// (internal/scheduledtask).
	ApprovalIntentSchedule = "schedule"
)

// ApprovalIntent is what an approved sync request does, stored on its item so a
// later kernel run can carry it out. Every field is kernel-held (the lease
// binding, the conversation record, the validated call) — never a payload value
// (INV-5). It is persisted with the item while the item is pending and cleared
// once the item is resolved and carried out (or refused).
type ApprovalIntent struct {
	// Executor names the recoverer (ApprovalIntentTool, ApprovalIntentSchedule).
	Executor string `json:"executor"`
	// AgentID is the agent that made the request.
	AgentID string `json:"agent_id,omitempty"`
	// Tool is the tool (or RPC) the request runs.
	Tool string `json:"tool,omitempty"`
	// Args is the call's full, validated argument JSON — what ran the checks at
	// raise time and what runs on recovery. For a schedule, the proposal. Bytes
	// (base64 in the stored JSON), not json.RawMessage: marshalling compacts a
	// RawMessage, and the call carried out must be byte for byte the call the
	// person approved.
	Args []byte `json:"args,omitempty"`
	// OnBehalfOf is the run principal (ADR-0140 D4): the person a per_user
	// integration acts as.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// ConversationOwner is the owner of the ordering conversation (ADR-0139 D14).
	ConversationOwner string `json:"conversation_owner,omitempty"`
	// Beneficiary is the task beneficiary, when the call ran for a task.
	BeneficiaryID   string `json:"beneficiary_id,omitempty"`
	BeneficiaryKind string `json:"beneficiary_kind,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	// Surface is the ADR-0085 D7 surface clamp the call ran under.
	SurfaceID   string `json:"surface_id,omitempty"`
	SurfaceKind string `json:"surface_kind,omitempty"`
	// RunID / StepIndex name the plan step that made the call (HasStep), so a
	// recovered call keeps its ADR-0145 D9 idempotency key and a resumed plan's
	// retry is answered from the stored result instead of running again.
	HasStep   bool   `json:"has_step,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	StepIndex int    `json:"step_index,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	// Lease is the budget lease the call ran under — for the audit trail only.
	// A lease does not survive a restart, so nothing it conferred (a skill's
	// run-scoped grant, its spend account) is presented again: a recovered call
	// is re-authorized from the agent's durable grants, fail-closed.
	Lease string `json:"lease,omitempty"`
}

// Person is the person the intent acts for: the run principal, else the task
// beneficiary, else the conversation owner ("" for a call made for nobody).
func (in ApprovalIntent) Person() string {
	switch {
	case in.OnBehalfOf != "":
		return in.OnBehalfOf
	case in.BeneficiaryID != "" && personish(PrincipalKind(in.BeneficiaryKind)):
		return in.BeneficiaryID
	}
	return in.ConversationOwner
}

// Context rebuilds the request facts on ctx: the agent principal, the person the
// call is for, the ordering conversation and session, the surface clamp and the
// plan step. What those facts ALLOW is not carried — it is resolved again, now,
// by whatever reads them (the authorizer, the grant store, the predicate).
func (in ApprovalIntent) Context(ctx context.Context) context.Context {
	if in.AgentID != "" {
		ctx = WithPrincipal(ctx, AgentPrincipal(in.AgentID))
	}
	if in.OnBehalfOf != "" {
		ctx = WithRunPrincipal(ctx, in.OnBehalfOf)
	}
	if in.ConversationOwner != "" {
		ctx = WithConversationOwner(ctx, in.ConversationOwner)
	}
	if in.BeneficiaryID != "" {
		ctx = WithTaskBeneficiary(ctx, PrincipalRef{ID: in.BeneficiaryID, Kind: PrincipalKind(in.BeneficiaryKind)})
	}
	if in.ConversationID != "" {
		ctx = WithConversationID(ctx, in.ConversationID)
	}
	if in.SessionID != "" {
		ctx = WithSessionID(ctx, SessionID(in.SessionID))
	}
	if in.SurfaceKind != "" || in.SurfaceID != "" {
		ctx = WithSurface(ctx, SurfaceRef{ID: in.SurfaceID, Kind: in.SurfaceKind})
	}
	if in.HasStep {
		ctx = WithStepCall(ctx, StepCall{RunID: RunID(in.RunID), StepIndex: in.StepIndex})
	}
	return ctx
}

func cloneIntent(in *ApprovalIntent) *ApprovalIntent {
	if in == nil {
		return nil
	}
	c := *in
	c.Args = append([]byte(nil), in.Args...)
	return &c
}

// ApprovalRecoverer carries out an approved intent whose requester is gone
// (raised by an earlier kernel run). Registered per executor at boot
// (AddressedHub.RecoverWith).
type ApprovalRecoverer interface {
	// CheckRecovery reports why the intent can no longer be carried out safely
	// (the person is no longer known or active, the tool is gone, the access
	// rules in force now refuse it), or nil. Runs BEFORE the approval is
	// recorded: a refused intent is withdrawn, not approved.
	CheckRecovery(ctx context.Context, it ApprovalItem) error
	// Recover carries the approved intent out once and returns what happened,
	// for the person. An error wrapping ErrRecoveryRefused means nothing ran;
	// any other error means the outcome is unknown or the action failed.
	Recover(ctx context.Context, it ApprovalItem) (string, error)
}

var (
	// ErrApprovalNotCarriedOut: an approve of an item whose requester is gone
	// was refused because its action could not be carried out; nothing was done.
	ErrApprovalNotCarriedOut = errors.New("approval not carried out")
	// ErrRecoveryRefused: a recoverer refused the intent before anything ran.
	ErrRecoveryRefused = errors.New("refused before anything ran")
)

// RecoveryActionKey is the ADR-0154 durable action record key of an approval's
// recovered execution: deterministic from the approval id (AGENTS.md §2.6), so a
// second approve, a second kernel or a recovery re-run after a crash finds the
// same record instead of running the action again.
func RecoveryActionKey(approvalID string) string { return "approval-recovery:" + approvalID }

// RecoverWith registers the recoverer of an intent executor. Later
// registrations replace; nil removes. Registered at boot, before the transport
// opens; an item answered while none is registered for its executor is refused,
// never approved with nothing run.
func (h *AddressedHub) RecoverWith(executor string, r ApprovalRecoverer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r == nil {
		delete(h.recoverers, executor)
		return
	}
	h.recoverers[executor] = r
}

func (h *AddressedHub) recovererFor(it ApprovalItem) ApprovalRecoverer {
	if it.Intent == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recoverers[it.Intent.Executor]
}

// orphaned reports whether a pending item has no requester in this process: a
// sync item nobody here raised, joined or waits on — its requester died with an
// earlier kernel run. An async item never has a requester (its kind handler
// applies it), so it is never an orphan.
func (h *AddressedHub) orphaned(it ApprovalItem) bool {
	if it.Async {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiters[it.ID]) == 0 && !h.local[it.ID]
}

// orphanRefusal checks an orphaned item before an approve is recorded. It
// returns the reason the approve must be refused (the item is then withdrawn),
// or "".
func (h *AddressedHub) orphanRefusal(ctx context.Context, it ApprovalItem) string {
	rec := h.recovererFor(it)
	if rec == nil {
		return "the request that raised it ended with an earlier kernel run and this kind of request cannot be carried out after a restart; nothing was done"
	}
	if err := rec.CheckRecovery(ctx, it); err != nil {
		return "the request that raised it ended with an earlier kernel run, and it can no longer be carried out safely: " + err.Error() + "; nothing was done"
	}
	return ""
}

// recover carries out an approved orphan's intent exactly once and records the
// outcome on the item. It returns the item as recorded and the error the
// answerer is given (nil when the action was carried out).
func (h *AddressedHub) recover(ctx context.Context, it ApprovalItem) (ApprovalItem, error) {
	ctx = context.WithoutCancel(ctx) // the answer's RPC ending must not cut the action in half
	rec := h.recovererFor(it)
	detail, err := h.runRecovery(ctx, rec, it)
	it.Detail = detail
	it.Intent = nil // carried out or refused: the intent has done its job
	it.Consumed = true
	if perr := h.store.PutApproval(ctx, it); perr != nil {
		// The Detail is the person's record of what became of the approval
		// (AGENTS.md §3.2): a lost write is logged with the item id.
		h.logger.Warn("approvals: recovered item's outcome not recorded", "event", "approval_detail_lost",
			"approval", it.ID, "err", perr)
	}
	h.publishResolved(ctx, it)
	return it, err
}

func (h *AddressedHub) runRecovery(ctx context.Context, rec ApprovalRecoverer, it ApprovalItem) (string, error) {
	if rec == nil {
		// The recoverer was removed between the check and now (never in a booted
		// kernel). Nothing ran.
		d := "approved, but it was not carried out: nothing can carry out this request after a restart"
		return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
	}
	key := RecoveryActionKey(it.ID)
	stored, created, err := h.actions.Record(ctx, DurableAction{
		Key: key, Tool: it.Intent.Tool, Principal: it.Intent.Person(),
		SessionID: SessionID(it.Intent.SessionID), ArgsHash: hashBytes(it.Intent.Args),
		State: ActionStateApproved,
	})
	if err != nil {
		// Without the record exactly-once cannot be promised: run nothing.
		d := "approved, but it was not carried out: its action record could not be written (" + err.Error() + ")"
		return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
	}
	if !created {
		switch {
		case stored.State.Settled() && stored.Result != nil:
			return "approved and carried out: " + string(stored.Result.ResultJSON), nil
		case stored.State == ActionStateNotConfirmed:
			d := "approved, but it was not carried out: " + stored.Error
			return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
		case stored.State == ActionStateDispatched:
			// An earlier run sent it and stopped before recording the outcome:
			// running it again could do it twice.
			d := "approved; carrying it out was interrupted and its outcome is unknown — check before asking again"
			if stored.Error != "" {
				d += " (" + stored.Error + ")"
			}
			return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
		}
		// proposed / approved: recorded but never dispatched — safe to run.
	}
	if _, err := h.actions.Advance(ctx, key, ActionStateDispatched, ActionUpdate{}); err != nil {
		d := "approved, but it was not carried out: its action record could not be advanced (" + err.Error() + ")"
		return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
	}
	h.logger.Info("approvals: carrying out an approval raised by an earlier kernel run", "event", "approval_recovering",
		"approval", it.ID, "executor", it.Intent.Executor, "tool", it.Intent.Tool, "person", it.Intent.Person())
	summary, rerr := rec.Recover(ctx, it)
	if rerr != nil {
		to := ActionStateDispatched // outcome unknown: stays dispatched with the error (ADR-0154 D3)
		d := "approved, but carrying it out failed: " + rerr.Error()
		if errors.Is(rerr, ErrRecoveryRefused) {
			to = ActionStateNotConfirmed
			d = "approved, but it was not carried out: " + rerr.Error()
		}
		if _, aerr := h.actions.Advance(ctx, key, to, ActionUpdate{Error: rerr.Error()}); aerr != nil {
			h.logger.Warn("approvals: recovery outcome not recorded on its action record", "approval", it.ID, "err", aerr)
		}
		h.logger.Warn("approvals: approval raised by an earlier kernel run was not carried out", "event", "approval_recovery_failed",
			"approval", it.ID, "executor", it.Intent.Executor, "err", rerr)
		return d, fmt.Errorf("%w: %s", ErrApprovalNotCarriedOut, d)
	}
	if _, aerr := h.actions.Advance(ctx, key, ActionStateAccepted, ActionUpdate{
		Result: &ToolCallResponse{ResultJSON: []byte(summary)},
	}); aerr != nil {
		h.logger.Warn("approvals: recovery outcome not recorded on its action record", "approval", it.ID, "err", aerr)
	}
	h.logger.Info("approvals: approval raised by an earlier kernel run carried out", "event", "approval_recovered",
		"approval", it.ID, "executor", it.Intent.Executor, "outcome", summary)
	return "approved and carried out: " + summary, nil
}

// resumeRecoveries carries out the approved intents a crash interrupted between
// recording the approve and finishing the action (state approved, intent still
// stored). The action record decides whether running is safe. Boot only: in a
// running kernel such an item is being carried out by its answerer.
func (h *AddressedHub) resumeRecoveries(ctx context.Context) {
	items, err := h.store.ListApprovals(ctx, ApprovalQuery{States: []ApprovalState{ApprovalApproved}, WithIntent: true})
	if err != nil {
		h.logger.Warn("approvals: could not list interrupted recoveries", "err", err)
		return
	}
	for _, it := range items {
		if it.Intent == nil {
			continue
		}
		_, _ = h.recover(ctx, it) // the outcome is on the item and the feed; there is no answerer to tell
	}
}

// RecoverySummary bounds a recovered action's outcome text for the person.
func RecoverySummary(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}
