package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ADR-0150 D1: the single-agent lane for Execute callers (ingress pipelines,
// scheduled work, the benchmark transport). It is the ADR-0050 D1 branch of
// Server.Execute promoted from control arm to the default lane: no router, no
// planner, no checklist, no coverage — one agent with the whole request, bound
// like a plan step (session, run, acting-for, ceiling, liveness).

// RequestComposer composes the worker of a whole request (compose.Composer).
type RequestComposer interface {
	ComposeRequest(ctx context.Context, lease domain.LeaseID, brief string, in *domain.Handoff) (string, domain.WorkerComposition, error)
	// ComposeTurnRequest composes for a lease whose process is already chosen
	// (the chat turn's merged agent): third-party professions are left out
	// (ENG-2083).
	ComposeTurnRequest(ctx context.Context, lease domain.LeaseID, brief string) (domain.WorkerComposition, error)
}

// singleAgentDelegationRefusal is the lane's refusal of delegate_to_planner,
// or "" when the caller may run. There is no planner in this lane (ADR-0150 D1,
// owner 2026-11-13: the agent fans out by run_subtasks, never by delegating):
//   - a worker's delegation (a plan-step lease calling Execute, ADR-0145 D14)
//     would dispatch to the same single agent again, unbounded;
//   - a chat turn's delegation (a turn lease: conversation and ordering message,
//     no run) is the separate front desk ADR-0150 D3 removed; the merged agent is
//     not offered the tool, and a call to it is refused here, not only hidden.
func singleAgentDelegationRefusal(b domain.LeaseBinding, known bool) string {
	if !known {
		return ""
	}
	if b.RunID != "" && b.StepIndex >= 0 {
		return "worker delegation refused: execution.lane.default single_agent runs every request on one agent and has no planner to hand work to; finish with what you have and report what is missing"
	}
	if b.RunID == "" && b.ConversationID != "" && b.OriginMessageID != "" {
		return "delegate_to_planner refused: in the single-agent lane (execution.lane.default single_agent) the conversation agent does the work itself with its own tools; there is no planner to hand it to"
	}
	return ""
}

// laneLimits are the D4 caps and stuck thresholds the lane's agent runs under.
// The failing half of the stuck detector is execution.tools.repeated_failure_stop
// (one setting for the kernel's per-step stop and the agent loop).
func (s *Server) laneLimits() domain.LaneLimits {
	l := s.ExecCfg.Lane
	return domain.LaneLimits{
		MaxToolRounds:      l.ToolRounds(),
		MaxWallSeconds:     l.WallSeconds(),
		SameResultRepeats:  l.SameResultRepeats(),
		SameFailureRepeats: s.ExecCfg.Tools.RepeatedFailureStop,
	}
}

// LaneLimits exposes laneLimits to the composition root (the chat lane stamps
// the same limits on a turn).
func (s *Server) LaneLimits() domain.LaneLimits { return s.laneLimits() }

// executeSingleAgent dispatches the user's input verbatim to the lane's agent:
// execution.lane.single_agent.agent_id when pinned, else the composed runtime
// with a composition of the whole request (RequestComposer). Everything
// downstream of agent selection is identical to a plan step (CallAgent seam:
// priming, grants, scope, telemetry), and so is the identity the step runs
// under: Execute calls this only after it opened the session (linked to the
// ordering conversation) and registered the ordering chat turn (delegated), so
// the lease is bound like a step lease (session, run, step 0, agent) — which is
// what gives a tool approval its beneficiary and the run its principal — the
// step is told whom it acts for, and the run is under the same plan ceiling and
// turn liveness as a plan (plan_watchdog.go), which stay outer guards over the
// lane's own D4 caps (stamped on the handoff, enforced in the agent loop).
// The "/plan " slash-prefix is stripped, as the router would have consumed it.
func (s *Server) executeSingleAgent(ctx context.Context, in *pb.Handoff, rawInput string,
	sessionID domain.SessionID, delegated *DelegatedRun, reserve callerReserve) (*pb.Handoff, error) {
	agentID := s.ExecCfg.Lane.SingleAgent.AgentID
	if agentID == "" && (s.RequestComposer == nil || s.LLMGateway == nil) {
		return nil, status.Error(codes.FailedPrecondition,
			"single-agent lane: execution.lane.single_agent.agent_id is empty and there is no composed runtime to compose the request on (execution.workers.composed is off); pin an agent or turn composed workers on")
	}
	// The run id: the lease's run (RunActivity liveness, the worker-delegation
	// refusal in Execute) and the turn registry's name for this work.
	runID := domain.RunID(newPlanID())
	operator.AttachRunID(ctx, runID) // contract 0102 CancelRun by run_id
	// The session's spend before the lane's run, so the result reports what THIS
	// run cost and which generators answered it (ADR-0131 _llm_* keys; ENG-453
	// _llm_models), the same figures a plan's result carries.
	spendBase := s.SpendMeter.Session(sessionID)
	userInput := strings.TrimSpace(rawInput)
	if rest, ok := strings.CutPrefix(userInput, "/plan "); ok {
		userInput = strings.TrimSpace(rest)
	}

	handoff := &domain.Handoff{
		Payload: &domain.Payload{Type: "task", Data: []byte(userInput)},
		Context: make(map[string]string),
	}
	for k, v := range in.GetMetadata() {
		handoff.Context[k] = v
	}
	handoff.Context["task_id"] = "task-0"
	handoff.Context["_step_index"] = "0"
	handoff.Context["original_prompt"] = userInput
	// ADR-0150 D4: the stopping rule's caps and stuck thresholds; the agent
	// loop enforces them (sdk stopping.py) and marks a capped result partial.
	s.laneLimits().Stamp(handoff.Context)
	// Kernel-authored, as on every plan step: whom this run acts for (the run
	// principal); a caller's metadata copied above cannot assert it.
	actingForID, actingForName := s.actingForRun(ctx, sessionID, runID)
	domain.StampActingFor(handoff.Context, actingForID, actingForName)

	// Mirror stepFn's session-token acquisition so cognitive agents can call
	// GenerateViaModelStream exactly as a plan step does: an empty allocation, so
	// the model resolves through the gateway's configured default.
	if s.LLMGateway != nil {
		tokenID, _ := s.LLMGateway.Acquire(ctx, domain.StepAllocation{}, StepLeaseTokenLimit, StepLeaseDuration)
		bind := func(agent string) {
			// Bound like a plan step's lease: the session (and through it the
			// ordering conversation and its owner: approval beneficiary, run
			// principal), the run and the agent (ADR-0139 D8 ownership).
			if r := s.leaseResolver(); r != nil {
				r.BindLease(tokenID, domain.LeaseBinding{
					SessionID:       sessionID,
					RunID:           runID,
					StepIndex:       0,
					AgentID:         agent,
					DelegationDepth: domain.DelegationDepthFrom(ctx),
				})
			}
		}
		bind(agentID)
		// ADR-0150 D2: the lane's agent may fan out with run_subtasks (depth 0);
		// re-registered below when the composed runtime takes the lease.
		s.RegisterSubtaskParent(tokenID, agentID)
		// Revoked with the ordering chat turn (turn_delegation.go).
		delegated.AddLease(tokenID)
		handoff.Context["_session_token_id"] = string(tokenID)
		defer func() {
			// ADR-0046 D6 ephemerality, same as the dispatch path: Complete runs the
			// gateway's completion hook (ToolExecutor.EndRun, the composition's Forget).
			_, _ = s.LLMGateway.Complete(ctx, tokenID)
		}()
		if agentID == "" {
			// ADR-0150 D1: the composed runtime with the person's permitted tools,
			// composed under the run principal so the scope check sees the person.
			cctx := s.withRunPrincipal(withPresentedLease(ctx, tokenID), tokenID)
			if s.ledgerOn() {
				cctx = domain.WithLedgerProvenance(cctx, s.toolCallProvenance(cctx, tokenID))
			}
			runtime, _, err := s.RequestComposer.ComposeRequest(cctx, tokenID, userInput, handoff)
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "single-agent lane: composing the request's worker: %v", err)
			}
			agentID = runtime
			bind(agentID)
			s.RegisterSubtaskParent(tokenID, agentID)
		}
	}

	// The same progress-aware ceiling and turn liveness as a plan: idle for
	// plan_idle_timeout_ms or past plan_max_duration_ms ends it, and the ordering
	// chat turn's stall watchdog sees the agent's model and tool calls on its lease.
	act := newPlanActivity()
	maxDuration, idleTimeout := planLimits(s.ExecCfg.Plan)
	probe := s.planLivenessProbe(ctx, act, runID, sessionID)
	reservedCtx, stopCallerReserve := reserve.bound(ctx, errPlanCallerDeadline(reserve.reserve))
	defer stopCallerReserve()
	callCtx, stopCeiling := startPlanCeiling(reservedCtx, maxDuration, idleTimeout, probe)
	defer stopCeiling()
	delegated.Attach(runID, probe)
	act.touch()
	defer act.touch()

	slog.Info("⚡ SINGLE-AGENT LANE (ADR-0150 D1)", "agent", agentID, "run", runID, "session", sessionID,
		"acting_for", actingForID)
	resp, err := s.Dispatcher.CallAgent(callCtx, agentID, handoff, "")
	// Contract 0102: an operator CancelRun ended the run. The lane persists no run
	// record, so nothing can resume it; the answer says who stopped it and why.
	if rc, cancelled := operator.RunCancelled(ctx); cancelled {
		return nil, status.Error(codes.Canceled, rc.Error())
	}
	if err != nil {
		return nil, fmt.Errorf("single-agent lane: agent %q: %w", agentID, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("single-agent lane: agent %q returned no handoff", agentID)
	}
	if resp.FromAgent == "" {
		resp.FromAgent = agentID
	}
	singleAgentOutcome(resp, runID, sessionID)
	out := handoffToProto(resp)
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	s.stampSpend(out.Metadata, sessionID, spendBase)
	return out, nil
}

// singleAgentOutcome maps what the lane's agent returned onto the Execute
// response metadata callers already read:
//   - a run ended at a D4 cap or by the stuck detector (payload metadata
//     partial=true, exit_reason): _partial_plan=true, _exit_reason, _resumable —
//     the person gets the partial answer, not an error (ADR-0150 D4.3);
//   - a top-level ask-back (payload type input_required, ADR-0150 D5): the
//     ADR-0145 D5a InputRequired shape under _input_required, with the questions
//     as the payload text. Only the main agent returns it; subtasks never do.
func singleAgentOutcome(resp *domain.Handoff, runID domain.RunID, sessionID domain.SessionID) {
	if resp.Context == nil {
		resp.Context = map[string]string{}
	}
	if sessionID != "" {
		resp.Context["_session_id"] = string(sessionID)
	}
	if resp.Payload == nil {
		return
	}
	pm := resp.Payload.Metadata
	if strings.EqualFold(pm[domain.AgentResultPartial], "true") {
		resp.Context["_partial_plan"] = "true"
		reason := pm[domain.AgentResultExitReason]
		if reason == "" {
			reason = "cap"
		}
		resp.Context[domain.HandoffKeyExitReason] = reason
		resp.Context[domain.HandoffKeyResumable] = "true"
		slog.Info("ADR-0150 D4: single-agent run ended partial", "run", runID, "exit_reason", reason)
	}
	if resp.Payload.Type != domain.PayloadTypeInputRequired {
		return
	}
	var ask struct {
		Questions []domain.InputQuestion `json:"questions"`
		Reason    string                 `json:"reason"`
	}
	if err := json.Unmarshal(resp.Payload.Data, &ask); err != nil || len(ask.Questions) == 0 {
		// Not a well-formed ask: delivered as the agent's text.
		resp.Payload.Type = "text"
		return
	}
	for i := range ask.Questions {
		if ask.Questions[i].ID == "" {
			ask.Questions[i].ID = fmt.Sprintf("Q%d", i+1)
		}
	}
	ir := &domain.InputRequired{
		Version:   domain.InputRequiredVersion,
		RunID:     string(runID),
		Questions: ask.Questions,
		Reason:    ask.Reason,
		Round:     1,
	}
	attachInputRequired(resp.Context, ir)
	resp.Payload.Type = "text"
	resp.Payload.Data = []byte(inputRequiredText(ir))
}

// ComposeLease binds the single-agent lane's composition of a request (the
// person's permitted tools, RequestComposer) to an existing lease: the chat
// turn's, so the merged conversation agent holds those tools (ADR-0150 D3). The
// scope check runs under the lease's run principal (the conversation owner).
// Without composed workers it returns an error and the turn keeps the chat
// agent's own grants.
func (s *Server) ComposeLease(ctx context.Context, lease domain.LeaseID, brief string) error {
	if s.RequestComposer == nil {
		return fmt.Errorf("no composed runtime (execution.workers.composed is off)")
	}
	cctx := s.withRunPrincipal(withPresentedLease(ctx, lease), lease)
	if s.ledgerOn() {
		cctx = domain.WithLedgerProvenance(cctx, s.toolCallProvenance(cctx, lease))
	}
	// The turn's agent is the chat agent, whose tier the composition cannot
	// lower: third-party professions stay out of it (ENG-2083).
	_, err := s.RequestComposer.ComposeTurnRequest(cctx, lease, brief)
	return err
}
