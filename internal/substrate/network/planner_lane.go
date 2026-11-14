package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
	"github.com/cambrian-sh/core/internal/substrate/operator"

	"github.com/cambrian-sh/core/internal/substrate/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The planner lane of Server.Execute (ADR-0145; kept behind
// execution.lane.default: planner, ADR-0150 D8), split out of server.go
// (ENG-1408, owner decision 2026-11-14, behaviour unchanged): the shared
// prologue and the lane switch stay in Execute; everything from mood injection to
// the plan report is here.

// plannerLaneRequest carries what Execute settled before the lane switch.
type plannerLaneRequest struct {
	rawInput         string
	authoredPlanJSON string
	sessionID        domain.SessionID
	delegated        *DelegatedRun
	reserve          callerReserve
	// The ADR-0131 router-spend bookkeeping: the caller's live session when it
	// routed under one, the session's spend before routing, and the tab the
	// router's calls went on otherwise (settled into the session below).
	callerSession domain.SessionID
	routeBase     llm.SessionSpend
	routeTab      *llm.SpendTab
}

// executePlanned runs one request through the planner lane.
func (s *Server) executePlanned(ctx context.Context, in *pb.Handoff, r plannerLaneRequest) (*pb.Handoff, error) {
	rawInput, authoredPlanJSON := r.rawInput, r.authoredPlanJSON
	sessionID, delegated, reserve := r.sessionID, r.delegated, r.reserve
	callerSession, routeBase, routeTab := r.callerSession, r.routeBase, r.routeTab
	userInput := rawInput

	// ADR-0025: FetchContext retired from planning path.
	// LTM enrichment now handled exclusively by WorkspaceStage.PrimeForPlanning
	// inside the Planner, injecting typed <FactLTM> and <NegativeLTM> sections.

	// Mood Injection: append last 3 SessionEvents as social context.
	if sessionID != "" {
		userInput = injectMoodContext(ctx, s, sessionID, userInput)
	}

	// Phase 3: either CONTINUE a named run (explicit, replaying its persisted plan) or
	// start a new one. There is deliberately no middle path — an unrequested resume is what
	// used to apply a stale step index to a freshly generated plan.
	var resumed *ResumedRun
	if rid := in.GetMetadata()[ResumeMetadataKey]; rid != "" {
		r, rerr := s.ResumeRun(ctx, domain.RunID(rid), sessionID)
		if rerr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", rerr)
		}
		resumed = r
	}

	// ADR-0131: the session's spend before this Execute, so the response can report
	// what THIS plan cost (the session total also carries earlier plans). The base
	// is taken BEFORE routing when the caller's session already existed, and the
	// router's tab is settled AFTER the base otherwise, so router classification
	// is part of this Execute's figures either way.
	spendBase := s.SpendMeter.Session(sessionID)
	if callerSession != "" && callerSession == sessionID {
		spendBase = routeBase
	}
	s.SpendMeter.SettleTab(routeTab, sessionID)

	// One planner-call budget for the whole plan: the validation retry below and
	// the DAG's replans draw on the same 1 + max_replan_attempts calls.
	planBudget := newPlanCallBudget(s.planCallLimit())

	// ADR-0145 D5: the requirement checklist, derived ONCE from the user's own words
	// before planning (coverage_stages.go). Not for an operator-authored plan or a
	// resumed run: there is no request to derive it from.
	var reqs *requestRequirements
	// staged is the ADR-0145 D11 staged planner's state across this Execute's
	// stages (staged_planner.go); nil unless execution.plan.staged_planner.
	var staged *stagedRun
	if s.checklistOn() && authoredPlanJSON == "" && resumed == nil {
		reqs = s.deriveRequirements(ctx, rawInput)
		ctx = domain.WithRequirementChecklist(ctx, reqs.checklist)
	}

	var plan *domain.ExecutionPlan
	switch {
	case resumed != nil:
		plan = resumed.Plan
		slog.Info("↩️ RESUMING RUN", "run", resumed.Run.ID, "from_step", resumed.StartFrom)
	case authoredPlanJSON != "":
		// Contract 0074: the operator wrote this plan. It is still validated here
		// — the operator plane validates before accepting, but this is the LAST
		// gate before execution and it must not depend on a caller having done
		// its job. Same TopologicalSort, so the two answers cannot disagree.
		authored, aerr := parseAuthoredPlan(authoredPlanJSON)
		if aerr != nil {
			return nil, aerr
		}
		plan = authored
		slog.Info("📝 OPERATOR-AUTHORED PLAN", "subject", plan.Subject, "steps", len(plan.Steps))
	default:
		planningCtx, stopPlanningReserve := reserve.bound(ctx, errPlanningCallerDeadline("planning", reserve.reserve))
		var p *domain.ExecutionPlan
		var perr error
		var early *pb.Handoff
		if reqs != nil && s.stagedOn() {
			// ADR-0145 D11: stage 1 of the staged planner, which may instead ask
			// the person or find nothing to plan (staged_planner.go).
			p, staged, early, perr = s.planFirstStage(planningCtx, planBudget.planner(s.Planner), userInput, in, sessionID, reqs)
		} else {
			p, perr = s.planChecked(planningCtx, planBudget.planner(s.Planner), userInput)
		}
		planningCut := planCeilingCause(ctx, planningCtx)
		stopPlanningReserve()
		// Cut by the reserve — even a plan that arrived on the edge has no time
		// left to run, and dispatching it would start steps it cannot finish.
		if planningCut != nil {
			return s.planningOutOfTime(in, "planning", planningCut, sessionID, spendBase), nil
		}
		if early != nil {
			s.stampSpend(early.Metadata, sessionID, spendBase)
			return early, nil
		}
		if perr != nil {
			if errors.Is(perr, llm.ErrSpendCeiling) {
				return nil, s.endPlanAtSpendCeiling(sessionID, "", spendBase, perr)
			}
			return nil, perr
		}
		plan = p
	}

	// ROUTE-03 offline routing eval: return the generated plan as JSON and skip
	// DAG execution when plan_preview_only is set. An eval can then score the
	// planner's required_capabilities emission + deterministic L1 gating without
	// paying for full agentic execution (benchmark/eval-only path).
	if s.ExecCfg.Plan.PlanPreviewOnly {
		planJSON, mErr := json.Marshal(plan)
		if mErr != nil {
			return nil, fmt.Errorf("plan_preview_only: marshal plan: %w", mErr)
		}
		return &pb.Handoff{Payload: &pb.Object{Type: "plan_preview", Data: planJSON}}, nil
	}

	// Logging plan summary
	slog.Info("📜 STRATEGIC PLAN", "subject", plan.Subject, "steps", len(plan.Steps))

	initialCtx := make(map[string]string)
	for k, v := range in.GetMetadata() {
		initialCtx[k] = v
	}
	initialCtx["original_prompt"] = userInput

	// Resume seeds the checkpointed context and start position — but ONLY when the caller
	// explicitly asked for it, and only from the run's own plan.
	var startFromStep int
	if resumed != nil {
		for k, v := range resumed.Context {
			initialCtx[k] = v
		}
		startFromStep = resumed.StartFrom
	}

	// executionID scopes all neural traces produced by this Execute call — and, since
	// Phase 3, IS the run id: the same identifier the run is persisted under.
	executionID := domain.RunID(newPlanID())
	if resumed != nil {
		executionID = resumed.Run.ID
	}
	operator.AttachRunID(ctx, executionID) // contract 0102 CancelRun by run_id
	// ADR-0145 D1: the run's ledger provenance (ordering conversation, turn, owner)
	// rides the plan ctx to the step-output choke point; the executor reads its own
	// run's entries acting for that owner.
	var ledgerScope *domain.TagPredicate
	if s.ledgerOn() {
		prov := s.runProvenance(ctx, in.GetMetadata(), executionID, sessionID)
		ctx = domain.WithLedgerProvenance(ctx, prov)
		ledgerScope = runLedgerScope(prov.Owner)
	}
	// ADR-0145 D5: the checklist is stored in the run ledger; stepRun is the run id
	// the CURRENT stage's leases and ledger entries are bound to (a coverage stage
	// is a new plan with its own step indices, so it gets its own run id).
	stepRun := executionID
	if reqs != nil {
		s.recordChecklist(ctx, reqs)
		reqs.exec = executer.NewStepRequirements(reqs.checklist, reqs.coverage, s.stepActionsOf(&stepRun))
	}

	// The run's side-effecting tool calls are recorded from here until Execute returns,
	// and reported in its result (plan_actions.go).
	s.planActions.begin(executionID)
	defer s.planActions.end(executionID)

	// Persist the run (with its plan) so it can be resumed later. ADR-0012 §3 always
	// specified storing "the associated ExecutionPlan (for replay)"; without it a step
	// index has no steps to index into, which is why resume could never be made sound.
	if s.Runs != nil {
		run := domain.Run{
			ID:        executionID,
			SessionID: sessionID,
			PlanID:    string(executionID),
			Subject:   plan.Subject,
			Status:    domain.RunRunning,
			Plan:      plan,
			StartedAt: time.Now(),
		}
		if resumed != nil {
			run.StartedAt = resumed.Run.StartedAt
		}
		if serr := s.Runs.SaveRun(run); serr != nil {
			slog.Warn("run persist failed; this execution will not be resumable", "run", executionID, "err", serr)
		}
	}

	// Memory Ingestion tracking for DAG Lineage
	var ingestedIDsMu sync.Mutex
	var ingestedIDs []string

	// Confidence accumulator
	var confMu sync.Mutex
	var confValues []float64

	// Whom this run's steps act for: the run principal of its step leases, the
	// person ExecuteTool's per_user credentials belong to. Every step carries it
	// (domain.HandoffKeyActingFor) so a worker reading a task that names that
	// person knows the person IS the user its tools authenticate as
	// (T-20261107-delegated-steps-name-the-conversation-owner-in).
	actingForID, actingForName := s.actingForRun(ctx, sessionID, executionID)

	// Named results: the lease's deferred Complete stamps the gateway's overrun
	// verdict on the handoff this function hands back (see the defer below).
	stepFn := func(stepCtx context.Context, i int, handoff *domain.Handoff) (stepResp *domain.Handoff, stepErr error) {
		// The DAGExecutor may replan (ADR-0005) or fan-out expand (ADR-0078) its plan
		// mid-run, so the `plan` this closure captured is stale: a fan-out child's index
		// is past its end, and a replanned plan's index i is a DIFFERENT step. The
		// executor hands its CURRENT step on ctx (domain.WithPlanStep), and that wins,
		// so the step's routing contract (RequiredCapabilities, pin, budget) travels to
		// every fan-out child and replanned step. Before it did, every fan-out child
		// routed with no capability requirement — to whichever agent ranked first —
		// (T-20261106-planner-routes-a-file-move-step-to-an-agent). Without it (an
		// executor that does not stamp ctx), index the captured plan when in range, else
		// trust the handoff's query: an out-of-range index must never panic the kernel.
		var step domain.Step
		if cur, ok := domain.PlanStepFrom(stepCtx); ok {
			step = cur
		} else if i >= 0 && i < len(plan.Steps) {
			step = plan.Steps[i]
		} else {
			q := ""
			if handoff != nil && handoff.Payload != nil {
				q = string(handoff.Payload.Data)
			}
			step = domain.Step{Query: q}
		}
		prePwd, _ := os.Getwd()

		// ADR-0131 D3: once the session is at its spend ceiling no further step is
		// dispatched — the agent would only meet the same refusal at its first
		// generate, after dispatch and a seed recall it cannot use.
		if err := s.SpendMeter.AdmitSession(sessionID, string(domain.PurposeAgentStep), 0); err != nil {
			return nil, err
		}

		// Stamp task_id so AgentManager.CallAgent builds a consistent snapshot key.
		if handoff.Context == nil {
			handoff.Context = make(map[string]string)
		}
		handoff.Context["task_id"] = fmt.Sprintf("task-%d", i)

		// ADR-0023 Fix 1+3: inject session token and step index so cognitive agents
		// can call GenerateViaModelStream and log their position in the plan.
		handoff.Context["_step_index"] = fmt.Sprintf("%d", i)
		// Kernel-authored, always overwritten or removed: a caller's Execute
		// metadata (which reaches the plan context) cannot name whom a step acts for.
		domain.StampActingFor(handoff.Context, actingForID, actingForName)
		if s.LLMGateway != nil {
			// The step's model allocation is deliberately EMPTY here, and the gateway
			// resolves it: an empty Winner makes StreamChunks fall through to the
			// configured default (`SetDefaultModelID("llm:" + cfg.LLMProvider.Default)`,
			// app.go), which it logs. So configuration decides which model an agent
			// streams through.
			//
			// This previously hardcoded `llm:ollama:qwen3:8b` whenever an Ollama router
			// was present, on the stated reason that StreamChunks needed a non-empty
			// Winner. It does not — the default-model fallback below it has covered that
			// since it was added. The literal therefore did not fill a gap, it OVERRODE
			// the operator's configured default with a model named in Go, which is why
			// "which model runs my agents" was unanswerable from config on any
			// Ollama-equipped deployment.
			//
			// What is still missing is the per-STEP allocation (ADR-0018): the lease is
			// minted here, at dispatch, BEFORE selection picks a winner, so the
			// StepAllocation the Dispatcher computes (dispatch.go, DispatchResult) cannot
			// be bound to it yet. See the Known Gaps entry "Per-step model allocation
			// never reaches the lease" for the two candidate fixes.
			sa := domain.StepAllocation{}
			tokenID, _ := s.LLMGateway.Acquire(stepCtx, sa, StepLeaseTokenLimit, StepLeaseDuration)
			// Phase 0: record WHICH session/run/step this lease belongs to, so an agent
			// presenting it can be resolved back to its session server-side. Without
			// this the kernel has no way to answer "whose lease is this?" and has to
			// trust whatever the agent puts in a header.
			if r := s.leaseResolver(); r != nil {
				// AgentID is left empty HERE: the lease is acquired before selection
				// picks the winner, so the executing agent is not yet known. The
				// dispatcher binds it (BindLeaseAgent) immediately before each
				// CallAgent — winner, runner-up fallback, yield delegate and resume
				// — so the ADR-0139 D8 ownership check applies to this lease too.
				r.BindLease(tokenID, domain.LeaseBinding{
					SessionID: sessionID,
					RunID:     stepRun,
					StepIndex: i,
					// ADR-0145 D14: the request facts the tool executor's guards
					// need (nil unless execution.tools.request_guards).
					Guard: s.stepGuard(stepCtx, step, reqs, executionID),
					// ADR-0145 D14: how deep this run is in worker delegations.
					DelegationDepth: domain.DelegationDepthFrom(stepCtx),
					// The executor's plan generation (0 until a replan): the step's
					// ledger entries record under domain.LedgerRunID(run, generation).
					PlanGeneration: domain.PlanGenerationFrom(stepCtx),
				})
			}
			// Revoked with the ordering chat turn, even once Completed (turn_delegation.go).
			delegated.AddLease(tokenID)
			handoff.Context["_session_token_id"] = string(tokenID)
			defer func() {
				usage, cerr := s.LLMGateway.Complete(stepCtx, tokenID)
				// The gateway's Pass-1 cut is the server-authoritative budget overrun
				// (ADR-0018). Complete used to discard it; it now rides to the
				// DAGExecutor's TaskEvent on the handoff
				// (T-20261031-taskevent-budgetoverrun-is-written-by-no-kernel).
				if cerr == nil && usage.BudgetOverrun && stepResp != nil {
					if stepResp.Context == nil {
						stepResp.Context = make(map[string]string)
					}
					stepResp.Context[domain.HandoffKeyBudgetOverrun] = "true"
				}
				// ADR-0131 D2: the lease's provider usage (every stream and tool turn of
				// the step, plus the retrieval chain charged back to it) rides to the
				// step's TaskEvent the same way, replacing the executor's len/4 estimate.
				// Stamp clears any agent-echoed value first; a failed Complete clears too.
				if stepResp != nil {
					if cerr == nil && !usage.IsZero() && stepResp.Context == nil {
						stepResp.Context = make(map[string]string)
					}
					if cerr == nil {
						llm.StampStepUsage(stepResp.Context, usage)
					} else {
						llm.ClearStepUsage(stepResp.Context)
					}
				}
				// ADR-0046 D6 ephemerality: the grants a system skill conferred for THIS
				// run fall away with the lease — Complete runs the gateway's completion
				// hook, registered in NewServer, which calls ToolExecutor.EndRun
				// (T-20261031-skill-conferred-run-grants-are-never-cleared).
			}()
		}

		// healingOccurred is set true by innerFn when SelfHealer injects _heal_attempt,
		// signalling that at least one retry was consumed. It is passed to the Memory
		// Barrier, which since ADR-0049 D3 only logs.
		healingOccurred := false

		// runnerUps and winningAgentID are captured from dispatch so that the
		// fallback loop can use runner-ups when SelfHealer exhausts.
		var runnerUps []domain.ScoredCandidate
		winningAgentID := ""

		innerFn := harness.StepFunc(func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
			if h.Context["_heal_attempt"] != "" {
				healingOccurred = true
			}
			dispatchTask := &domain.DispatchTask{
				ID:          fmt.Sprintf("task-%d", i),
				Description: step.Query,
				Context:     fmt.Sprintf("Subject: %s", plan.Subject),
				// Deadline is left zero: the Dispatcher stamps the deadline the step
				// actually runs under (execution.plan.step_timeout_* under the plan
				// ceiling). A hardcoded `now + 20 s` here was read by nothing
				// (T-20261105-slow-model-reliability-hardcoded-20-s).
			}
			// ROUTE-03: thread the step's declared capabilities into
			// dispatch ONLY under the capability_contract arm, so the control
			// arm leaves RequiredCapabilities empty (byte-identical L1).
			if s.ExecCfg.Routing.CapabilityContract {
				dispatchTask.RequiredCapabilities = step.RequiredCapabilities
			}
			// Agent pin: carried unconditionally (the flag is read at the gate, so
			// one place decides whether a pin is honoured). Empty PreferredAgent is
			// the overwhelmingly common case and changes nothing downstream.
			dispatchTask.PreferredAgent = step.PreferredAgent
			dispatchTask.AgentPin = step.AgentPin
			// Effect-checked dispatch: carried unconditionally (it is a fail-closed
			// eligibility gate, not an A/B arm). nil stays nil — "declared nothing"
			// (a legacy plan, logged unverified) differs from an empty "needs none".
			dispatchTask.RequiredEffects = step.RequiredEffects
			// ADR-0100 D4: carry the step's budget + verification flag so dispatch
			// can pick its per-step policy.
			dispatchTask.MaxEnergy = step.MaxEnergy
			dispatchTask.CheckpointAfter = step.CheckpointAfter
			result, err := s.Dispatcher.Execute(ctx, dispatchTask, h)
			if err != nil {
				if result != nil {
					runnerUps = result.RunnerUps
				}
				if aid := h.Context["_winning_agent_id"]; aid != "" {
					winningAgentID = aid
				}
				return nil, err
			}
			winningAgentID = result.Handoff.FromAgent
			runnerUps = result.RunnerUps
			h.Context["_winning_confidence"] = fmt.Sprintf("%f", result.Confidence)
			return result.Handoff, nil
		})

		// Retry/heal only: workspace rollback between attempts (ADR-0005 D2b/D3)
		// was removed by owner decision 2026-10-31 — see harness.SelfHealer.
		healer := &harness.SelfHealer{StepIndex: i}
		// The step's yield sub-goal budget lives on the step, so every attempt the
		// healer makes draws on it (T-20261112-self-healer-retries-a-step-whose-yield-chain).
		resp, err := healer.Wrap(innerFn)(yield.WithStepBudget(stepCtx), handoff)
		if err != nil {
			var healErr *harness.HealingExhaustedError
			if errors.As(err, &healErr) {
				if s.MemoryAgent != nil {
					slog.Warn("🏥 Healing exhausted", "step", i,
						"attempts", healErr.AttemptCount,
						"loop", healErr.LoopDetected,
						"timed_out", healErr.TimedOut,
						"err", healErr.LastError)
					// Experiential memory removed: no negative-edge (failure) write-back.
				}

				// Inter-step fallback: try runner-up candidates when winner fails. A
				// failure that needs re-planning (a missing capability, a repeated
				// identical tool failure) goes straight to the re-planner: the step as
				// planned is what is wrong, not which agent ran it (owner 2026-11-12).
				if s.ExecCfg.Plan.FallbackEnabled && len(runnerUps) > 0 && healErr.Class != domain.StepFailureNeedsReplan {
					if fbResp, ok := s.runFallback(stepCtx, i, handoff, runnerUps, winningAgentID, healErr); ok {
						resp = fbResp
						err = nil
					}
				}
			}
			if err != nil {
				return nil, err
			}
		}

		// Confidence tracking
		confMu.Lock()
		winConf := 0.0 // default to 0.0 (unknown) until dispatch reports one
		if cStr := handoff.Context["_winning_confidence"]; cStr != "" {
			if c, err := strconv.ParseFloat(cStr, 64); err == nil {
				winConf = c
			}
		}
		confValues = append(confValues, winConf)
		confMu.Unlock()

		// Memory Barrier: forced when agent signals _kernel_sync, when the
		// environment mutated, or when SelfHealer consumed at least one retry.
		kernelSync := resp.Context["_kernel_sync"] == "true" || healingOccurred
		postPwd, _ := os.Getwd()
		envMutation := prePwd != postPwd

		ingestedIDsMu.Lock()
		links := make([]string, len(ingestedIDs))
		copy(links, ingestedIDs)
		ingestedIDsMu.Unlock()

		s.handleMemoryBarrier(stepCtx, i, resp, kernelSync, envMutation, links)

		return resp, nil
	}

	// The progress-aware plan ceiling (owner decision 2026-11-05, plan_watchdog.go):
	// the plan is cut when it goes idle for plan_idle_timeout_ms, or at the
	// plan_max_duration_ms safety cap — never merely for being long while it works.
	// Each cause names its key, so a step that dies because the PLAN was cut says so
	// (SelfHealer reports context.Cause) instead of reading as the step's own
	// failure (T-20261105-slow-model-reliability-hardcoded-20-s).
	planAct := newPlanActivity()
	maxDuration, idleTimeout := planLimits(s.ExecCfg.Plan)
	planProbe := s.planLivenessProbe(ctx, planAct, executionID, sessionID)
	// And it stops a reserve BEFORE its caller's deadline (a chat turn's delegation
	// carries one), so the answer below — what completed, and the ledger of what it
	// wrote — still reaches the caller (T-20261107-a-delegated-plan-stopped-by-its-caller-s).
	reservedCtx, stopCallerReserve := reserve.bound(ctx, errPlanCallerDeadline(reserve.reserve))
	defer stopCallerReserve()
	planCtx, stopPlanCeiling := startPlanCeiling(reservedCtx, maxDuration, idleTimeout, planProbe)
	defer stopPlanCeiling()
	// The same probe is the ordering chat turn's view of this plan's progress, so
	// the chat stall watchdog sees step dispatch, tool calls, approvals in flight and
	// model calls on the step leases (T-20261106-chat-stall-detection-cancels-a-turn-whose).
	delegated.Attach(executionID, planProbe)

	var replanner executer.ReplanHandler = inFlightReplanner{inner: s.replanHandler(planBudget), own: planAct}

	eventWriter := s.taskEventWriter()

	// newExecutor builds the DAG executor of one stage (the first plan, then each
	// ADR-0145 coverage stage) over the run id its steps are bound to.
	newExecutor := func(runID domain.RunID) *executer.DAGExecutor {
		executor := &executer.DAGExecutor{
			EventWriter:         eventWriter,
			EnqueueVerification: s.EnqueueVerification,
			// ADR-0049 §A2.2 / ADR-0095: the plan lifecycle (BeginExperience,
			// RecordPlanOutcome). Self-gates on RecordOutcomes
			// (execution.experience_records_enabled), default TRUE since the owner
			// decision of 2026-07-28. The raw step-result path (RecordExecution) that
			// used to sit behind this recorder was deleted 2026-11-05.
			MemoryRecorder:         s.MemoryAgent,
			Surprise:               s.SurpriseOracle, // ADR-0049 A2.3: may be nil ⇒ surprise unknown (-1)
			WorkspaceStage:         s.WorkspaceStage,
			ArtifactLister:         s.ArtifactMeta, // ADR-0034: surface prior-step artifacts (scope-filtered)
			Authz:                  s.Authz,        // ADR-0085: artifact discovery filter (may be nil)
			LLMGateway:             s.LLMGateway,
			Observer:               s.TelemetryObserver,
			ContentStore:           s.planContentStore(),
			StepCache:              s.StepCache,
			UseGlobalWorkspace:     s.ExecCfg.Workspace.UseGlobalWorkspace,
			ContextRefSnippetChars: s.ExecCfg.Plan.ContextRefSnippetChars,
			// The one bound on upstream results handed to a dependent step (agent or
			// thought), domain.StepDependency: sized from the receiving worker's prompt
			// budget (execution.content, with execution.lane.context's override:
			// the same effective budget the worker is told, lane_context.go).
			DependencyFit: dependencyFit(s.ExecCfg.EffectiveContent()),
			// A thought step is an in-kernel model call on no step lease: in flight
			// for the plan idle watchdog while it runs.
			ThoughtFn: inFlightStepFunc(planAct, executer.StepFunc(s.thoughtFn(plan))),
			// No mid-plan checkpoint gate: the LLM checkpoint validator and the executor
			// gate that consumed it were deleted (ADR-0139 D10 / owner Q5). It failed open
			// and trusted the agent's self-report; verification is the async
			// VerificationWorker's job.
			ReplanHandler:          replanner,
			MaxReplanAttempts:      s.ExecCfg.Plan.MaxReplanAttempts,
			MaxFanOutWidth:         s.ExecCfg.Plan.MaxFanOutWidth,
			FanOutConcurrency:      s.ExecCfg.Plan.FanOutConcurrency, // fan-out children in parallel, dedicated instances (T-20261107)
			Calendar:               s.Calendar,
			MaxPlanCost:            s.ExecCfg.Plan.MaxPlanCost,
			DefaultInputCostPer1M:  s.Manager.DefaultInputCostPer1M,
			DefaultOutputCostPer1M: s.Manager.DefaultOutputCostPer1M,
			CurrentSessionID:       sessionID,
			CurrentRunID:           runID,
			// Close the task when its plan finishes. Nothing else did: `completed`
			// was only ever set by the operator's explicit command, so finished work
			// stayed open indefinitely.
			SessionCloser:     s.SessionMgr,
			CheckpointStore:   s.executorCheckpointStore(),
			StepCachePolicies: s.ExecCfg.Plan.StepCachePolicies,
			EventBus:          s.EventBus, // ADR-0047 0047-17: PlanStateChanged → operator feed
			// ADR-0145 D9: only read-only steps are cached, for this TTL (step_cache_wiring.go).
			EffectVocabulary:    stepCacheVocabulary(s.ExecCfg.Capability),
			StepCacheDefaultTTL: stepCacheDefaultTTL(s.ExecCfg.Plan),
			RunTracer:           s.RunTracer, // ADR-0145 D10: step-output spans
		}

		if s.ledgerOn() {
			// ADR-0145 D1: the step-output choke point and the dependants' read path.
			executor.Ledger = s.Ledger.Writer
			executor.LedgerReader = s.Ledger.Reader
			executor.LedgerReadScope = ledgerScope
		}
		if reqs != nil {
			// ADR-0145 D5/D6: item keys, envelope validation, code-decided coverage.
			executor.Requirements = reqs.exec
			// No mid-stage replan under the checklist: replan_cap emits no answers,
			// inputs or for_each, so its steps would answer nothing. A failed step
			// ends the stage; the next coverage stage plans the open items
			// (runStages; T-20261108-replanner-replan-cap-does-not-emit-answers).
			executor.ReplanHandler = nil
			executor.MaxReplanAttempts = 0
		}
		return executor
	}
	executor := newExecutor(executionID)

	// ADR-0047 0047-18: register this live execution's controls so the operator
	// PauseSession/ResumeSession commands can steer it; deregister on completion.
	if s.ControlHub != nil && sessionID != "" {
		s.ControlHub.Register(sessionID, executor)
		defer s.ControlHub.Deregister(sessionID)
	}

	masterCtx, err := executor.ExecuteFrom(planCtx, plan, initialCtx,
		activityStepFunc(planAct, executer.StepFunc(stepFn)), startFromStep)
	// ADR-0139 D13: the run's outcome credits the skills its steps loaded. Only a
	// run that finished every step counts as a success (a partial plan does not).
	s.recordSkillRunOutcome(ctx, executionID, err == nil)
	// Contract 0102: an operator CancelRun ended the plan. Terminal — recorded as
	// cancelled (not resumable) and answered as Canceled naming who and why;
	// nothing the steps already dispatched is rolled back.
	if rc, cancelled := operator.RunCancelled(ctx); cancelled {
		s.markRunCancelled(executionID, rc)
		return nil, status.Error(codes.Canceled, rc.Error())
	}
	// The plan ceiling's verdict, when it is what ended the plan: the idle or the
	// max-duration cause (both wrap context.DeadlineExceeded). Nil for a plan that
	// finished, failed on its own, or was abandoned by its caller.
	ceilingCause := planCeilingCause(ctx, planCtx)

	// ADR-0131 D3: a plan that hit the session ceiling ENDS AS A FAILURE, whatever
	// shape the executor's outcome took. A refused call can surface as a step error,
	// a partial plan, a failed replan — or not at all, when an agent swallows its
	// generate error and reports text. The meter's verdict is the authority.
	if errors.Is(err, llm.ErrSpendCeiling) || s.SpendMeter.Session(sessionID).Exhausted {
		cause := err
		if !errors.Is(cause, llm.ErrSpendCeiling) {
			cause = llm.ErrSpendCeiling
		}
		return nil, s.endPlanAtSpendCeiling(sessionID, executionID, spendBase, cause)
	}

	// ADR-0145 D5.5: under the checklist the first stage stopping part-way is a
	// stage result like any other: what it settled stands and the coverage stages
	// below plan the rest (staged_planner.go stageStoppedPartway). A plan ceiling
	// and an effect-check refusal keep their own endings.
	var firstStageStopped error
	if reqs != nil && err != nil && ceilingCause == nil {
		if m, ok := stageStoppedPartway(err); ok {
			slog.Info("ADR-0145 D5.5: the first stage stopped part-way; its results stand", "run", executionID, "err", err)
			masterCtx, firstStageStopped, err = m, err, nil
		}
	}

	if err != nil {
		var partialErr *executer.PartialPlanError
		// Effect-checked dispatch: a step the effect check refused, after its one
		// replan, ends as a QUESTION for the user — which agent should get the tool,
		// or how to proceed — never as a dispatch to an arbitrary agent.
		if errors.As(err, &partialErr) && ceilingCause == nil {
			var noEffect *domain.NoEffectMatchError
			if errors.As(partialErr.LastError, &noEffect) {
				s.stampSpend(partialErr.Context, sessionID, spendBase)
				return effectRefusalClarification(in.Id, noEffect, partialErr.Context)
			}
		}
		if errors.As(err, &partialErr) {
			slog.Warn("⚠️ Partial plan failure", "failed_step", partialErr.FailedStep, "context_entries", len(partialErr.Context))
			partialErr.Context["_partial_plan"] = "true"
			if ceilingCause != nil {
				// Reported as a TIMEOUT, with the cause naming which limit fired.
				partialErr.Context[PlanTimedOutKey] = "true"
				partialErr.Context[PlanTimeoutCauseKey] = ceilingCause.Error()
				slog.Warn("⏱️ plan cut by its ceiling", "run", executionID, "cause", ceilingCause)
			}
			s.stampSpend(partialErr.Context, sessionID, spendBase)
			// The payload says what DID happen before the failure — the completed
			// steps' results and the side-effecting calls — not only the error: a
			// caller told just "failed at step 3" re-orders the writes of steps 0-2.
			actions := s.planActions.snapshot(executionID)
			if j := planActionsJSON(actions); j != "" {
				partialErr.Context[PlanActionsKey] = j
			}
			payload := partialPlanPayload(partialFailure(partialErr, ceilingCause), partialErr.Report, actions)
			if reqs != nil {
				// ADR-0145 D5: the plan stopped; every open item closes as a gap.
				reqs.coverage.ObserveActions(actions, s.effectsOfTool())
				reqs.coverage.CloseGaps("the plan stopped before it finished")
				payload += "\n\n" + coverageText(reqs)
			}
			// ADR-0145 D8: the structured result rides the metadata, the text stays the payload.
			s.stampExecuteResult(partialErr.Context, payload, executionID, sessionID, actions, partialErr, ceilingCause)
			if reqs != nil {
				s.attachCoverage(ctx, partialErr.Context, reqs)
			}
			return &pb.Handoff{
				Id:        in.Id,
				FromAgent: "orchestrator",
				ToAgent:   "user",
				Payload:   &pb.Object{Data: []byte(payload)},
				Metadata:  partialErr.Context,
			}, nil
		}
		if ceilingCause != nil {
			// The executor returns the bare ctx error when the ceiling fires between
			// steps — context.Canceled for the idle cut, which would reach the caller
			// as CANCELLED. It is a timeout, and the caller is told which one.
			slog.Warn("⏱️ plan cut by its ceiling", "run", executionID, "cause", ceilingCause)
			return nil, status.Error(codes.DeadlineExceeded, ceilingCause.Error())
		}
		return nil, err
	}

	// Experiential memory removed: no procedural-memory (Hippocampus) plan write-back.

	// ADR-0145 D5.5 / D11: code decides whether the request is done, stage by
	// stage (coverage_stages.go runStages): settled, stalled, the runaway cap, or —
	// under the staged planner — the planner having nothing more, or a question
	// for the person.
	var allActions []domain.PlanAction
	var askBack *domain.InputRequired
	if reqs != nil {
		out := s.runStages(stageLoop{reqs: reqs, executionID: executionID, stepRun: &stepRun, ctx: ctx, planCtx: planCtx,
			userInput: userInput, initialCtx: initialCtx, newExecutor: newExecutor,
			stepFn: activityStepFunc(planAct, executer.StepFunc(stepFn)), staged: staged,
			stopped: firstStageStopped}, masterCtx)
		allActions, askBack = out.actions, out.askBack
		masterCtx[finalResultKey] = strings.Join(out.reports, "\n\n") + "\n\n" + coverageText(reqs)
	}

	// The executor's report of EVERY final step (not only the last in topological
	// order), followed by the kernel's ledger of the side-effecting calls the plan
	// made (T-20261107-plan-result-is-only-the-last-topo-step-a-plan).
	actions := s.planActions.snapshot(executionID)
	if reqs != nil {
		actions = allActions // every stage's calls (the first stage's included)
	}
	finalResult := planResultPayload(masterCtx[finalResultKey], actions)
	delete(masterCtx, finalResultKey)
	if j := planActionsJSON(actions); j != "" {
		masterCtx[PlanActionsKey] = j
	}
	if sessionID != "" {
		masterCtx["_session_id"] = string(sessionID)
	}
	s.stampSpend(masterCtx, sessionID, spendBase)
	// ADR-0145 D8: the structured result rides the metadata, the text stays the payload.
	s.stampExecuteResult(masterCtx, finalResult, executionID, sessionID, actions, nil, nil)
	if reqs != nil {
		s.attachCoverage(ctx, masterCtx, reqs)
	}
	// ADR-0145 D5a: a later stage asked the person (staged_planner.go).
	attachInputRequired(masterCtx, askBack)

	return &pb.Handoff{
		Id:        in.Id,
		FromAgent: "orchestrator",
		ToAgent:   "user",
		Payload:   &pb.Object{Data: []byte(finalResult)},
		Metadata:  masterCtx,
	}, nil
}
