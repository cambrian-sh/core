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
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/memorynotes"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
	"github.com/cambrian-sh/core/internal/scheduledtask"
	"github.com/cambrian-sh/core/internal/stepoutput"
	"github.com/cambrian-sh/core/internal/substrate/operator"

	"github.com/cambrian-sh/core/internal/substrate/harness"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/grpc"
)

// storeNeuralTrace persists an agent thought trace asynchronously to the vector
// store. No-op when vs is nil or trace is empty.
func storeNeuralTrace(ctx context.Context, vs domain.VectorStore, trace, traceID, planID string, stepIndex, healAttempt int, agentID string) {
	if vs == nil || trace == "" {
		return
	}
	go func() {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		doc := &domain.Document{
			ID:           fmt.Sprintf("trace-%s", traceID),
			DocumentType: domain.DocTypeNeuralTrace,
			Text:         trace,
			Metadata: map[string]interface{}{
				"trace_id":     traceID,
				"plan_id":      planID,
				"step_index":   stepIndex,
				"agent_id":     agentID,
				"heal_attempt": healAttempt,
			},
		}
		// BRAIN-01: stamp the SESSION that produced this trace.
		//
		// A neural trace is the archetypal "another task's evidence" — it is one
		// run's reasoning, and without this it was retrievable by every other run
		// forever. `trace_id` looks session-shaped but is a plan or lease id, so
		// nothing downstream could tell whose run a trace belonged to: session
		// isolation could not fence it and `cross_session_retrieval_rate` could not
		// count it.
		//
		// Read from the CONTEXT, never from a parameter: the session is seeded
		// server-side from the authenticated caller, and a value passed in here
		// would be one a caller could choose.
		if sid, ok := domain.SessionIDFromContext(saveCtx); ok && sid != "" {
			doc.Metadata[domain.MetaSessionID] = string(sid)
		}
		if err := vs.Save(saveCtx, doc); err != nil {
			slog.Error("🧠 NEURAL AMNESIA: Trace storage failed", "step", stepIndex, "err", err)
		}
	}()
}

// Planner is the interface Server depends on for plan generation.
type Planner interface {
	GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error)
	Generate(ctx context.Context, prompt string) (string, error)
}

// PlanValidationError carries a structured Pain Signal from validatePlan.
type PlanValidationError struct {
	Signal string
}

func (e *PlanValidationError) Error() string { return e.Signal }

// Server, Cambrian gRPC sunucusunu temsil eder.

type Server struct {
	// Progress reports what the kernel is doing on behalf of a chat turn, so a user
	// waiting on a slow request sees movement rather than one static line (ADR-0098).
	// nil ⇒ nothing is listening, which is the OSS default.
	Progress domain.ProgressSink
	// Activity reports WHAT an agent is doing inside a call — which tool, with
	// which arguments — to whoever minted its session token.
	//
	// Distinct from Progress on purpose: Progress is a supersedable snapshot with
	// a closed phase vocabulary for customer-facing surfaces, and deliberately
	// names no tool. This is the append-only operator-facing counterpart, for a
	// console inspecting a run. nil ⇒ nothing is emitted.
	Activity domain.AgentActivityObserver

	pb.UnimplementedOrchestratorServer
	// Router is the universal input classifier (ADR-0031). When nil, Execute
	// falls back to the legacy PLAN-only path for backward compatibility.
	Router      domain.InputRouter
	Planner     Planner
	Manager     *agentmgr.AgentManager
	Dispatcher  domain.StepDispatcher
	MemoryAgent domain.MemoryAgent
	// SurpriseOracle scores a step outcome against merit (ADR-0049 A2.3). Nil ⇒ the
	// episode records surprise as unknown rather than as zero.
	SurpriseOracle      domain.SurpriseOracle
	ExecCfg             config.ExecutionConfig
	EnqueueVerification executer.EnqueueVerification
	Watcher             *supwatcher.Watcher

	VectorStore    domain.VectorStore
	MemorySearcher domain.MemorySearcher
	Hippocampus    domain.ProceduralMemory
	Provider       domain.LLMProvider // ADR-0042: agent-step model provisioning (health-guarded)
	SessionMgr     *session.SessionManager
	// ConvSurfaces resolves which ENTRY POINT a conversation arrived on, for turns
	// that have no task session. Without it a chat turn from an ingress is
	// authorised as an ordinary agent call and the ingress's policy never applies.
	ConvSurfaces authz.ConversationSurfaceReader
	// TaskOwners resolves a task session to its beneficiary owner principal
	// (ADR-0126 phase 4). Read by withCallerSession so agents attending a
	// submitted task see the beneficiary's contributed worker fleet in their
	// menus (ADR-0127 D4) and the dispatch-side ownership check holds. nil ⇒ no
	// task carries a beneficiary and contributed resolution stays fail-closed.
	TaskOwners TaskBeneficiarySource
	// Runs persists plan executions so a resume can replay against the run's OWN plan.
	Runs domain.RunStore
	// Checkpoints persists mid-run state. Wired EXPLICITLY at the composition root rather
	// than discovered by a runtime type assertion on the registry — that assertion silently
	// returned nil for the whole life of the feature (the registry held the store in a named
	// field, so its methods were never promoted), and checkpointing simply never happened.
	// An explicit field turns "not wired" into something you can see at the wiring site.
	Checkpoints       executer.CheckpointStore
	WorkspaceStage    domain.WorkspaceStage    // ADR-0016: may be nil
	LLMGateway        LLMGateway               // ADR-0018: may be nil; wired by kernel provider
	TelemetryObserver domain.TelemetryObserver // ADR-0019: may be nil
	ContentStore      domain.ContentStore      // ADR-0022 Phase 1: may be nil; nil disables CAS
	StepCache         domain.StepCache         // ADR-0026: may be nil; nil disables step-level memoization

	// RunTracer records step outputs and kernel-executed tool calls as OTel GenAI
	// spans (ADR-0145 D10, run_trace.go). nil ⇒ off (execution.trace.enabled).
	RunTracer domain.RunTracer

	// TokenRecorder taps token usage for the hourly spend series (contract 0075).
	// nil ⇒ no tap and the resolved event writer is used unchanged, which is what
	// a build with no operator console wants.
	TokenRecorder TokenRecorder

	// AgentCallLogger records agent-initiated LLM calls (GenerateViaModelStream) to an
	// external observability backend. nil = disabled (no-op). OBSERVABILITYREQ REQ1.
	AgentCallLogger AgentCallLogger

	// ThoughtLLM serves in-kernel thought steps (thoughtFn) on the C1 request path:
	// the thought-purpose generator (domain.PurposeThought; routed like the planner
	// unless a `thought` role is configured), wired at the composition root. nil =
	// the Planner's legacy Generate, which sends the request flattened to one
	// prompt (and is labelled planner — only a bare test server takes that path).
	ThoughtLLM domain.Generator

	// GenWrapper decorates a raw generator with cross-cutting concerns (Langfuse
	// tracing) so thought/synthesis steps generated in-kernel are
	// observable, not just the Planner's own generator. nil = identity.
	GenWrapper func(domain.Generator) domain.Generator

	// EFETrafficPercent is the session-scoped A/B split for "auto" mode (0..100).
	EFETrafficPercent int

	// SignalReceiver dispatches incoming signals to the condition/action pipeline.
	// OSS build: the Watcher (LTM enrichment + one Planner call; a non-empty plan
	// is queued for operator approval and runs only if approved).
	// Premium build: ReactiveEngine (condition evaluation + action execution). ADR-0032.
	// nil → signals are logged and discarded.
	SignalReceiver domain.SignalReceiver

	// IngressInbound routes a signal from a REGISTERED ingress into the chat lane
	// instead of the signal pipeline (ADR-0090). nil ⇒ no ingress is registered and
	// every signal takes the ordinary path, which is the OSS default.
	//
	// It is checked BEFORE the Watcher because an ingress message is a
	// conversational turn, and ADR-0080 D4 exists because turns that reach the
	// planner get decomposed into steps like "ask the customer for their booking
	// reference" — unexecutable, and the failure was emitted as spoken dialogue.
	IngressInbound IngressAccepter

	// WatchHandler is the premium reactive engine's WatchConfig store. Injected by
	// the premium binary via the app.Options reactive hook; nil in OSS. It backs the
	// OperatorConsole watch commands (app.go hands it to the operator service); the
	// Orchestrator watch-CRUD RPCs that once served it directly were removed in
	// contract 0099. ADR-0032 / ADR-0057.
	WatchHandler domain.WatchConfigHandler

	// REQ-SDK-007c: artifact storage, gated by the decision point. Both nil → the
	// artifact RPCs return Unimplemented.
	ArtifactBytes ArtifactByteStore // CAS byte store (ArtifactVault)
	ArtifactMeta  ArtifactMetaStore // metadata + tags persistence

	// Ledger is the ADR-0145 D1 run ledger (kernel.LedgerStack), nil when
	// execution.ledger.enabled is off. On: the DAG executor records every step
	// output to it and reads dependants' results from it, the plan executor gets no
	// content store (step results are no longer plan-scoped scratch there), and
	// GetContextNode resolves led_… ids through its enforced reader (ledger.go).
	Ledger *LedgerAccess
	// Checklist derives a delegated request's requirement checklist (ADR-0145 D5,
	// requirements.Deriver). Used only when execution.plan.checklist_enabled is on
	// and the ledger is wired (checklistOn); nil ⇒ the legacy single-plan path.
	Checklist ChecklistDeriver

	// Authz is the access-control decision point (ADR-0085). nil ⇒ the OSS
	// allow-all default: the server still ASKS at every enforcement point, and in
	// an unscoped deployment the answer is always yes.
	Authz domain.Authorizer

	// MemoryNotes is the ADR-0139 D14 notes service (GetMemoryNotes /
	// ProposeMemoryNote, memory_notes_rpc.go). nil ⇒ both RPCs answer
	// Unimplemented. MemoryNotesPrincipal resolves a lease binding to the run
	// principal; nil ⇒ no principal, so runs see only org notes (fail-closed).
	MemoryNotes          *memorynotes.Service
	MemoryNotesPrincipal MemoryNotesPrincipalFunc
	// RunPrincipal resolves a lease binding to the person the run is FOR
	// (ADR-0140 D4) — the same lease → principal resolution as
	// MemoryNotesPrincipal (conversation owner → task beneficiary → default),
	// wired to the same function. ExecuteTool stamps it on ctx
	// (domain.WithRunPrincipal) so a per_user integration uses that person's
	// own credential. nil ⇒ no run principal: per_user tools fail closed.
	RunPrincipal MemoryNotesPrincipalFunc
	// PrincipalNames is the ADR-0142 registry, read for the display name of the
	// person a plan's steps act for (domain.HandoffKeyActingForName, acting_for.go).
	// nil ⇒ steps carry the principal id only.
	PrincipalNames PrincipalNameSource
	// Calendar is the kernel's one "today" (domain.Calendar, built from
	// organisation.timezone): the plan executor stamps it on every step, a replan and
	// a thought step are dated with it. The planner holds the same instance. nil ⇒
	// the wall clock in UTC.
	Calendar *domain.Calendar

	// ScheduledTasks is the gate for principal-owned recurring tasks
	// (ProposeScheduledTask / ListScheduledTasks / ManageScheduledTask,
	// scheduled_tasks_rpc.go). The owner is always RunPrincipal of the presented
	// lease. nil ⇒ the three RPCs answer Unimplemented (no premium schedule store,
	// or execution.schedules.disabled).
	ScheduledTasks *scheduledtask.Service

	// Knowledge is the knowledge proposal bridge behind ProposeKnowledge
	// (knowledge_propose_rpc.go; knowledgecommit.Service). nil ⇒ the RPC answers
	// Unimplemented.
	Knowledge KnowledgeProposer

	// OntologyDrafts is the ontology draft bridge behind ProposeOntologyChange
	// (ontology_draft_rpc.go; ontology.Drafts). nil ⇒ the RPC answers
	// Unimplemented.
	OntologyDrafts OntologyDraftProposer

	// ConversationOwner resolves a lease binding to the owner of the conversation
	// it was issued under (on the lease, or on its task session); "" when the work
	// is not for a conversation. withCallerSession and QueryMemory stamp it on ctx
	// (domain.WithConversationOwner) so the decision point narrows the agent's
	// reads by the owner's entitlements. Unlike RunPrincipal it never falls back
	// to a beneficiary or a default principal. nil ⇒ nothing is stamped.
	ConversationOwner MemoryNotesPrincipalFunc
	// PrivateByDefault (ADR-0142 D12, config privacy.private_by_default, on by
	// default): when the conversation owner is stamped, the context is also
	// MARKED so every write chokepoint owner-scopes what the agent writes to
	// that person (domain.WithPrivateWrites / StampPrivateOwner).
	PrivateByDefault bool

	// IngestionProcessor is the ONLY way memory is written (ADR-0060 D8/D9): the
	// body goes through the chunker registry, a source-doc entity is minted, and
	// each chunk is ingested with chunk_relations. Satisfied by
	// *memory.IngestionManager.
	//
	// nil → IngestMemory FAILS. There is deliberately no raw-store-write fallback;
	// the one that existed produced an un-chunked row with different metadata keys,
	// invisible to ListDocuments, and could not fire anyway.
	IngestionProcessor IngestionProcessor

	// ADR-0039: kernel-owned tool registry + executor. nil → ExecuteTool returns
	// Unimplemented (default: no tools).
	ToolExecutor *domain.ToolExecutor
	// ApprovalHub backs the operator-plane WatchApprovals / SubmitApprovalDecision
	// RPCs (ADR-0039 D10). nil → those RPCs return Unimplemented.
	ApprovalHub domain.ApprovalHub

	// ADR-0047: the operator-feed EventBus (PlanStateChanged is published from the
	// executor) and the shared ExecutionControlHub (live executions register here
	// so operator PauseSession/ResumeSession can steer them). Both may be nil.
	EventBus   domain.EventBus
	ControlHub *operator.ExecutionControlHub

	// Delegations ties a plan to the chat turn that ordered it (turn_delegation.go):
	// the turn's end cancels the plan and revokes its leases, and the turn's stall
	// watchdog reads the plan's progress. nil ⇒ no chat lane; plans are unaffected.
	Delegations *TurnDelegations

	// StepOutput is the live step-output stream (ADR-0139 amendment 2026-11-06,
	// superseding ADR-0047 D12's token tier): gateway deltas from both model lanes
	// and agent-pushed output (EmitStepOutput) reach the operator feed and MCP
	// progress through it. nil ⇒ no live output, EmitStepOutput is Unimplemented.
	StepOutput *stepoutput.Stream

	// LLMExchangeSink, when set, receives the full prompt+completion of each managed-proxy
	// agent generation (ADR-0079) for the operator feed's live-only exchange lane — the
	// same tap that feeds Langfuse, forked to a benchmark-observable event. Best-effort,
	// fire-and-forget; nil unless execution.capture_llm_exchanges is on.
	LLMExchangeSink func(sessionID, agentID, modelID string, stepIndex int, prompt, completion string)

	// ADR-0046: the system-skill plane backing ListSkills. SkillRegistry holds the
	// discovered SKILL.md skills; SkillRetriever ranks them by relevance within the
	// principal's predicate, which Authz resolves. Both nil → ListSkills returns an
	// empty menu (agents simply see no system skills).
	SkillRegistry  domain.SkillRegistry
	SkillRetriever domain.SkillRetriever
	// SkillLifecycle is the ADR-0139 D13 lifecycle (skills_lifecycle_rpc.go):
	// ProposeSkill, use_skill usage and run-outcome telemetry. nil → ProposeSkill
	// is Unimplemented and no usage is recorded.
	SkillLifecycle SkillLifecycle
	// SkillAccess bounds a profile agent's skills to its profile's list plus its
	// operator-approved grants and runs use_skill access requests
	// (skills_rpc.go; decisions/skill-access-grants.md). nil = no agent is bounded.
	SkillAccess SkillAccess
	// Compositions holds the ADR-0145 D12 worker composition bound to each composed
	// step lease (execution.workers.composed). ListTools serves the composition as
	// the menu, ListSkills serves nothing outside its skill index and opens no access
	// request for it, and the lease's completion forgets it. nil = no composed workers.
	Compositions *domain.ComposedLeases
	// Subtasks is the ADR-0150 registry of subtask parents and child leases
	// (domain/subtasks.go): the server dispatches subtask workers on child leases
	// (subtasks.go), refuses a child lease's delegation to the planner, and a
	// lease's completion forgets it. nil = no subtasks.
	Subtasks *domain.Subtasks
	// ComposedRuntimeID is the composed runtime's agent id; "" when composed
	// workers are off. That agent holds nothing of its own: without a composed
	// lease it gets no tool, no menu and no skill (composed_workers.go).
	ComposedRuntimeID string
	// RequestComposer composes the single-agent lane's worker for a whole request
	// when execution.lane.single_agent.agent_id is empty (ADR-0150 D1;
	// compose.Composer.ComposeRequest). nil (composed workers off) ⇒ an empty
	// agent_id is refused, naming both keys (single_agent_lane.go).
	RequestComposer RequestComposer
	// ToolsOutsideStep lists the tools a composition could hold for the act-for
	// person but this composition does not (compose.Composer.OutsideStep): what a
	// composed worker's find_tools may report, never load (ListTools'
	// x-tool-outside-step response header). nil = nothing is reported.
	ToolsOutsideStep func(ctx context.Context, comp domain.WorkerComposition) []domain.SystemTool
	// WorkerDelegations tracks the step leases whose worker is waiting on its own
	// delegation to the planner (ADR-0145 D14), shared with the dispatcher so such
	// a step is neither timed out nor re-dispatched while it waits
	// (T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent). nil: not tracked.
	WorkerDelegations *domain.ActiveDelegations

	// Embedder backs the Embed RPC (ADR-0041) used by an agent's Local Recurrent
	// Workspace for relevance ranking. nil → Embed returns Unimplemented.
	Embedder domain.Embedder

	// SpendMeter is the ADR-0131 LLM spend ledger (the same object the Provider and
	// the gateway meter through). Execute reads it to end a plan visibly at the
	// session ceiling and to report the plan's spend. nil → no ceiling, no report.
	SpendMeter *llm.SpendMeter

	// planActions is the per-run ledger of side-effecting tool calls (plan_actions.go),
	// recorded at ExecuteTool and reported in Execute's result.
	planActions planActionLedger
}

// AgentCallLogger observes LLM calls made by cognitive agents through the
// Substrate streaming proxy. Implemented by the Langfuse logger shim.
type AgentCallLogger interface {
	Log(ctx context.Context, subsystem, prompt, completion, model, agentID string, stepIndex int)
}

// IngressAccepter handles a message from a registered ingress. Implemented by
// internal/ingress.InboundService; returns ErrNotAnIngress when the sender is an
// ordinary agent, which is the signal to fall through.
type IngressAccepter interface {
	Accept(ctx context.Context, m ingress.InboundMessage) error
}

// SyncProcessor extends domain.SignalReceiver with synchronous request/response
// semantics used by the CHAT routing path. The premium ReactiveEngine implements
// this interface; the OSS NoOpSignalReceiver does not. ADR-0032.
type SyncProcessor interface {
	domain.SignalReceiver
	ProcessSync(ctx context.Context, signal domain.Signal) (*domain.Handoff, error)
}

// WatchConfigHandler moved to domain (domain.WatchConfigHandler) — ADR-0057, so the
// premium reactive hook can name it across the module boundary.

// NewServer assembles the gRPC server from wired subsystems.
func NewServer(
	planner Planner,
	manager *agentmgr.AgentManager,
	memoryAgent domain.MemoryAgent,
	execCfg config.ExecutionConfig,
	vectorStore domain.VectorStore,
	memorySearcher domain.MemorySearcher,
	hippocampus domain.ProceduralMemory,
	enqVerification executer.EnqueueVerification,
	auctioneer domain.StepDispatcher,
	watcher *supwatcher.Watcher,
	sessionMgr *session.SessionManager,
	workspaceStage domain.WorkspaceStage,
	llmGateway LLMGateway,
	observer domain.TelemetryObserver,
	contentStore domain.ContentStore,
) *Server {
	s := &Server{
		Planner:             planner,
		Manager:             manager,
		Dispatcher:          auctioneer,
		MemoryAgent:         memoryAgent,
		ExecCfg:             execCfg,
		VectorStore:         vectorStore,
		MemorySearcher:      memorySearcher,
		Hippocampus:         hippocampus,
		EnqueueVerification: enqVerification,
		Watcher:             watcher,
		SessionMgr:          sessionMgr,
		WorkspaceStage:      workspaceStage,
		LLMGateway:          llmGateway,
		TelemetryObserver:   observer,
		ContentStore:        contentStore,
	}
	// ADR-0046 D6 ephemerality, in ONE place: every lease the gateway completes —
	// plan steps, the bypass arm, the retrieval dispatcher, and any lease site
	// added later — clears the run-scoped skill grants keyed by it. The executor
	// is read at call time because the composition root attaches it after
	// construction (T-20261101-retrieval-dispatch-lease-retrieval-dispatch-go).
	if h, ok := llmGateway.(interface{ SetCompleteHook(func(domain.LeaseID)) }); ok {
		h.SetCompleteHook(func(lease domain.LeaseID) {
			s.ToolExecutor.EndRun(string(lease))
			// The step-output stream's per-lease state (seq, caps, dedupe) ends
			// with the lease.
			s.StepOutput.EndLease(lease)
			// ADR-0145 D12: a composed step's bounds end with its lease.
			s.Compositions.Forget(lease)
			// ADR-0150: a subtask's child lease (or a parent's) ends with it.
			s.Subtasks.Forget(lease)
			s.Subtasks.ForgetParent(lease)
		})
	}
	return s
}

// validatePlan checks the plan's DAG structure (ADR-0003 D4). It does NOT check the
// capability vocabulary: ADR-0006 D2 removed that check from here on purpose, and the
// planner drops invented required_capabilities itself (internal/awareness/planner.go).
func validatePlan(plan *domain.ExecutionPlan) error {
	if _, err := executer.TopologicalSort(plan.Steps); err != nil {
		var cycleErr *executer.CyclicPlanError
		if errors.As(err, &cycleErr) {
			return &PlanValidationError{Signal: "The plan contains a dependency cycle: " + cycleErr.Description}
		}
		return &PlanValidationError{Signal: fmt.Sprintf("Invalid DAG structure: %v", err)}
	}
	return nil
}

// TokenRecorder decorates a task-event writer so token usage lands in the
// hourly series on its way past. Declared here rather than imported so the
// network layer keeps no dependency on the telemetry package.
type TokenRecorder interface {
	Wrap(inner domain.TaskEventWriter) domain.TaskEventReadWriter
}

func planWithValidation(ctx context.Context, planner Planner, userInput string) (*domain.ExecutionPlan, error) {
	plan, err := planner.GetExecutionPlan(ctx, userInput)
	if err != nil {
		return nil, err
	}

	if err := validatePlan(plan); err == nil {
		return plan, nil
	} else {
		var valErr *PlanValidationError
		if !errors.As(err, &valErr) {
			return nil, fmt.Errorf("plan validation: %w", err)
		}
		slog.Warn("plan validation failed, retrying planner", "signal", valErr.Signal)

		retryInput := fmt.Sprintf("%s\n\nPREVIOUS PLAN ERROR: %s", userInput, valErr.Signal)
		plan, err = planner.GetExecutionPlan(ctx, retryInput)
		if err != nil {
			return nil, err
		}

		if err := validatePlan(plan); err != nil {
			return nil, fmt.Errorf("planner produced an invalid plan after retry: %w", err)
		}
		return plan, nil
	}
}

// planningOutOfTime is Execute's answer when the caller-deadline reserve instant
// passed while the request was still being routed or planned
// (T-20261107-planner-llm-call-is-not-bounded-by-the-caller). Before it, the
// planner's model call ran under the bare caller ctx: a slow planner spent the
// caller's whole budget and the caller got DEADLINE_EXCEEDED with no payload. It
// is answered in time instead, as a timed-out partial plan that says nothing ran —
// which is the one thing a caller deciding whether to retry must know.
func (s *Server) planningOutOfTime(in *pb.Handoff, phase string, cause error, sessionID domain.SessionID, base llm.SessionSpend) *pb.Handoff {
	meta := map[string]string{
		"_partial_plan":     "true",
		PlanTimedOutKey:     "true",
		PlanTimeoutCauseKey: cause.Error(),
	}
	if sessionID != "" {
		meta["_session_id"] = string(sessionID)
	}
	s.stampSpend(meta, sessionID, base)
	slog.Warn("⏱️ ran out of time before the plan started", "phase", phase, "cause", cause)
	text := fmt.Sprintf("The plan did not start: it ran out of time while %s the request, so nothing was executed (%v).", phase, cause)
	return &pb.Handoff{
		Id:        in.GetId(),
		FromAgent: "orchestrator",
		ToAgent:   "user",
		Payload:   &pb.Object{Data: []byte(text)},
		Metadata:  meta,
	}
}

// Execute handles requests from the external world and manages the agent chain.
func (s *Server) Execute(ctx context.Context, in *pb.Handoff) (execOut *pb.Handoff, execErr error) {
	rawInput := string(in.Payload.Data)

	// ADR-0150 D1: the single-agent lane (execution.lane.default single_agent, the
	// default), which is the ADR-0050 D1 branch promoted from control arm to
	// product lane. It skips Router classification, Planner and DAGExecutor — the
	// input goes verbatim to one agent through the same CallAgent seam a plan step
	// uses — but NOT the caller's identity: the session, its conversation link and
	// the ordering chat turn's lifetime are set up below exactly as for a plan, and
	// the request leaves through executeSingleAgent once they are
	// (T-20261112-baseline-arm-run-the-use-case-journeys-through: run before them,
	// approvals named no beneficiary and the step did not know whom it acted for).
	bypass := s.ExecCfg.Lane.IsSingleAgent()
	if bypass {
		if why := singleAgentDelegationRefusal(s.resolveBindingFromHandoff(ctx, in.GetMetadata())); why != "" {
			return nil, status.Error(codes.FailedPrecondition, why)
		}
	}

	// An operator-authored plan (contract 0074) is a plan by construction, so
	// there is nothing for the router to classify — and running it anyway could
	// route the plan's own text to CHAT and answer instead of executing it.
	authoredPlanJSON := in.GetMetadata()[AuthoredPlanMetadataKey]

	// The caller-deadline reserve (plan_watchdog.go), fixed once for this Execute:
	// routing, planning and the plan all end by the same instant, so whichever phase
	// is running when the time is up still leaves Execute the reserve to answer.
	reserve := newCallerReserve(ctx, callerDeadlineReserve(s.ExecCfg.Plan))

	// ADR-0131 (owner decision 2026-11-01): router classification is request-path
	// spend — it counts against the session ceiling and in this Execute's _llm_*
	// figures. The router runs before the session is resolved below, and opening
	// one here would create sessions for CHAT turns, so: a caller presenting an
	// existing live session routes under it (and is refused when it is over the
	// ceiling); otherwise the router's calls go on a spend tab that is settled
	// into the session once it exists, or into the kernel no-session bucket when
	// the turn never gets one (CHAT, clarification, errors).
	var callerSession domain.SessionID
	if s.Router != nil && authoredPlanJSON == "" && !bypass && s.SpendMeter != nil {
		callerSession = s.liveCallerSession(ctx)
	}
	routeCtx := ctx
	var routeTab *llm.SpendTab
	if callerSession != "" {
		routeCtx = domain.WithSessionID(ctx, callerSession)
	} else {
		routeCtx, routeTab = llm.WithSpendTab(ctx)
	}
	routeBase := s.SpendMeter.Session(callerSession)
	var sessionID domain.SessionID
	defer func() { s.SpendMeter.SettleTab(routeTab, sessionID) }() // early returns; no-op once settled

	// ADR-0031: Route through InputRouter when configured.
	// The Router classifies raw input before any enrichment (mood, LTM, etc.).
	// Not in the single-agent mode (ADR-0050 D1): the one agent answers everything.
	if s.Router != nil && authoredPlanJSON == "" && !bypass {
		routerInput := domain.RouterInput{
			Body:       rawInput,
			SourceType: "grpc",
			Metadata:   in.GetMetadata(),
		}
		boundedRouteCtx, stopRouteReserve := reserve.bound(routeCtx, errPlanningCallerDeadline("routing", reserve.reserve))
		decision, err := s.Router.Resolve(boundedRouteCtx, routerInput)
		routeCut := planCeilingCause(ctx, boundedRouteCtx)
		stopRouteReserve()
		// Cut by the reserve: answered as out of time even when a decision arrived
		// on the edge, since no time is left to act on it.
		if routeCut != nil {
			return s.planningOutOfTime(in, "classifying", routeCut, "", llm.SessionSpend{}), nil
		}
		if err != nil {
			if errors.Is(err, llm.ErrSpendCeiling) && callerSession != "" {
				// The visible ADR-0131 D3 ending, with its TaskEvent record.
				return nil, s.endPlanAtSpendCeiling(callerSession, "", routeBase, err)
			}
			return nil, fmt.Errorf("router: %w", err)
		}
		switch decision.Type {
		case domain.DecisionChat:
			// ADR-0032: CHAT is handled by the ReactiveEngine (premium) via ProcessSync.
			// Falls back to not_implemented when no SyncProcessor is wired (OSS build).
			if sp, ok := s.SignalReceiver.(SyncProcessor); ok {
				convID := in.GetMetadata()["_conversation_id"]
				if convID == "" {
					// The sync (chat) pipelines are keyed to a conversation stream, so a
					// turn with no conversation has nothing to answer it: ProcessSync
					// with an empty stream can only fail ("no sync WatchConfig found for
					// stream \"\""), which surfaced as UNKNOWN for any agent-plane Execute
					// the router read as chat. Run it as work instead — the PLAN path
					// below, which opens a fresh session exactly as a turn with no
					// session header does. The caller's session header is not consulted
					// here; an unadoptable raw x-session-id stays unadopted (ADR-0139 D8).
					// T-20261104-agent-plane-execute-with-an-unrecognised-raw-x.
					slog.Info("router: chat turn with no conversation, running it as a plan")
					break
				}
				sig := domain.Signal{
					StreamID: convID,
					RawText:  rawInput,
					Payload:  metadataToPayload(in.GetMetadata()),
				}
				resp, err := sp.ProcessSync(ctx, sig)
				if err != nil {
					return nil, fmt.Errorf("chat ProcessSync: %w", err)
				}
				return handoffToProto(resp), nil
			}
			return &pb.Handoff{Payload: &pb.Object{
				Type: "not_implemented",
				Data: []byte("chat"),
			}}, nil
		case domain.DecisionWatch:
			return &pb.Handoff{Payload: &pb.Object{
				Type: "not_implemented",
				Data: []byte("watch"),
			}}, nil
		case domain.DecisionClarification:
			return s.serializeClarification(decision)
		case domain.DecisionPlan:
			// Fall through to the existing PLAN path below.
		}
		// Ingestion is intentionally NOT a router outcome: storing content into LTM
		// is an automatic memory-subsystem function (IngestionManager / the
		// /v1/ingest webhook / the SynapticWatcher), not a user request planned into
		// agent tasks — agents cannot write LTM directly. A DecisionIngest arriving
		// via an explicit Layer-0 gateway intent falls through to the PLAN path
		// rather than being rewritten into an ingestion plan.
	}

	userInput := rawInput

	if s.SessionMgr != nil {
		// Phase 2 strict mode: refuse to invent a session. The caller must have opened one
		// (CreateSession) and must present it, so "new work" and "continuation" are the
		// caller's decision rather than a guess made from whether a header happens to be set.
		if s.ExecCfg.Session.RequireExplicitSession {
			resolved := s.resolveCallerSession(ctx)
			if resolved == "" {
				return nil, status.Error(codes.InvalidArgument,
					"execution.require_explicit_session is on: open a session (CreateSession) and present it as x-session-id, or a lease bound to one as x-lease-id")
			}
			ses, err := s.SessionMgr.GetSession(ctx, resolved)
			if err != nil || ses == nil {
				return nil, status.Errorf(codes.NotFound, "unknown session %q", resolved)
			}
			if ses.Status == domain.SessionCompleted {
				return nil, status.Errorf(codes.FailedPrecondition, "session %q is completed", resolved)
			}
			sessionID = ses.ID
			ctx = domain.WithSessionID(ctx, sessionID)
		} else if ses := s.loadOrCreateSession(ctx, userInput); ses != nil {
			// ADR-0084 D2: when this work was ordered by a chat turn, link the session
			// back to the conversation. Resolved from the caller's lease, never from a
			// client-supplied field, and persisted only on a session we just opened —
			// re-linking an existing session would rewrite its origin.
			if b, known := s.resolveBindingFromHandoff(ctx, in.GetMetadata()); known && b.ConversationID != "" && ses.ConversationID == "" {
				ses.ConversationID = b.ConversationID
				ses.OriginMessageID = b.OriginMessageID
				if serr := s.SessionMgr.SaveConversationLink(ctx, ses.ID, b.ConversationID, b.OriginMessageID); serr != nil {
					slog.Warn("could not link session to its conversation", "session", ses.ID, "err", serr)
				}
			}
			sessionID = ses.ID
			// Phase 0: carry the session through the whole execution context.
			//
			// Until now WithSessionID was called ONLY on inbound agent RPCs, so the
			// executor goroutine — which writes step records, tool-output records and
			// content nodes — ran with no session in ctx. Those writes were stamped
			// with an empty session_id, which is why the ADR-0048 D1 same-session
			// filter could never match on the read side: writer wrote "", reader
			// compared a lease. Both sides now speak the same identifier.
			ctx = domain.WithSessionID(ctx, sessionID)
		}
	}

	// ADR-0145 D5a resume: the ask-back round and the pending InputRequired a
	// delegated request answers are kernel state the chat lane bound on the turn
	// lease, never an agent's metadata.
	stampAskBack(in, func(md map[string]string) (domain.LeaseBinding, bool) {
		return s.resolveBindingFromHandoff(ctx, md)
	})

	// A plan a chat turn delegated lives no longer than the turn
	// (turn_delegation.go, T-20261106-a-chat-turn-that-fails-deadline-exceeded-or):
	// it runs under a context the turn's end cancels, and a late Execute from a
	// turn that has already ended is refused. The ordering turn is read from the
	// caller's lease binding only, like the conversation link above.
	// ADR-0145 D14: a WORKER (a plan step's lease) handing extra work to the
	// planner. The nested run is one level deeper (bounded), and its steps run on
	// dedicated agent instances: the delegating worker's own process is still
	// inside its step and would otherwise serve the nested steps after it.
	// ADR-0150 D2: a subtask worker never hands work to the planner (its
	// delegate_to_planner is refused here, at the kernel): it reports what is
	// missing to its parent instead.
	if s.Subtasks.IsChild(callerLeaseID(ctx, in.GetMetadata())) {
		return nil, status.Error(codes.FailedPrecondition,
			"delegation refused: a subtask cannot hand work to the planner (ADR-0150 D2); finish with what your tools can do and say in your answer what else is needed")
	}
	if b, known := s.resolveBindingFromHandoff(ctx, in.GetMetadata()); known && b.RunID != "" && b.StepIndex >= 0 {
		depth := b.DelegationDepth + 1
		if depth > s.ExecCfg.Plan.MaxWorkerDelegationDepth {
			return nil, status.Errorf(codes.FailedPrecondition,
				"worker delegation refused: this work already runs at delegation depth %d (execution.plan.max_worker_delegation_depth %d); finish the step with what you have and report what is missing",
				b.DelegationDepth, s.ExecCfg.Plan.MaxWorkerDelegationDepth)
		}
		ctx = domain.WithDedicatedInstance(domain.WithDelegationDepth(ctx, depth))
		// The nested run may cite the delegating run's ledger entries (readable
		// under the person's access; T-20261109-staged-planner-names-earlier-stage-ledger).
		ctx = domain.WithParentRun(ctx, b.RunID)
		// The delegating step is in progress while the nested run works: the
		// dispatcher neither times it out nor re-dispatches it until we return.
		defer s.WorkerDelegations.Begin(callerLeaseID(ctx, in.GetMetadata()))()
		slog.Info("ADR-0145 D14: worker delegation to the planner", "from_run", b.RunID, "from_step", b.StepIndex, "depth", depth)
	}
	var delegated *DelegatedRun
	if s.Delegations != nil {
		if b, known := s.resolveBindingFromHandoff(ctx, in.GetMetadata()); known {
			runCtx, run, derr := s.Delegations.BeginRun(ctx, b)
			if derr != nil {
				return nil, status.Error(codes.FailedPrecondition, derr.Error())
			}
			ctx, delegated = runCtx, run
			// The turn's chat lane reads every direct delegation's structured result
			// kernel-side (ADR-0145 D2/D5a/D8: turn index, ask-back, reply check),
			// whichever return path produced it.
			defer func() {
				if execOut != nil {
					run.SetResult(execOut.GetMetadata())
				}
				run.End()
			}()
		}
	}

	// ADR-0150 D1: the single-agent lane leaves here, with the session, its
	// conversation link and the ordering turn's registration in place.
	if bypass {
		return s.executeSingleAgent(ctx, in, rawInput, sessionID, delegated, reserve)
	}

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
		var authored domain.ExecutionPlan
		if uerr := json.Unmarshal([]byte(authoredPlanJSON), &authored); uerr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "authored plan is not valid JSON: %v", uerr)
		}
		if _, terr := executer.TopologicalSort(authored.Steps); terr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "authored plan is not executable: %v", terr)
		}
		plan = &authored
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
		// generate, after an auction and a seed recall it cannot use.
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

		// runnerUps and winningAgentID are captured from the auction so that the
		// fallback loop can use runner-ups when SelfHealer exhausts.
		var runnerUps []domain.ScoredCandidate
		winningAgentID := ""

		innerFn := harness.StepFunc(func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
			if h.Context["_heal_attempt"] != "" {
				healingOccurred = true
			}
			auctionTask := &domain.DispatchTask{
				ID:          fmt.Sprintf("task-%d", i),
				Description: step.Query,
				Context:     fmt.Sprintf("Subject: %s", plan.Subject),
				// Deadline is left zero: the Dispatcher stamps the deadline the step
				// actually runs under (execution.plan.step_timeout_* under the plan
				// ceiling). A hardcoded `now + 20 s` here was read by nothing
				// (T-20261105-slow-model-reliability-hardcoded-20-s).
			}
			// ROUTE-03: thread the step's declared capabilities into the
			// auction ONLY under the capability_contract arm, so the control
			// arm leaves RequiredCapabilities empty (byte-identical L1).
			if s.ExecCfg.Routing.CapabilityContract {
				auctionTask.RequiredCapabilities = step.RequiredCapabilities
			}
			// Agent pin: carried unconditionally (the flag is read at the gate, so
			// one place decides whether a pin is honoured). Empty PreferredAgent is
			// the overwhelmingly common case and changes nothing downstream.
			auctionTask.PreferredAgent = step.PreferredAgent
			auctionTask.AgentPin = step.AgentPin
			// Effect-checked dispatch: carried unconditionally (it is a fail-closed
			// eligibility gate, not an A/B arm). nil stays nil — "declared nothing"
			// (a legacy plan, logged unverified) differs from an empty "needs none".
			auctionTask.RequiredEffects = step.RequiredEffects
			// ADR-0100 D4: carry the step's budget + verification flag so dispatch
			// can pick its per-step policy. The auction ignores both, so this is
			// inert on that arm.
			auctionTask.MaxEnergy = step.MaxEnergy
			auctionTask.CheckpointAfter = step.CheckpointAfter
			result, err := s.Dispatcher.Execute(ctx, auctionTask, h)
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

		// Neural trace ingestion — async, must not block step execution.
		if trace := resp.Context["_thought_trace"]; trace != "" {
			healAttempt := 0
			if h := handoff.Context["_heal_attempt"]; h != "" {
				if n, err := strconv.Atoi(h); err == nil {
					healAttempt = n
				}
			}
			storeNeuralTrace(ctx, s.VectorStore, trace, newPlanID(), string(executionID), i, healAttempt, resp.FromAgent)
		}

		// Confidence tracking
		confMu.Lock()
		winConf := 0.0 // default to 0.0 (unknown) until auction completes
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
			// budget (execution.content).
			DependencyFit: dependencyFit(s.ExecCfg.Content),
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

// GetContextNode resolves a CID from the ContentStore (step results, SHA-256 keyed)
// or from pgvector (LTM documents, UUID keyed). Returns empty data when unknown.
// Used by agents' assemble_context(fetch_fn=agent.substrate.get_context_node).
// ADR-0022 Phase 3.
func (s *Server) GetContextNode(ctx context.Context, req *pb.ContextNodeRequest) (*pb.ContextNodeResponse, error) {
	if req == nil || req.Cid == "" {
		return &pb.ContextNodeResponse{}, nil
	}
	cid := domain.CID(req.Cid)

	// ADR-0048 D4: thread the caller's session into ctx so the read-gate below can
	// identify the caller against the node's owner. Phase 0: RESOLVED from the caller's
	// opaque BudgetLease, not read off the header — the header carried a per-STEP lease,
	// so the "owned by its session" gate was really "owned by its step" and an agent
	// could not read back its own offload from an earlier step.
	ctx = s.withCallerSession(ctx)

	// ADR-0145 D1: a ledger reference (a step result handed to a dependant, a
	// large tool result) resolves through the ledger's ENFORCED reader under the
	// caller's own predicate — never through the content store.
	if domain.IsLedgerID(req.Cid) {
		if !s.ledgerOn() {
			return &pb.ContextNodeResponse{Cid: req.Cid}, nil
		}
		return s.readLedgerNode(ctx, req.Cid), nil
	}

	// ADR-0048 D5: drill-down serves ONLY ContentStore CIDs (transient working-memory
	// / tool-result blobs). The former pgvector fallback returned raw LTM document
	// text via GetByID, bypassing the enforcing store (today authz.EnforcingVectorStore) that every other read goes
	// through (ADR-0034) — a scope hole. LTM is now reachable only through the
	// scope-filtered QueryMemory path.
	if s.ContentStore != nil {
		if node, err := s.ContentStore.Get(ctx, cid); err == nil {
			// ADR-0048 D4: read-gate. An owned node (an agent's offload) is readable
			// only by its owning session; an out-of-session caller gets not-found,
			// indistinguishable from absent (no existence leak).
			callerSid, _ := domain.SessionIDFromContext(ctx)
			if !domain.CanReadContentNode(node.OwnerSession, callerSid) {
				return &pb.ContextNodeResponse{Cid: req.Cid}, nil
			}
			return &pb.ContextNodeResponse{
				Cid:    req.Cid,
				Type:   node.Type,
				Data:   node.Data,
				Labels: node.Labels,
			}, nil
		}
	}

	return &pb.ContextNodeResponse{Cid: req.Cid}, nil // unknown CID → caller uses snippet
}

// PutContextNode stores an agent-offloaded blob in the ephemeral ContentStore and
// returns its CID (ADR-0048 D4/R7). The ContentStore stamps the owning session
// from ctx, so GetContextNode read-gates it to the owning session. Idempotent
// (content-addressed); the blob is plan-scoped and reclaimed at plan end.
func (s *Server) PutContextNode(ctx context.Context, req *pb.PutContextNodeRequest) (*pb.PutContextNodeResponse, error) {
	if req == nil || len(req.Data) == 0 {
		return nil, status.Error(codes.InvalidArgument, "data is required")
	}
	if s.ContentStore == nil {
		return nil, status.Error(codes.Unimplemented, "content store not configured")
	}
	// ADR-0048 D4: take the owning session from the caller's lease (never the payload,
	// and never a caller-chosen header) and thread it into ctx so the ContentStore stamps
	// it as owner. Phase 0: resolving through the lease registry means the owner is the
	// SESSION the kernel dispatched under, so the node stays readable for the whole
	// session instead of only the step that wrote it.
	ctx = s.withCallerSession(ctx)
	nodeType := req.NodeType
	if nodeType == "" {
		nodeType = "agent_offload"
	}
	cid, err := s.ContentStore.Put(ctx, req.Data, nodeType, nil, "")
	if err != nil {
		return nil, status.Error(codes.Internal, "content store put: "+err.Error())
	}
	return &pb.PutContextNodeResponse{Cid: string(cid)}, nil
}

// replanHandler re-plans through the plan's shared planner-call budget, so DAG
// replans and the validation retry draw on one count.
func (s *Server) replanHandler(budget *planCallBudget) executer.ReplanHandler {
	// The concrete *awareness.Planner reports its capability_contract arm, so a
	// replan under that arm keeps required_capabilities (and the vocabulary guard).
	caps, _ := s.Planner.(awareness.CapabilityContractSource)
	return awareness.NewPlannerReplanHandler(budget.generator(s.Planner), awareness.WithCapabilityContract(caps),
		awareness.WithCalendar(s.Calendar))
}

// taskEventWriter resolves the TaskEvent sink: the registry when it can write
// TaskEvents, decorated by the token recorder (contract 0075) when one is set.
// Decorating the resolved writer rather than adding an emission site means the
// hourly series counts exactly what the kernel recorded. Nil when the registry
// cannot write TaskEvents.
func (s *Server) taskEventWriter() executer.TaskEventWriter {
	var eventWriter executer.TaskEventWriter
	if s.Manager != nil {
		if ew, ok := s.Manager.Registry.(executer.TaskEventWriter); ok {
			eventWriter = ew
		}
	}
	if s.TokenRecorder != nil {
		eventWriter = s.TokenRecorder.Wrap(eventWriter)
	}
	return eventWriter
}

// wrapGen applies the optional generator decorator (Langfuse tracing) so routed
// thought-step generations are observable. Identity when GenWrapper is nil.
func (s *Server) wrapGen(g domain.Generator) domain.Generator {
	if s.GenWrapper == nil {
		return g
	}
	return s.GenWrapper(g)
}

func (s *Server) thoughtFn(plan *domain.ExecutionPlan) executer.StepFunc {
	return func(ctx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		// ADR-0139 D2: library prompts, the step's upstream results (the executor's
		// domain.StepDependency manifest, already ordered and bounded) in tagged data
		// regions (thought_synthesis.go).
		//
		// There is deliberately no "payload is the prompt" branch any more. The
		// executor always puts the step's query on the payload, so that branch —
		// kept for the ADR-0013 checkpoint probe, deleted by ADR-0139 D10 — ran for
		// EVERY thought step and sent the bare query with none of the results; the
		// results path was unreachable from the first commit. A merge step answered
		// "I don't see any previous steps"
		// (T-20261105-a-planner-thought-step-that-depends-on-two).
		results, err := thoughtResults(handoff)
		if err != nil {
			return nil, err
		}
		// The step as the executor's CURRENT plan has it (domain.WithPlanStep): the
		// captured plan is stale after a fan-out or replan, and indexing it would
		// read another step's query or run past its end (see stepFn).
		var query string
		if cur, ok := domain.PlanStepFrom(ctx); ok {
			query = cur.Query
		} else if i >= 0 && i < len(plan.Steps) {
			query = plan.Steps[i].Query
		} else if handoff != nil && handoff.Payload != nil {
			query = string(handoff.Payload.Data)
		}
		req, err := buildThoughtRequest(plan.Subject, thoughtToday(handoff, s.Calendar), query, results)
		if err != nil {
			return nil, err
		}

		// The model serving a thought step is the Provider's call, resolved from the
		// declared need — no per-step model name travels on the plan any more.
		resp, genErr := domain.GenerateVia(ctx, s.thoughtGenerator(), req)
		if genErr != nil {
			return nil, genErr
		}

		return &domain.Handoff{
			Payload: &domain.Payload{Data: []byte(strings.TrimSpace(resp.Text))},
			Context: handoff.Context,
		}, nil
	}
}

// SignalStream is the proactive neural signal endpoint. It receives Handoff
// messages from Daemon Observer agents, gatekeeps them through the Watcher
// (signal shape, auth token, circuit breaker), and hands each accepted signal to
// the SignalReceiver: proactive planning + operator approval (OSS Watcher) or
// condition/action evaluation (premium ReactiveEngine). ADR-0032.
func (s *Server) SignalStream(stream grpc.BidiStreamingServer[pb.Handoff, pb.SymbiosisEvent]) error {
	if s.Watcher == nil && s.SignalReceiver == nil {
		return errors.New("SignalStream: neither Watcher nor SignalReceiver configured")
	}

	ctx := stream.Context()
	for {
		protoHandoff, err := stream.Recv()
		if err != nil {
			return err
		}

		dHandoff := protoToHandoff(protoHandoff, s.TelemetryObserver)

		// ADR-0090: a signal from a registered ingress is a conversational turn, not a
		// trigger. It is routed to the chat lane and this iteration ends — the planner
		// never sees it, which is ADR-0080 D4 enforced for external traffic.
		if s.IngressInbound != nil && dHandoff.FromAgent != "" {
			msg := ingressFields(dHandoff)
			msg.Sender = domain.AgentPrincipal(dHandoff.FromAgent)
			err := s.IngressInbound.Accept(ctx, msg)
			if err == nil {
				continue
			}
			if !errors.Is(err, ingress.ErrNotAnIngress) {
				// A REGISTERED ingress whose message was refused — outside its namespace,
				// unopenable conversation. Dropping it silently would make a namespace
				// misconfiguration look like the bot simply not answering.
				slog.Warn("ADR-0090: inbound ingress message refused",
					"ingress", dHandoff.FromAgent, "err", err)
				continue
			}
			// ErrNotAnIngress: an ordinary agent signal, so fall through untouched.
		}

		if s.Watcher != nil {
			// The Watcher is the GATE here: signal shape, daemon token, circuit
			// breaker. It does not plan. Planning belongs to the SignalReceiver below
			// (the OSS Watcher's OnSignal, or premium's ReactiveEngine), so a signal is
			// planned at most once. This block used to call Watcher.ProcessSignal as
			// well, which in OSS — where the receiver IS the Watcher — planned every
			// signal twice and discarded both plans (ADR-0009, ADR-0032).

			// 1. Validate signal
			if valErr := s.Watcher.ValidateSignal(ctx, dHandoff); valErr != nil {
				_ = s.Watcher.HandleInvalidSignal(ctx, dHandoff)
				continue
			}

			// 2. Validate auth token
			if _, tokErr := s.Watcher.ValidateToken(ctx); tokErr != nil {
				if dHandoff.FromAgent != "" {
					_ = s.Watcher.HandleInvalidSignal(ctx, dHandoff)
				}
				continue
			}

			// Valid signal resets circuit breaker
			s.Watcher.ResetInvalidSignals(dHandoff.FromAgent)
		}

		// ADR-0032: the SignalReceiver acts on the signal. Premium: the
		// ReactiveEngine evaluates conditions and executes actions. OSS: the Watcher
		// plans the signal once and queues a non-empty plan for operator approval
		// (ApprovalHub / ResolveHITL); it runs only if approved. A kernel with no
		// Watcher falls back to the NoOpSignalReceiver.
		if s.SignalReceiver != nil {
			rawText := ""
			if dHandoff.Payload != nil {
				rawText = string(dHandoff.Payload.Data)
			}
			sig := domain.Signal{
				StreamID:  dHandoff.Context["_signal_type"],
				FromAgent: dHandoff.FromAgent,
				RawText:   rawText,
				Payload:   metadataToPayload(dHandoff.Context),
			}
			_ = s.SignalReceiver.OnSignal(ctx, sig)
		}
	}
}

// loadOrCreateSession resumes the caller's session, or opens a new one under goal.
//
// Phase 0: the caller's session is RESOLVED (resolveCallerSession) rather than read
// straight off x-session-id. The operator plane sends a real session ID and is unchanged;
// a caller that presents a lease now resumes the session that lease was bound to, instead
// of missing the lookup and silently minting a second session for the same work.
//
// Identity is still INFERRED here — "header present ⇒ continue, absent ⇒ create" cannot
// distinguish new work from a continuation from a replay, which is the resume defect. That
// is Phase 2's explicit OpenSession/CloseSession verbs, not this change.
func (s *Server) loadOrCreateSession(ctx context.Context, goal string) *domain.Session {
	mgr := s.SessionMgr
	if sid := s.resolveCallerSession(ctx); sid != "" {
		ses, err := mgr.GetSession(ctx, sid)
		if err == nil && ses != nil && ses.Status != domain.SessionCompleted {
			return ses
		}
	}
	ses, err := mgr.CreateSession(ctx, goal, "")
	if err != nil {
		return nil
	}
	return ses
}

// runFallback tries runner-up candidates in score order when the winner fails.
// effectRefusalClarification is the ask that ends a plan whose step no agent's tools
// can do (owner design, effect-checked dispatch): the same payload.type
// "clarification" sentinel the router's clarification uses, so every client that
// renders a clarification renders this one. The metadata keeps the partial plan's
// context (results of the steps that did run) and names the refusal.
func effectRefusalClarification(id string, e *domain.NoEffectMatchError, meta map[string]string) (*pb.Handoff, error) {
	// Options use the router clarification's option shape (serializeClarification),
	// so a client that renders one renders this one; the decision is left empty: the
	// answer is a new user turn, not a routing decision.
	type optionJSON struct {
		Label       string `json:"label"`
		Decision    string `json:"decision"`
		Recommended bool   `json:"recommended"`
	}
	type bodyJSON struct {
		Question string       `json:"question"`
		Options  []optionJSON `json:"options"`
		Missing  []string     `json:"missing_effects"`
		Detail   string       `json:"detail"`
	}
	missing := strings.Join(e.Missing, " + ")
	body := bodyJSON{
		Question: fmt.Sprintf("None of the available agents has a tool that can %s, which this request needs. "+
			"How should I proceed?", missing),
		Options: []optionJSON{
			{Label: fmt.Sprintf("Give an agent a tool that can %s (install an integration or widen an agent profile), then ask again", missing), Recommended: true},
			{Label: "Do the rest without that action"},
			{Label: "Cancel the request"},
		},
		Missing: e.Missing,
		Detail:  e.Error(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("effectRefusalClarification: %w", err)
	}
	if meta == nil {
		meta = map[string]string{}
	}
	meta["_partial_plan"] = "true"
	meta["_effect_refused"] = strings.Join(e.Missing, ",")
	slog.Warn("plan ended on an effect-check refusal; asking the user", "missing", e.Missing, "check", e.Check.Summary())
	return &pb.Handoff{
		Id:        id,
		FromAgent: "orchestrator",
		ToAgent:   "user",
		Payload:   &pb.Object{Type: "clarification", Data: data},
		Metadata:  meta,
	}, nil
}

// serializeClarification serializes a DecisionClarification into a pb.Handoff
// with payload.type = "clarification" and payload.data = JSON{question, options}.
// Follows the payload.type sentinel pattern (ADR-0031).
func (s *Server) serializeClarification(dec *domain.RouterDecision) (*pb.Handoff, error) {
	type optionJSON struct {
		Label       string `json:"label"`
		Decision    string `json:"decision"`
		Recommended bool   `json:"recommended"`
	}
	type bodyJSON struct {
		Question string       `json:"question"`
		Options  []optionJSON `json:"options"`
	}
	body := bodyJSON{Question: dec.ClarificationQuestion}
	for _, opt := range dec.ClarificationOptions {
		body.Options = append(body.Options, optionJSON{
			Label:       opt.Label,
			Decision:    string(opt.Decision),
			Recommended: opt.Recommended,
		})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("serializeClarification: %w", err)
	}
	return &pb.Handoff{Payload: &pb.Object{Type: "clarification", Data: data}}, nil
}

// Returns (response, true) on the first successful fallback, or (nil, false) if
// all candidates fail or fall below the confidence threshold.
func (s *Server) runFallback(
	ctx context.Context,
	i int,
	handoff *domain.Handoff,
	runnerUps []domain.ScoredCandidate,
	winningAgentID string,
	healErr *harness.HealingExhaustedError,
) (*domain.Handoff, bool) {
	winnerConf := 0.0
	if cStr := handoff.Context["_winning_confidence"]; cStr != "" {
		if c, err := strconv.ParseFloat(cStr, 64); err == nil {
			winnerConf = c
		}
	}
	threshold := s.ExecCfg.Plan.FallbackConfidenceThreshold * winnerConf
	if threshold == 0 {
		threshold = s.ExecCfg.Plan.FallbackConfidenceThreshold
	}

	instanceIDs := s.Manager.GetInstanceIDs(winningAgentID)
	topN := 3
	if len(runnerUps) < topN {
		topN = len(runnerUps)
	}

	lastErrMsg := ""
	if healErr.LastError != nil {
		lastErrMsg = healErr.LastError.Error()
	}

	for _, runnerUp := range runnerUps[:topN] {
		if runnerUp.Score < threshold {
			continue
		}
		fallbackHandoff := &domain.Handoff{
			Payload: &domain.Payload{Data: handoff.Payload.Data},
			Context: make(map[string]string, len(handoff.Context)+1),
		}
		for k, v := range handoff.Context {
			fallbackHandoff.Context[k] = v
		}
		fallbackHandoff.Context["task_id"] = fmt.Sprintf("task-%d", i)
		fallbackHandoff.Context["_fallback_reason"] = fmt.Sprintf("%s (from %s)", lastErrMsg, winningAgentID)

		excludeID := ""
		if runnerUp.Agent.ID == winningAgentID && len(instanceIDs) > 0 {
			excludeID = instanceIDs[0]
		}

		fbResp, fbErr := s.Dispatcher.CallAgent(ctx, runnerUp.Agent.ID, fallbackHandoff, excludeID)
		if fbErr == nil && fbResp != nil {
			slog.Info("🔄 Fallback succeeded", "step", i,
				"runner_up", runnerUp.Agent.ID,
				"exclude_instance", excludeID)
			return fbResp, true
		}
	}
	return nil, false
}

// handleMemoryBarrier is a barrier SIGNAL only — it no longer ingests anything.
// ADR-0049 D3 moved mutations onto synchronous `mnemonic_action` records, so
// re-ingesting here produced a duplicate row; step output itself is no longer captured
// (RecordExecution was deleted 2026-11-05). The former prioritised path went through IngestSync, which has since
// been removed along with the rest of the LLM-importance write path.
func (s *Server) handleMemoryBarrier(
	stepCtx context.Context,
	i int,
	resp *domain.Handoff,
	kernelSync, envMutation bool,
	links []string,
) {
	// ADR-0049 D3: mutations are recorded as synchronous `mnemonic_action` records,
	// so the barrier does not re-ingest a `Step N result:` row. Retained as a
	// barrier signal only (the sync-flush guarantee for mutations is met by the
	// synchronous action save). `resp`/`links`/`stepCtx` are now unused here.
	if kernelSync || envMutation {
		slog.Info("🛡️ Memory Barrier (signal only; no ingest)",
			"step", i, "reason", map[string]bool{"sync": kernelSync, "mutation": envMutation})
	}
}

// injectMoodContext appends recent session events as mood context to userInput.
func injectMoodContext(ctx context.Context, s *Server, sessionID domain.SessionID, userInput string) string {
	// Plan Drift: warn if session was completed long ago.
	if s.SessionMgr != nil {
		ses, err := s.SessionMgr.GetSession(ctx, sessionID)
		if err == nil && ses != nil && !ses.CompletedAt.IsZero() {
			days := int(time.Since(ses.CompletedAt).Hours() / 24)
			if days >= s.ExecCfg.Plan.PlanDriftDays && s.ExecCfg.Plan.PlanDriftDays > 0 {
				driftNote := fmt.Sprintf(
					"\n\nCONTEXT DRIFT WARNING: This session was completed %d days ago. "+
						"References, file paths, and configuration may be stale. "+
						"Verify critical details before acting.",
					days,
				)
				userInput = userInput + driftNote
			}
		}
	}
	return userInput
}

// looseString decodes a JSON string OR number into a string.
//
// An external id is an opaque handle: Telegram numbers its users, Slack does
// not. Insisting on one JSON type here would make the decoder silently drop half
// of them, and a dropped id is indistinguishable from a bridge that reports none.
type looseString string

func (l *looseString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*l = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*l = looseString(s)
		return nil
	}
	// A number, and it stays EXACTLY as written: routing it through float64 would
	// round a 64-bit user id into a different person's.
	*l = looseString(strings.TrimSpace(string(b)))
	return nil
}

// ingressFields pulls the sender and the text out of an ingress signal.
//
// The SDK sends {"external_id": ..., "text": ...} as the payload. The external id
// is read from the PAYLOAD rather than from metadata deliberately: it is a claim
// about who wrote the message, not about who is connected, and it is checked
// against the ingress's namespace before it is trusted for anything.
//
// speaker_id and speaker_name are OPTIONAL labels the SDK passes through from an
// ingress's extra kwargs. They were already on the wire and were being discarded
// here, which is why the unbound worklist could only ever show bare numeric ids —
// the one thing an operator cannot make a decision from.
func ingressFields(h *domain.Handoff) ingress.InboundMessage {
	if h == nil || h.Payload == nil {
		return ingress.InboundMessage{}
	}
	var body struct {
		ExternalID string `json:"external_id"`
		Text       string `json:"text"`
		Policy     string `json:"policy"`
		// SpeakerID arrives as a NUMBER from Telegram and as a STRING from
		// platforms that do not number their users. Decoded loosely so a bridge
		// author does not have to know which shape this decoder happened to pick —
		// guessing wrong would silently drop the id and leave a nameless worklist.
		SpeakerID   looseString `json:"speaker_id"`
		SpeakerName string      `json:"speaker_name"`
		DisplayName string      `json:"display_name"`
	}
	if err := json.Unmarshal(h.Payload.Data, &body); err != nil {
		return ingress.InboundMessage{}
	}
	return ingress.InboundMessage{
		ExternalID:  body.ExternalID,
		Text:        body.Text,
		Policy:      body.Policy,
		SpeakerID:   string(body.SpeakerID),
		Username:    body.SpeakerName,
		DisplayName: body.DisplayName,
	}
}
