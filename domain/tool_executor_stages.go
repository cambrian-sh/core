package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Execute's stages (ENG-2251). Execute (tool_executor.go) used to be one
// ~640-line function; it is now the sequence of named stages below, with the same order and the same
// outcomes (pinned by tool_executor_gates_test.go before the split). Two rules
// keep the split behaviour-preserving:
//
//   - every per-call value lives on toolCall, so a stage sees exactly what the
//     original code saw at that point (the request as rewritten so far, the
//     argument hash, the grant, the verdict);
//   - every deferred hook is registered by Execute itself, in the original
//     order, from the finish functions a stage returns — so each one still reads
//     Execute's FINAL response through the named result.

// toolCall is one Execute call's state as it moves through the stages.
type toolCall struct {
	req     ToolCallRequest
	argHash string

	tool    SystemTool
	contrib *contributedResolution
	// intentArgs are the arguments an approval's intent stores (tool_recovery.go):
	// validated, before CID hydration, after a pre_tool rewrite.
	intentArgs   []byte
	subtaskLease bool
	actionKey    string

	grant       ToolGrant
	unrequested bool

	staged        StagedCall
	stagedSources []string
	stagedMeta    map[string]any

	verdict   HookVerdict
	hookNotes []string // pre_tool + post_tool annotations, for the agent
	outbound  []OutboundIdentifier
	rot       *RuleOfTwoHit

	histScope  string
	histRef    ActionRef
	dispatched bool
	approver   string

	isMCP       bool
	resultClass SourceClass
}

// callFinish is a deferred hook a stage hands back to Execute; it receives
// Execute's final response (and may amend it, as the failure observer does).
type callFinish func(out *ToolCallResponse)

// stepFailureStage: repeated identical failure (step_failures.go). A stopped step
// runs nothing more; every failure of this call is counted per step lease, the
// one before the threshold carries a hint and the one at it stops the step. Only
// a plan step's call (the server resolved its lease to a run step: StepCallFrom)
// — a chat turn's lease is not a step the re-planner can re-plan.
func (e *ToolExecutor) stepFailureStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool, []callFinish) {
	req := c.req
	if _, isStep := StepCallFrom(ctx); isStep && e.StepFailures != nil && !req.System && req.SessionTokenID != "" {
		lease := req.SessionTokenID
		if stuck := e.StepFailures.Stuck(lease); stuck != nil {
			return ToolCallResponse{Denied: true, ArgHash: c.argHash,
				DenyReason: stuck.Error() + fmt.Sprintf(RepeatedFailureStop, stuck.Count)}, true, nil
		}
		origTool, origArgs := req.ToolName, append([]byte(nil), req.ArgsJSON...)
		return ToolCallResponse{}, false, []callFinish{func(out *ToolCallResponse) {
			e.observeFailure(lease, origTool, origArgs, out)
		}}
	}
	return ToolCallResponse{}, false, nil
}

// refusalRecordStage: ADR-0145 D14.5 — an identical call (tool + canonical
// arguments) this step had refused already is answered from the record, without
// running the checks, an approval or the tool again; every new refusal is
// recorded.
func (e *ToolExecutor) refusalRecordStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool, []callFinish) {
	req := c.req
	if e.guards == nil || e.guards.Refusals == nil || req.System {
		return ToolCallResponse{}, false, nil
	}
	scope := RefusalScope(ctx, req.SessionTokenID)
	origTool, origArgs := req.ToolName, append([]byte(nil), req.ArgsJSON...)
	if prev, ok := e.guards.Refusals.Lookup(scope, origTool, origArgs); ok {
		slog.Info("ADR-0145 D14: repeated refused call answered from the step's refusal record",
			"tool", origTool, "agent", req.AgentID, "reason", prev.DenyReason)
		prev.DenyReason = strings.TrimSuffix(prev.DenyReason, RepeatedRefusalNote) + RepeatedRefusalNote
		return prev, true, nil
	}
	return ToolCallResponse{}, false, []callFinish{func(out *ToolCallResponse) {
		if out.Denied && refusalIsFinal(out.ApprovalState) {
			e.guards.Refusals.Record(scope, origTool, origArgs, *out)
		}
	}}
}

// resolveStage resolves the tool and validates its arguments.
//
// A contributed local:<machine>/<tool> identity NEVER resolves from the
// kernel-global registry: it resolves per call from the task beneficiary's own
// live fleet (ADR-0127 D4), and this is also where the dispatch-layer scoping
// holds — a machine outside the beneficiary's fleet is refused HERE,
// structurally, whatever any menu said (the D9 defense in depth; the refusal is
// recorded as a decision). The two namespaces cannot answer for the same name:
// the registry refuses local: registrations at its chokepoint, and every
// contributed name carries the prefix. (Kernel-served tools resolved before
// this stage: their names are reserved, a registry tool of the same name is
// never reached.)
func (e *ToolExecutor) resolveStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool) {
	if strings.HasPrefix(c.req.ToolName, LocalToolPrefix) {
		res, reason, resolved := e.resolveContributed(ctx, c.req)
		if !resolved {
			return denied(reason, c.argHash), true
		}
		c.contrib = &res
		c.tool = res.tool
		// The ladder (D6) may have chosen the machine for a bare-capability
		// step: from here on the call IS the namespaced step it resolved to —
		// grants, effects, egress audit and dispatch all see the real target.
		c.req.ToolName = res.tool.Name
	} else {
		tool, ok := e.Registry.Get(c.req.ToolName)
		if !ok {
			return denied("unknown tool", c.argHash), true
		}
		c.tool = tool
	}

	// ADR-0145 D9: arguments are validated against the tool's schema, and coerced
	// where unambiguous (a JSON array sent as a string), before anything else sees
	// them; an invalid call is an error the model can correct, and never runs.
	if e.argValidator != nil && len(c.tool.Schema) > 0 {
		fixed, verr := e.argValidator.ValidateArgs(c.req.ToolName, c.tool.Schema, c.req.ArgsJSON)
		if verr != nil {
			return ToolCallResponse{Error: verr.Error(), ArgHash: c.argHash, ResultHash: hashString(verr.Error())}, true
		}
		if string(fixed) != string(c.req.ArgsJSON) {
			c.req.ArgsJSON = fixed
			c.argHash = hashBytes(fixed)
		}
	}
	// The arguments an approval's intent stores (tool_recovery.go): validated,
	// before CID hydration (authorize resolves references again, under the
	// access in force when the call is carried out), and after a pre_tool
	// rewrite (policyStage).
	c.intentArgs = c.req.ArgsJSON
	return ToolCallResponse{}, false
}

// writeKeyStage: the subtask effect ceiling and deterministic keys, the action
// key, and the step idempotency record.
func (e *ToolExecutor) writeKeyStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool, []callFinish) {
	req, tool := c.req, c.tool
	var fin []callFinish
	// ADR-0150 D2: a call on a subtask's child lease must stay inside the lease's
	// effect ceiling (default read + egress_read). Checked here, by the kernel,
	// before any grant, approval or dispatch, whatever the authorizer would say:
	// the ADR-0086 effect gate asks the authorizer, which allows everything in OSS.
	c.subtaskLease = !req.System && e.subtasks.IsChild(req.SessionTokenID)
	if c.subtaskLease {
		if why := e.subtasks.EffectRefusal(req.SessionTokenID, tool); why != "" {
			slog.Info("ADR-0150 D2: call outside the subtask effect ceiling refused",
				"tool", req.ToolName, "agent", req.AgentID, "lease", req.SessionTokenID)
			return denied(why, c.argHash), true, nil
		}
	}
	// ADR-0150 D2/D5: every side-effecting call inside a subtask carries a
	// deterministic key (subtask id + tool + canonical args + occurrence), so a
	// resumed subtask answers a call that already ran from its stored result.
	// The key is also the action's identity on the wire (ADR-0154 D4, ENG-812): it
	// travels in the MCP request's _meta like a plan step's, and the call is
	// recorded in the durable action record when the registry has one (ENG-1428).
	var subtaskKey string
	if c.subtaskLease && tool.SideEffecting() {
		if key, ok := e.subtasks.CallKey(req.SessionTokenID, req.ToolName, req.ArgsJSON); ok {
			subtaskKey = key
			sid, _ := SessionIDFromContext(ctx)
			stored, found, finish := e.subtasks.Idempotency().BeginAction(ctx, DurableAction{
				Key: key, Tool: req.ToolName, Principal: RunPrincipalFromContext(ctx),
				SessionID: sid, ArgsHash: c.argHash,
			})
			if found {
				slog.Info("ADR-0150 D5: resumed subtask's side-effecting call answered from its stored result",
					"tool", req.ToolName, "lease", req.SessionTokenID)
				return stored, true, fin
			}
			fin = append(fin, func(out *ToolCallResponse) { finish(*out, !out.Denied && out.Error == "") })
		}
	}
	// ADR-0154 D4 (ENG-812): a side-effecting call made by a plan step carries its
	// deterministic action key to the target, in the MCP request's _meta
	// (ActionKeyMetaKey), so the target — or premium's write-back confirmation —
	// can recognise a retry of the same action. Computed whether or not
	// idempotent_writes is on: it is the action's identity, not a cache key.
	c.actionKey = subtaskKey
	if tool.SideEffecting() && !req.System && !c.subtaskLease {
		if sc, ok := StepCallFrom(ctx); ok {
			c.actionKey = IdempotencyKey(sc.RunID, sc.StepIndex, req.ToolName, req.ArgsJSON)
		}
	}
	// ENG-1634: a side-effecting call a chat turn makes on its own lease (not a
	// subtask's, not a plan step's) is recorded as begun — the chat lane re-drives
	// a killed turn only when its attempt began none — and keyed turn-scoped. The
	// key is the action's identity on the wire, and a re-drive that repeats an
	// identical call is answered from the stored result instead of running again.
	if e.turnCalls != nil && tool.SideEffecting() && !req.System && !c.subtaskLease {
		if _, isStep := StepCallFrom(ctx); !isStep {
			if key, ok := e.turnCalls.Begin(req.SessionTokenID, req.ToolName, req.ArgsJSON); ok {
				c.actionKey = key
				sid, _ := SessionIDFromContext(ctx)
				stored, found, finish := e.turnCalls.Idempotency().BeginAction(ctx, DurableAction{
					Key: key, Tool: req.ToolName, Principal: RunPrincipalFromContext(ctx),
					SessionID: sid, ArgsHash: c.argHash,
				})
				if found {
					slog.Info("ENG-1634: a re-driven turn's side-effecting call answered from its stored result",
						"tool", req.ToolName, "lease", req.SessionTokenID)
					return stored, true, fin
				}
				fin = append(fin, func(out *ToolCallResponse) { finish(*out, !out.Denied && out.Error == "") })
			}
		}
	}
	// ADR-0145 D9: a side-effecting call made by a plan step has an idempotency
	// key; a retry of the same call returns the stored result.
	if e.idempotency != nil && tool.SideEffecting() && !req.System && !c.subtaskLease {
		if sc, ok := StepCallFrom(ctx); ok {
			sid, _ := SessionIDFromContext(ctx)
			stored, found, finish := e.idempotency.BeginAction(ctx, DurableAction{
				Key:  IdempotencyKey(sc.RunID, sc.StepIndex, req.ToolName, req.ArgsJSON),
				Tool: req.ToolName, RunID: sc.RunID, StepIndex: sc.StepIndex,
				Principal: RunPrincipalFromContext(ctx), SessionID: sid, ArgsHash: c.argHash,
			})
			if found {
				slog.Info("ADR-0145 D9: duplicate side-effecting call answered from its stored result",
					"tool", req.ToolName, "run", sc.RunID, "step", sc.StepIndex)
				return stored, true, fin
			}
			fin = append(fin, func(out *ToolCallResponse) { finish(*out, !out.Denied && out.Error == "") })
		}
	}
	return ToolCallResponse{}, false, fin
}

// authorizeStage: grant → CID hydration → resource policy → scope → effects
// (authorize), then the request-tied guards and the staged inputs.
func (e *ToolExecutor) authorizeStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool, []callFinish) {
	var fin []callFinish
	grant, reason, authorized := e.authorize(ctx, &c.req, c.tool)
	if !authorized {
		return denied(reason, c.argHash), true, nil
	}
	c.grant = grant

	// ADR-0145 D14: the request-tied guards (constraints, the write-target record,
	// the repair-turn write guard). unrequested is a write no open write/document
	// item asked for: it goes to approval with a notice (approvalStage).
	if !c.req.System {
		g := e.requestGuards(ctx, c.req, c.tool, c.argHash)
		if g.stop {
			return g.resp, true, nil
		}
		c.unrequested = g.unrequested
		if g.finish != nil {
			fin = append(fin, func(out *ToolCallResponse) { g.finish(*out) })
		}
	}

	// Staged inputs (option C, domain/staged_inputs.go): resolved under the
	// caller's access AFTER every authorization above, so a call the kernel would
	// refuse never reads anything; an input the caller cannot read refuses the
	// call with nothing staged. The folder lives until this call returns.
	if c.tool.StagedInputs {
		sc, resp, stop := e.stageInputs(ctx, c.req, c.tool, c.argHash)
		if stop {
			return resp, true, fin
		}
		if sc != nil {
			c.staged = sc
			fin = append(fin, func(*ToolCallResponse) { sc.Cleanup() })
		}
	}
	if c.staged != nil {
		c.stagedSources, c.stagedMeta = c.staged.Sources(), c.staged.Meta()
	}
	return ToolCallResponse{}, false, fin
}

// policyStage: the verdict, the pre_tool hooks, the outbound identifier guard
// and the Rule-of-Two gate. Each can only narrow the verdict.
func (e *ToolExecutor) policyStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool) {
	// The policy decision: allow, or ask for a dangerous tool. A2.2: an operator
	// ScopeSystem execution carries its own authority — no per-agent HITL gate. The
	// operator command handler audits the call and emits a dangerous-tool feed
	// event so the privileged action stays visible.
	c.verdict = HookAllow
	if (c.tool.Dangerous || c.unrequested) && !c.req.System {
		c.verdict = HookAsk
	}

	// ADR-0139 D11 pre_tool hooks, AFTER the policy decision. They can only narrow
	// it (allow → ask → deny). A rewritten call is re-authorized from the grant
	// onward, so a rewrite can never reach what the original call could not.
	if e.Hooks.Has(HookPreTool) {
		hook := e.Hooks.Run(ctx, e.toolHookEvent(ctx, c.req, c.tool, HookPreTool, c.verdict, nil))
		c.hookNotes = append(c.hookNotes, hook.Annotations...)
		if hook.ArgsRewritten {
			c.argHash = hashBytes(hook.Args) // the receipt names the call that runs
			c.req.ArgsJSON = hook.Args
			c.intentArgs = hook.Args
			g, why, ok := e.authorize(ctx, &c.req, c.tool)
			if !ok {
				slog.Warn("ADR-0139 D11: hook-rewritten tool call refused by policy",
					"tool", c.req.ToolName, "agent", c.req.AgentID, "reason", why)
				return denied("hook rewrite refused: "+why, c.argHash), true
			}
			c.grant = g
		}
		// Defence in depth over the chain's own rule: never looser than policy.
		c.verdict, _ = NarrowVerdict(c.verdict, hook.Verdict)
		if c.verdict == HookDeny {
			return denied(hook.DenyReason(), c.argHash), true
		}
	}

	// The outbound identifier guard (ENG-917, owner decision 2026-11-15). Placed
	// deliberately: AFTER argument validation, every authorization step and the
	// pre_tool hooks — so it reads the validated arguments exactly as they will
	// run (a hook rewrite included) and a call the kernel refuses anyway prompts
	// nobody — and BEFORE the approval gate, which it feeds. A call that can send
	// outside (egress_send; plain or inferred egress counts as send) whose text
	// arguments carry a personal identifier is narrowed to ask; the identifiers
	// are named in the approval request and the call is never rewritten or
	// masked. A dangerous call (undeclared tools included: read+write+egress)
	// already asks; the guard adds the identifiers to its approval. An operator
	// ScopeSystem call carries its own authority, as for the approval gate.
	if e.outboundIdentifiers != nil && !c.req.System {
		c.outbound = e.outboundIdentifiers.Check(c.tool, c.req.ArgsJSON)
		if len(c.outbound) > 0 {
			slog.Info("ENG-917: outbound call carries personal identifiers; approval required",
				"tool", c.req.ToolName, "agent", c.req.AgentID, "identifiers", len(c.outbound))
			c.verdict, _ = NarrowVerdict(c.verdict, HookAsk)
		}
	}

	// The Rule-of-Two gate (ENG-531, owner decision 2026-11-19). Same placement
	// and reasoning as the outbound guard above: after every authorization and
	// the pre_tool hooks (a call refused anyway prompts nobody), before the
	// approval gate it feeds. A write/egress call (configured gated effects) from
	// a session that has consumed untrusted content (or whose provenance the
	// kernel cannot tell) and holds sensitive access is narrowed to ask; what
	// tainted the session is named for the approver. The call is never rewritten.
	// An operator ScopeSystem call carries its own authority.
	if e.ruleOfTwo != nil && !c.req.System {
		if hit, gated := e.ruleOfTwo.Check(TaintScopeFromContext(ctx), c.tool, func() ([]string, error) {
			return e.grantedTags(ctx, c.req.AgentID)
		}); gated {
			c.rot = &hit
			slog.Info("ENG-531: Rule of Two — tainted session with sensitive access calls a gated tool; approval required",
				"tool", c.req.ToolName, "agent", c.req.AgentID, "sources", hit.Sources,
				"unknown_provenance", hit.UnknownProvenance, "effects", hit.Effects)
			c.verdict, _ = NarrowVerdict(c.verdict, HookAsk)
		}
	}
	return ToolCallResponse{}, false
}

// approvalStage: the approval gate (fail-closed) — a dangerous tool, or a call a
// hook or guard narrowed to ask — or the approval a recovered call carries.
//
// There is no evaluation-session exemption any more: the ADR-0037 interview
// that minted evaluation sessions was deleted 2026-11-05, and ADR-0139 D5
// qualification will run against a mock tool executor that records intended
// calls, never this one.
//
// The conversation's action history (action_history.go): a side-effecting call
// is recorded from the moment it asks for approval (or, needing none, is
// dispatched) until it ends — done, or dropped when it never ran or failed — and
// an approval it raises carries a notice naming any earlier call of the
// conversation it may duplicate (T-20261107-approver-is-not-told-that-an-identical-side).
// The history's finish is returned for Execute to defer, unconditionally.
func (e *ToolExecutor) approvalStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool, []callFinish) {
	req, tool := c.req, c.tool
	if e.History != nil && tool.SideEffecting() {
		hsid, _ := SessionIDFromContext(ctx)
		c.histScope = ActionHistoryScope(ConversationIDFromContext(ctx), string(hsid))
	}
	fin := []callFinish{func(out *ToolCallResponse) { e.finishHistory(ctx, c.histRef, c.dispatched, *out) }}

	if tool.Dangerous && req.System && c.verdict == HookAllow {
		c.approver = "operator:system"
	} else if pa, recovered := preApprovedFrom(ctx); recovered && c.verdict == HookAsk {
		// A call approved after the kernel that raised its approval restarted
		// (tool_recovery.go): the person's approval covers exactly the call they
		// were shown. Anything else asks nobody and runs nothing — a recovery
		// has no requester to wait on a new approval.
		if pa.tool != req.ToolName || pa.argHash != c.argHash {
			return denied("the call is not the one that was approved (approval "+pa.id+"); nothing was run", c.argHash), true, fin
		}
		c.approver = pa.approver
	} else if c.verdict == HookAsk {
		if e.approval == nil {
			if c.rot != nil {
				return denied(RuleOfTwoDenyReason(*c.rot), c.argHash), true, fin
			}
			if len(c.outbound) > 0 {
				return denied(OutboundIdentifierDenyReason(c.outbound), c.argHash), true, fin
			}
			if c.unrequested {
				return denied("approval required but unavailable: "+UnrequestedWriteNotice(req.ToolName), c.argHash), true, fin
			}
			return denied("approval required but unavailable", c.argHash), true, fin
		}
		// ADR-0142 D11: the request names who the call is for (kernel-held facts
		// only: the task beneficiary, else the conversation owner) and where it
		// came from, so the policy can route it and the hub can deliver it there.
		beneficiary := TaskBeneficiaryFromContext(ctx)
		if beneficiary.IsZero() {
			beneficiary = PersonPrincipal(ConversationOwnerFromContext(ctx))
		}
		sid, _ := SessionIDFromContext(ctx)
		notice := ""
		if c.histScope != "" {
			c.histRef = e.History.Begin(c.histScope, req.ToolName, req.ArgsJSON, ActionAwaitingApproval)
			notice = RenderDuplicateNotice(e.History.Matches(ctx, c.histScope, req.ToolName, req.ArgsJSON, c.histRef))
		}
		if c.unrequested {
			// ADR-0145 D14: the same notice channel as the duplicate notice (the
			// approval item's duplicate_notice field), so every surface shows it.
			notice = joinNotices(UnrequestedWriteNotice(req.ToolName), notice)
		}
		// ENG-917: the identifiers the guard found, named for the approver on the
		// same notice channel (every surface shows it), ahead of the duplicate
		// and unrequested-write notices.
		if len(c.outbound) > 0 {
			notice = joinNotices(RenderOutboundIdentifierNotice(c.outbound), notice)
		}
		// ENG-531: the Rule-of-Two notice leads — it is the reason the person is
		// asked at all when nothing else would have asked.
		if c.rot != nil {
			notice = joinNotices(RenderRuleOfTwoNotice(*c.rot), notice)
		}
		// ADR-0150 D5.1: an approval raised inside a subtask reaches the person
		// labelled with the subtask; only this call (so only that subtask) waits.
		subtaskLabel := ""
		if c.subtaskLease {
			if b, ok := e.subtasks.Binding(req.SessionTokenID); ok {
				subtaskLabel = b.Label
				notice = joinNotices(SubtaskApprovalNotice(b.Label), notice)
			}
		}
		dec, err := e.approval.Request(ctx, ApprovalRequest{
			AgentID: req.AgentID, ToolName: req.ToolName, ArgsPreview: preview(req.ArgsJSON, 200),
			Object: objectOfArgs(req.ArgsJSON), Beneficiary: beneficiary, Args: req.ArgsJSON,
			ConversationID: ConversationIDFromContext(ctx), SessionID: string(sid),
			DuplicateNotice: notice, Subtask: subtaskLabel,
			PersonalIdentifiers: c.outbound,
			Intent:              toolIntent(ctx, req, c.intentArgs, c.subtaskLease, c.contrib != nil),
		})
		if err != nil || !dec.Approved {
			reason := ApprovalDenyReason(dec, err)
			slog.Info("tool call not approved", "tool", req.ToolName, "agent", req.AgentID,
				"state", string(dec.State), "approval", dec.ApprovalID, "beneficiary", beneficiary.String(),
				"reason", reason)
			return ToolCallResponse{Denied: true, DenyReason: reason, ArgHash: c.argHash, ApproverID: dec.ApproverID, ApprovalState: dec.State}, true, fin
		}
		c.approver = dec.ApproverID
		e.History.Approved(c.histRef, dec.ApprovalID)
	}
	return ToolCallResponse{}, false, fin
}

// contributedStage: ADR-0127 CL-2, the contributed pre-dispatch gate — parking
// for an offline target (D6), then the consent decision (D7). Deliberately AFTER
// every kernel authorization (a step the kernel would refuse anyway must not
// prompt anyone) and immediately BEFORE dispatch, so a consent approval approves
// what actually runs.
func (e *ToolExecutor) contributedStage(ctx context.Context, c *toolCall) (ToolCallResponse, bool) {
	if c.contrib == nil {
		return ToolCallResponse{}, false
	}
	resp, proceed := e.contributedGate(ctx, c.req, c.contrib, c.argHash)
	if !proceed {
		return resp, true
	}
	c.tool = c.contrib.tool // parking re-resolves against the fresh manifest
	return ToolCallResponse{}, false
}

// dispatchStage picks the handler, records egress, admits the call against the
// budget and runs it. A failed call is answered here (stop); a successful one
// returns its raw result for resultStage.
func (e *ToolExecutor) dispatchStage(ctx context.Context, c *toolCall) ([]byte, ToolCallResponse, bool) {
	req, tool, argHash := c.req, c.tool, c.argHash
	// Dispatch. ADR-0043: an mcp:<server>/<tool> identity routes to the MCP
	// handler; ADR-0127 D5: a local:<machine>/<tool> identity routes to the
	// worker relay; anything else goes to the default Handler (nil in
	// production, ENG-1421 ⇒ refused below). All implement ToolHandler, so
	// authorization above is identical — no prefix ever changes what a call must
	// pass, only who executes it.
	handler := e.Handler
	c.isMCP = strings.HasPrefix(req.ToolName, "mcp:")
	if c.isMCP && e.MCPHandler != nil {
		handler = e.MCPHandler
	}
	isLocal := strings.HasPrefix(req.ToolName, LocalToolPrefix)
	if isLocal {
		handler = e.LocalHandler // nil ⇒ "no tool handler configured" below
	}
	// ADR-0043 D4: a remote MCP call sends the agent's args outside the trust
	// boundary — record the egress (the call itself is allowed; the operator owns
	// endpoint trust, and Regime-1 above already enforced any declared data class).
	// A contributed call egresses too, unconditionally (ADR-0127, owner ruling
	// 2026-08-20): the arguments leave the deployment for the consumer's machine
	// even when the tool only reads.
	if (c.isMCP || isLocal) && e.EgressAuditor != nil {
		e.EgressAuditor.RecordEgress(req.AgentID, req.ToolName, tool.DataWriteKinds)
	}
	if handler == nil {
		return nil, ToolCallResponse{Error: "no tool handler configured", ArgHash: argHash}, true
	}

	// ADR-0043 budget regime (admission): reserve the estimated cost before
	// dispatch; deny budget_exhausted if it does not fit the session budget. The
	// hold is reconciled to actual (or released) after the call.
	pricing, priced := e.priceFor(req)
	var holdID string
	if priced {
		id, rerr := e.Budget.Reserve(pricing.Reserve(), budgetAccountFor(req))
		if rerr != nil {
			var be *BudgetExhaustedError
			if errors.As(rerr, &be) {
				return nil, denied("budget_exhausted: "+be.Error(), argHash), true
			}
			return nil, ToolCallResponse{Error: rerr.Error(), ArgHash: argHash}, true
		}
		holdID = id
	}

	if c.histScope != "" && c.histRef.IsZero() {
		c.histRef = e.History.Begin(c.histScope, req.ToolName, req.ArgsJSON, ActionRunning)
	}
	c.dispatched = true
	if c.subtaskLease {
		e.subtasks.ObserveCall(req.SessionTokenID, tool)
	}
	callMeta := c.stagedMeta
	if c.actionKey != "" {
		callMeta = make(map[string]any, len(c.stagedMeta)+1)
		for k, v := range c.stagedMeta {
			callMeta[k] = v
		}
		callMeta[ActionKeyMetaKey] = c.actionKey
	}
	result, err := handler.Execute(ctx, ToolCall{ToolName: req.ToolName, ArgsJSON: req.ArgsJSON, Policy: c.grant.Policy, Meta: callMeta})
	// ENG-531: the session consumes what the tool answered — its result, or its
	// error text (a remote server writes that too). An operator System call is
	// not a model's input. Observed before the post_tool hooks: a hook may
	// redact or withhold, but the tool already ran against the session's lease
	// and its answer is the provenance fact.
	c.resultClass = ToolResultSourceClass(tool)
	if !req.System {
		e.observe(ctx, c.resultClass, tool.AuthzTags())
	}
	if err != nil {
		return nil, e.failedCall(ctx, c, err, priced, pricing, holdID), true
	}
	if priced {
		// Reconcile to actual. Usage-field extraction is a follow-up; absent it,
		// reconcile is exact for flat and cap-on-unmeasurable for per_unit (D6).
		_ = e.Budget.Reconcile(holdID, pricing.Reconcile(0, false))
	}
	return result, ToolCallResponse{}, false
}

// failedCall answers a call whose handler returned an error.
func (e *ToolExecutor) failedCall(ctx context.Context, c *toolCall, err error, priced bool, pricing ToolPricing, holdID string) ToolCallResponse {
	req, tool, argHash := c.req, c.tool, c.argHash
	if priced {
		// Failure-cost (D7): never-reached ⇒ 0; otherwise the per-server policy.
		reached := !strings.Contains(err.Error(), "not connected")
		_ = e.Budget.Reconcile(holdID, pricing.FailureCost(reached, 0, false))
	}
	if c.contrib != nil && errors.Is(err, ErrWorkerConsentDenied) {
		// The machine itself declined the step (on-machine-only knob,
		// ADR-0127 D7): a recorded REFUSAL, not a worker error. The deny
		// text is kernel-authored; nothing worker-written rides it.
		e.recordFleetDecision(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonConsentDeniedOnMachine,
			Detail:    "machine " + strconv.Quote(c.contrib.machine) + " declined consent locally",
		})
		c.dispatched = false // the machine refused it: it never ran
		return denied("consent denied on machine "+strconv.Quote(c.contrib.machine), argHash)
	}
	if c.contrib != nil {
		// The relay itself failed: the machine stopped answering, the call
		// deadline passed, the transport broke. The step was consented and
		// dispatched, so the ONLY record of what became of it is this one —
		// a machine vanishing mid-step is the failure an operator most
		// needs to see, and it used to leave the journal at the consent
		// decision with nothing after it.
		e.recordFleetDecision(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonRelayFailed,
			Detail:    "machine " + strconv.Quote(c.contrib.machine) + ": " + err.Error(),
		})
	}
	// ADR-0145 D1: a call that ran and failed is a result too (D5 needs the
	// error to judge a "not found" honestly).
	if errJSON, merr := json.Marshal(map[string]string{"error": err.Error()}); merr == nil {
		id := e.recordToLedger(ctx, req, tool, errJSON, c.stagedSources)
		if c.subtaskLease {
			e.subtasks.AddEvidence(req.SessionTokenID, id)
		}
	}
	failed := ToolCallResponse{Error: err.Error(), ArgHash: argHash, ResultHash: hashString(err.Error()), ApproverID: c.approver, SourceClass: c.resultClass}
	// ADR-0043 D4: a tool-reported error's message is written by the tool
	// server — untrusted like a result. Scanned here, fenced at the agent
	// boundary; Error itself stays raw so the repeated-failure detector
	// compares stable text.
	var reported *ToolReportedError
	if c.isMCP && !req.System && errors.As(err, &reported) {
		u := e.untrustedOutput.Inspect(req.ToolName, []byte(reported.Message))
		u.SourceClass = ToolResultSourceClass(tool) // ENG-2223: the fence's source=
		logUntrustedHit(u)
		if e.untrustedOutput.Withholds(u) {
			failed.Error = withheldReason(u)
			failed.ResultHash = hashString(failed.Error)
			return failed
		}
		u.ErrorFromSource = true
		failed.Untrusted = &u
		if note := untrustedNote(u); note != "" {
			failed.Annotations = append(failed.Annotations, note)
		}
	}
	return failed
}

// resultStage turns a successful call's raw result into the response: staged
// outputs, binary blocks, post_tool hooks, the untrusted-output scan, the memory
// report, the ledger record and offload.
func (e *ToolExecutor) resultStage(ctx context.Context, c *toolCall, result []byte) ToolCallResponse {
	req, tool, argHash := c.req, c.tool, c.argHash
	// (The "_artifacts" promotion of files a confined tool.py handler swept into
	// the ContentStore went with that handler — ENG-1421, ENG-2241. A result's
	// "_artifacts" key is not read: an MCP server must not be able to name a
	// content-store CID and have the kernel copy it into a run ledger or to disk.)
	// A staged call's output files become artifact ledger entries derived from
	// its inputs; the result gains a note naming them (staged_inputs.go).
	var stagedRecorded map[string]string
	if c.staged != nil {
		result, stagedRecorded = e.recordStagedOutputs(ctx, req, tool, c.staged, result)
	}
	// Binary blocks become ledger references at record time, so the ledger, memory,
	// the hooks and the agent all see the same readable result. An operator System
	// call is left as the tool returned it (it records nothing and is not a model's
	// input) (T-20261112-worker-resolve-cid-inline-has-no-size-cap-a).
	if !req.System {
		result = e.referenceBinaryBlocks(ctx, req, tool, result, stagedRecorded, c.stagedSources)
	}

	// ADR-0139 D11 post_tool hooks: they may rewrite the output (may_rewrite; e.g.
	// redaction) or withhold it (narrow to deny). The call already ran; a withheld
	// output is not recorded to memory and never reaches the agent.
	if e.Hooks.Has(HookPostTool) {
		hook := e.Hooks.Run(ctx, e.toolHookEvent(ctx, req, tool, HookPostTool, HookAllow, result))
		c.hookNotes = append(c.hookNotes, hook.Annotations...)
		if hook.ResultRewritten {
			result = hook.Result
		}
		if hook.Verdict != HookAllow {
			return ToolCallResponse{Denied: true, DenyReason: hook.DenyReason() + " (output withheld)", ArgHash: argHash, ApproverID: c.approver}
		}
	}

	// ADR-0043 D4 (ENG-117): every MCP response is untrusted data. It is scanned
	// here — after binary blocks became references and post_tool hooks ran, so
	// the scan reads what the agent would — and fenced at the agent boundary
	// (AgentToolResult). A withheld hit is replaced by a kernel notice BEFORE
	// memory and the ledger see it; the call still succeeded (it ran, and a
	// write it made must not be retried), only its output is kept from the
	// agent. An operator System call is not a model's input and is left alone.
	var untrusted *UntrustedOutput
	if c.isMCP && !req.System {
		u := e.untrustedOutput.Inspect(req.ToolName, result)
		u.SourceClass = ToolResultSourceClass(tool) // ENG-2223: the fence's source=
		logUntrustedHit(u)
		if e.untrustedOutput.Withholds(u) {
			result, _ = json.Marshal(map[string]string{"output_withheld": withheldReason(u)})
		} else {
			untrusted = &u
			if note := untrustedNote(u); note != "" {
				c.hookNotes = append(c.hookNotes, note)
			}
		}
	}

	// Report the output to memory, routed by the tool's declared nature (ADR-0049 D1):
	// a MUTATION is an EVENT and becomes an action record regardless of size (a write
	// that happened is durable history). Best-effort — never fails the call. A denied
	// call is skipped outright: it never ran, so it touched nothing.
	if e.ToolOutput != nil && !isDeniedResult(result) {
		isMutation := len(tool.DataWriteKinds) > 0
		_ = e.ToolOutput.RecordToolOutput(ctx, ToolOutputRecord{
			ToolName:           req.ToolName,
			ArgsJSON:           req.ArgsJSON,
			Output:             result,
			IsMutation:         isMutation,
			TaskID:             req.TaskID,
			ClassificationTags: tool.AuthzTags(),
		})
	}

	resp := ToolCallResponse{ResultJSON: result, ArgHash: argHash, ResultHash: hashBytes(result), ApproverID: c.approver, Annotations: c.hookNotes, Untrusted: untrusted, SourceClass: c.resultClass}
	// ADR-0145 D1: with the ledger on, every result is recorded, and a large one
	// travels as its ledger id instead of a plan-scoped content-store cid (one
	// pathway: the content store holds no tool results then). A result the ledger
	// could not record stays inline — never lost to the agent.
	if e.Ledger != nil {
		id := e.recordToLedger(ctx, req, tool, result, c.stagedSources)
		if c.subtaskLease {
			e.subtasks.AddEvidence(req.SessionTokenID, id)
		}
		switch {
		case id != "" && e.InlineThreshold > 0 && len(result) > e.InlineThreshold:
			resp.ResultCID = id
			resp.ResultJSON = nil
			s := NewOffloadSummary(id, req.ToolName, result, e.previewChars(), e.PageBytes)
			resp.Offloaded = &s
		case id != "" && (e.ReturnLedgerRef || ledgerRefWanted(ctx) || len(result) > e.previewChars()):
			// Inline result plus its reference (ADR-0145 D6): a worker can cite
			// it, and — for anything longer than a preview — hand it on unchanged
			// with answer_ref instead of re-typing it
			// (T-20261112-summarise-a-document-step-gets-only-the-first).
			resp.ResultCID = id
		}
		return resp
	}
	// Legacy path (ledger off): offload large results to CAS.
	if e.ContentStore != nil && e.InlineThreshold > 0 && len(result) > e.InlineThreshold {
		if cid, serr := e.ContentStore.Put(ctx, result, "tool_result", nil, preview(result, 500)); serr == nil {
			resp.ResultCID = string(cid)
			resp.ResultJSON = nil
			s := NewOffloadSummary(string(cid), req.ToolName, result, e.previewChars(), e.PageBytes)
			s.Read = fmt.Sprintf("resolve_cid cid='%s' page=1..%d; search inside it: resolve_cid cid='%s' find='<text>'", cid, max(s.Pages, 1), cid)
			resp.Offloaded = &s
		}
	}
	return resp
}
