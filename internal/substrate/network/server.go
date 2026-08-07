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
	"github.com/cambrian-sh/core/internal/centralexec"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
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
	Auctioneer  domain.Auctioneer
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
	ModelRouter    *llm.ProviderRegistry
	Provider       domain.LLMProvider // ADR-0042: agent-step model provisioning (health-guarded)
	SessionMgr     *session.SessionManager
	// ConvSurfaces resolves which ENTRY POINT a conversation arrived on, for turns
	// that have no task session. Without it a chat turn from an ingress is
	// authorised as an ordinary agent call and the ingress's policy never applies.
	ConvSurfaces authz.ConversationSurfaceReader
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
	// SceneWriterFactory produces a fresh domain.SceneWriter for each Execute call.
	// ADR-0025: per-request because PgSceneWriter tracks lastSceneID for specifies edges.
	// nil = scene writing disabled.
	SceneWriterFactory func() domain.SceneWriter

	// TokenRecorder taps token usage for the hourly spend series (contract 0075).
	// nil ⇒ no tap and the resolved event writer is used unchanged, which is what
	// a build with no operator console wants.
	TokenRecorder TokenRecorder

	// AgentCallLogger records agent-initiated LLM calls (GenerateViaModelStream) to an
	// external observability backend. nil = disabled (no-op). OBSERVABILITYREQ REQ1.
	AgentCallLogger AgentCallLogger

	// GenWrapper decorates a raw generator with cross-cutting concerns (Langfuse
	// tracing) so thought/synthesis steps generated in-kernel are
	// observable, not just the Planner's own generator. nil = identity.
	GenWrapper func(domain.Generator) domain.Generator

	// ResourceSelector is the ADR-0037 Central-Executive selection arm. When the
	// session's assigned variant is "efe", a step is bound via this selector
	// instead of the Auctioneer. nil = auction only.
	ResourceSelector domain.ResourceSelector
	// SelectorMode is the resource_selector flag: "auction" | "efe" | "auto".
	SelectorMode string
	// EFETrafficPercent is the session-scoped A/B split for "auto" mode (0..100).
	EFETrafficPercent int

	// SignalReceiver dispatches incoming signals to the condition/action pipeline.
	// OSS build: OSS Watcher (passive LTM enrichment + Planner).
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

	// WatchHandler provides the 4 WatchConfig CRUD RPCs. Injected by the premium
	// binary via the app.Options reactive hook; nil in OSS — RPC shells guard against
	// nil and return Unimplemented. ADR-0032 / ADR-0057.
	WatchHandler domain.WatchConfigHandler

	// REQ-SDK-007c: artifact storage, gated by the decision point. Both nil → the
	// artifact RPCs return Unimplemented.
	ArtifactBytes ArtifactByteStore // CAS byte store (ArtifactVault)
	ArtifactMeta  ArtifactMetaStore // metadata + tags persistence

	// Authz is the access-control decision point (ADR-0085). nil ⇒ the OSS
	// allow-all default: the server still ASKS at every enforcement point, and in
	// an unscoped deployment the answer is always yes.
	Authz domain.Authorizer

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

	// TokenSink, when set, receives each managed-proxy generation chunk for the
	// operator feed's live-only token lane (ADR-0047 D12/0047-23). Best-effort.
	TokenSink func(sessionID string, stepIndex int, text string)

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

	// Embedder backs the Embed RPC (ADR-0041) used by an agent's Local Recurrent
	// Workspace for relevance ranking. nil → Embed returns Unimplemented.
	Embedder domain.Embedder

	// YieldDriver resolves agent yields (ADR-0037 D10–D15) on the EFE dispatch
	// path: it binds + dispatches a yielded sub-goal and resumes the parent. nil ⇒
	// a yield is inert (the sub-goal is not executed).
	YieldDriver *centralexec.YieldDriver
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
	auctioneer domain.Auctioneer,
	watcher *supwatcher.Watcher,
	modelRouter *llm.ProviderRegistry,
	sessionMgr *session.SessionManager,
	workspaceStage domain.WorkspaceStage,
	llmGateway LLMGateway,
	observer domain.TelemetryObserver,
	contentStore domain.ContentStore,
) *Server {
	return &Server{
		Planner:             planner,
		Manager:             manager,
		Auctioneer:          auctioneer,
		MemoryAgent:         memoryAgent,
		ExecCfg:             execCfg,
		VectorStore:         vectorStore,
		MemorySearcher:      memorySearcher,
		Hippocampus:         hippocampus,
		EnqueueVerification: enqVerification,
		Watcher:             watcher,
		ModelRouter:         modelRouter,
		SessionMgr:          sessionMgr,
		WorkspaceStage:      workspaceStage,
		LLMGateway:          llmGateway,
		TelemetryObserver:   observer,
		ContentStore:        contentStore,
	}
}

func validatePlan(plan *domain.ExecutionPlan, knownTools map[string]struct{}) error {
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

func planWithValidation(ctx context.Context, planner Planner, userInput string, knownTools map[string]struct{}) (*domain.ExecutionPlan, error) {
	plan, err := planner.GetExecutionPlan(ctx, userInput)
	if err != nil {
		return nil, err
	}

	if err := validatePlan(plan, knownTools); err == nil {
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

		if err := validatePlan(plan, knownTools); err != nil {
			return nil, fmt.Errorf("planner produced an invalid plan after retry: %w", err)
		}
		return plan, nil
	}
}

// Execute handles requests from the external world and manages the agent chain.
func (s *Server) Execute(ctx context.Context, in *pb.Handoff) (*pb.Handoff, error) {
	rawInput := string(in.Payload.Data)

	// ADR-0050 D1: benchmark React-baseline arm. Skips Router classification,
	// Planner, Auctioneer, and DAGExecutor — the input goes
	// verbatim to one configured agent through the same CallAgent seam a
	// winning bidder would use (same priming, grants, scope, telemetry).
	if s.ExecCfg.Routing.BypassAuction {
		return s.executeBypassAuction(ctx, in, rawInput)
	}

	// An operator-authored plan (contract 0074) is a plan by construction, so
	// there is nothing for the router to classify — and running it anyway could
	// route the plan's own text to CHAT and answer instead of executing it.
	authoredPlanJSON := in.GetMetadata()[AuthoredPlanMetadataKey]

	// ADR-0031: Route through InputRouter when configured.
	// The Router classifies raw input before any enrichment (mood, LTM, etc.).
	if s.Router != nil && authoredPlanJSON == "" {
		routerInput := domain.RouterInput{
			Body:       rawInput,
			SourceType: "grpc",
			Metadata:   in.GetMetadata(),
		}
		decision, err := s.Router.Resolve(ctx, routerInput)
		if err != nil {
			return nil, fmt.Errorf("router: %w", err)
		}
		switch decision.Type {
		case domain.DecisionChat:
			// ADR-0032: CHAT is handled by the ReactiveEngine (premium) via ProcessSync.
			// Falls back to not_implemented when no SyncProcessor is wired (OSS build).
			if sp, ok := s.SignalReceiver.(SyncProcessor); ok {
				convID := in.GetMetadata()["_conversation_id"]
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

	var sessionID domain.SessionID
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
		p, perr := planWithValidation(ctx, s.Planner, userInput, nil)
		if perr != nil {
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

	stepFn := func(stepCtx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		// The DAGExecutor may replan (ADR-0005) or fan-out expand (ADR-0078) its plan
		// mid-run, growing it beyond the `plan` this closure captured, and then dispatch
		// a step whose index is out of range HERE. The executor builds the handoff from
		// its CURRENT plan's step, so trust the handoff's query and reconstruct a minimal
		// step rather than index a stale slice — an out-of-range index must never panic
		// and crash the whole kernel. (RequiredCapabilities is unavailable for a
		// replanned step; routing falls back to query-only selection.)
		var step domain.Step
		if i >= 0 && i < len(plan.Steps) {
			step = plan.Steps[i]
		} else {
			q := ""
			if handoff != nil && handoff.Payload != nil {
				q = string(handoff.Payload.Data)
			}
			step = domain.Step{Query: q}
		}
		prePwd, _ := os.Getwd()

		// Stamp task_id so AgentManager.CallAgent builds a consistent snapshot key.
		if handoff.Context == nil {
			handoff.Context = make(map[string]string)
		}
		handoff.Context["task_id"] = fmt.Sprintf("task-%d", i)

		// ADR-0023 Fix 1+3: inject session token and step index so cognitive agents
		// can call GenerateViaModelStream and log their position in the plan.
		handoff.Context["_step_index"] = fmt.Sprintf("%d", i)
		if s.LLMGateway != nil {
			// ADR-0018 pending: pre-allocate with the primary model agent so
			// StreamChunks has a non-empty Winner ID to resolve the streaming client.
			sa := domain.StepAllocation{}
			if s.ModelRouter != nil && s.ModelRouter.Ollama != nil {
				sa.Winner = domain.AgentDefinition{ID: "llm:ollama:qwen3:8b"}
			}
			tokenID, _ := s.LLMGateway.Acquire(stepCtx, sa, 4096, 30*time.Second)
			// Phase 0: record WHICH session/run/step this lease belongs to, so an agent
			// presenting it can be resolved back to its session server-side. Without
			// this the kernel has no way to answer "whose lease is this?" and has to
			// trust whatever the agent puts in a header.
			if r := s.leaseResolver(); r != nil {
				// AgentID is intentionally left empty: the lease is acquired at step
				// DISPATCH, before the auction picks a winner, so the executing agent
				// is not yet known here.
				r.BindLease(tokenID, domain.LeaseBinding{
					SessionID: sessionID,
					RunID:     executionID,
					StepIndex: i,
				})
			}
			handoff.Context["_session_token_id"] = string(tokenID)
			defer func() { _, _ = s.LLMGateway.Complete(stepCtx, tokenID) }()
		}

		// healingOccurred is set true by innerFn when SelfHealer injects _heal_attempt,
		// signalling that at least one retry was consumed. It is passed to the Memory
		// Barrier, which since ADR-0049 D3 only logs — the step result is recorded by
		// RecordExecution onto the Tier-1 channel, not re-ingested here.
		healingOccurred := false

		// runnerUps and winningAgentID are captured from the auction so that the
		// fallback loop can use runner-ups when SelfHealer exhausts.
		var runnerUps []domain.ScoredCandidate
		winningAgentID := ""

		innerFn := harness.StepFunc(func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
			if h.Context["_heal_attempt"] != "" {
				healingOccurred = true
			}
			auctionTask := &domain.AuctionTask{
				ID:          fmt.Sprintf("task-%d", i),
				Description: step.Query,
				Context:     fmt.Sprintf("Subject: %s", plan.Subject),
				Deadline:    time.Now().Add(20 * time.Second),
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
			// ADR-0100 D4: carry the step's budget + verification flag so dispatch
			// can pick its per-step policy. The auction ignores both, so this is
			// inert on that arm.
			auctionTask.MaxEnergy = step.MaxEnergy
			auctionTask.CheckpointAfter = step.CheckpointAfter
			// ADR-0037: when the session's variant is "efe", bind via the
			// Central-Executive selector (no Auctioneer). Any selection failure
			// falls through to the auction so the path is never worse than today.
			if s.useEFE(sessionID) {
				if resp, ok := s.selectViaEFE(ctx, auctionTask, step.Query, h, &winningAgentID); ok {
					return resp, nil
				}
			}

			result, err := s.Auctioneer.Execute(ctx, auctionTask, h)
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

		healer := &harness.SelfHealer{
			Restorer:  s.Manager,
			TaskID:    fmt.Sprintf("task-%d", i),
			StepIndex: i,
		}
		resp, err := healer.Wrap(innerFn)(stepCtx, handoff)
		if err != nil {
			var healErr *harness.HealingExhaustedError
			if errors.As(err, &healErr) {
				if s.MemoryAgent != nil {
					slog.Warn("🏥 Healing exhausted", "step", i,
						"attempts", healErr.AttemptCount,
						"loop", healErr.LoopDetected,
						"err", healErr.LastError)
					// Experiential memory removed: no negative-edge (failure) write-back.
				}

				// Inter-step fallback: try runner-up candidates when winner fails.
				if s.ExecCfg.Plan.FallbackEnabled && len(runnerUps) > 0 {
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

	planCtx, cancelPlan := context.WithTimeout(ctx, time.Duration(s.ExecCfg.Plan.PlanTimeoutMs)*time.Millisecond)
	defer cancelPlan()

	var eventWriter executer.TaskEventWriter
	if ew, ok := s.Manager.Registry.(executer.TaskEventWriter); ok {
		eventWriter = ew
	}
	// Contract 0075: tap token usage for the hourly series on its way past.
	// Decorating the resolved writer rather than adding an emission site means
	// the series counts exactly what the kernel recorded and cannot drift from
	// it. Nil recorder ⇒ the original writer, unchanged.
	if s.TokenRecorder != nil {
		eventWriter = s.TokenRecorder.Wrap(eventWriter)
	}

	var sceneWriter domain.SceneWriter
	if s.SceneWriterFactory != nil {
		sceneWriter = s.SceneWriterFactory()
	}
	executor := &executer.DAGExecutor{
		EventWriter:         eventWriter,
		EnqueueVerification: s.EnqueueVerification,
		// ADR-0049 §A2.2. The recorder is wired again, but the two paths behind it are
		// gated independently and only one is armed by default:
		//   RecordExecution  — the RAW path removed 2026-07-18 (whole tool payloads as
		//                      single embeddings). Self-gates on RecordExperiential,
		//                      which stays false permanently.
		//   WritePlanScene   — the OUTCOME RECORD. Self-gates on RecordOutcomes
		//                      (execution.experience_records_enabled), default false.
		// Wiring the recorder therefore does NOT restore the removed design; with both
		// flags at their defaults this is exactly the current no-write-back behaviour.
		MemoryRecorder:         s.MemoryAgent,
		Surprise:               s.SurpriseOracle, // ADR-0049 A2.3: may be nil ⇒ surprise unknown (-1)
		WorkspaceStage:         s.WorkspaceStage,
		ArtifactLister:         s.ArtifactMeta, // ADR-0034: surface prior-step artifacts (scope-filtered)
		Authz:                  s.Authz,        // ADR-0085: artifact discovery filter (may be nil)
		LLMGateway:             s.LLMGateway,
		Observer:               s.TelemetryObserver,
		ContentStore:           s.ContentStore,
		StepCache:              s.StepCache,
		SceneWriter:            sceneWriter,
		UseGlobalWorkspace:     s.ExecCfg.Workspace.UseGlobalWorkspace,
		MaxContextSlots:        s.ExecCfg.Plan.MaxContextSlots,
		ContextRefSnippetChars: s.ExecCfg.Plan.ContextRefSnippetChars,
		ThoughtFn:              executer.StepFunc(s.thoughtFn(plan)),
		CheckpointValidator:    awareness.NewLLMCheckpointValidator(s.Planner),
		ReplanHandler:          s.replanHandler(),
		MaxReplanAttempts:      s.ExecCfg.Plan.MaxReplanAttempts,
		MaxFanOutWidth:         s.ExecCfg.Plan.MaxFanOutWidth,
		MaxPlanCost:            s.ExecCfg.Plan.MaxPlanCost,
		DefaultInputCostPer1M:  s.Manager.DefaultInputCostPer1M,
		DefaultOutputCostPer1M: s.Manager.DefaultOutputCostPer1M,
		CurrentSessionID:       sessionID,
		CurrentRunID:           executionID,
		// Close the task when its plan finishes. Nothing else did: `completed`
		// was only ever set by the operator's explicit command, so finished work
		// stayed open indefinitely.
		SessionCloser:     s.SessionMgr,
		CheckpointStore:   s.executorCheckpointStore(),
		StepCachePolicies: s.ExecCfg.Plan.StepCachePolicies,
		EventBus:          s.EventBus, // ADR-0047 0047-17: PlanStateChanged → operator feed
	}

	// ADR-0047 0047-18: register this live execution's controls so the operator
	// PauseSession/ResumeSession commands can steer it; deregister on completion.
	if s.ControlHub != nil && sessionID != "" {
		s.ControlHub.Register(sessionID, executor)
		defer s.ControlHub.Deregister(sessionID)
	}

	masterCtx, err := executor.ExecuteFrom(planCtx, plan, initialCtx, executer.StepFunc(stepFn), startFromStep)

	if err != nil {
		var partialErr *executer.PartialPlanError
		if errors.As(err, &partialErr) {
			slog.Warn("⚠️ Partial plan failure", "failed_step", partialErr.FailedStep, "context_entries", len(partialErr.Context))
			partialErr.Context["_partial_plan"] = "true"
			return &pb.Handoff{
				Id:        in.Id,
				FromAgent: "orchestrator",
				ToAgent:   "user",
				Payload:   &pb.Object{Data: []byte(partialErr.Error())},
				Metadata:  partialErr.Context,
			}, nil
		}
		return nil, err
	}

	// Experiential memory removed: no procedural-memory (Hippocampus) plan write-back.

	finalResult := masterCtx[finalResultKey]
	delete(masterCtx, finalResultKey)
	if sessionID != "" {
		masterCtx["_session_id"] = string(sessionID)
	}

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

	// ADR-0048 D5: drill-down serves ONLY ContentStore CIDs (transient working-memory
	// / tool-result blobs). The former pgvector fallback returned raw LTM document
	// text via GetByID, bypassing the ScopedVectorStore that every other read goes
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

func (s *Server) replanHandler() executer.ReplanHandler {
	return awareness.NewPlannerReplanHandler(s.Planner)
}

// wrapGen applies the optional generator decorator (Langfuse tracing) so routed
// thought-step generations are observable. Identity when GenWrapper is nil.
func (s *Server) wrapGen(g domain.Generator) domain.Generator {
	if s.GenWrapper == nil {
		return g
	}
	return s.GenWrapper(g)
}

// useEFE reports whether this session's assigned variant is the EFE arm
// (ADR-0037). Returns false unless a selector is wired and the flag/traffic
// resolve to "efe" — so the default "auction" rollout never takes the new path.
func (s *Server) useEFE(sessionID domain.SessionID) bool {
	if s.ResourceSelector == nil {
		return false
	}
	return centralexec.AssignVariant(s.SelectorMode, s.EFETrafficPercent, string(sessionID)) == domain.MechanismEFE
}

// selectViaEFE binds a step via the Central-Executive selector and dispatches it
// through the Manager's CallAgent. It returns (resp, true) on success; on any
// selection or dispatch failure it returns (nil, false) so the caller falls
// through to the auction path (the EFE arm is never worse than the status quo).
func (s *Server) selectViaEFE(ctx context.Context, task *domain.AuctionTask, query string, h *domain.Handoff, winningAgentID *string) (*domain.Handoff, bool) {
	sel, err := s.ResourceSelector.Select(ctx, domain.Intent{
		ID:          task.ID,
		Description: query,
		// ROUTE-03: hand the selector the capability contract the caller already
		// resolved onto the task; without it the selector's rebuilt AuctionTask
		// reaches L1 with no requirements and the gate is a no-op for this arm.
		RequiredCapabilities: task.RequiredCapabilities,
		PreferredAgent:       task.PreferredAgent,
		AgentPin:             task.AgentPin,
	}, nil)
	if err != nil || sel.ResourceID == "" {
		slog.Warn("EFE selection failed; falling back to auction", "task", task.ID, "err", err)
		return nil, false
	}
	// Route through the YieldDriver so a yielded sub-goal (ADR-0037 D10) is bound,
	// dispatched, and the parent resumed; falls back to a plain call when unwired.
	var resp *domain.Handoff
	var callErr error
	if s.YieldDriver != nil {
		resp, callErr = s.YieldDriver.Drive(ctx, sel.ResourceID, h)
	} else {
		resp, callErr = s.Auctioneer.CallAgent(ctx, sel.ResourceID, h, "")
	}
	if callErr != nil {
		slog.Warn("EFE-bound agent call failed; falling back to auction", "agent", sel.ResourceID, "err", callErr)
		return nil, false
	}
	*winningAgentID = sel.ResourceID
	if h.Context == nil {
		h.Context = map[string]string{}
	}
	h.Context["_winning_confidence"] = fmt.Sprintf("%f", sel.Confidence)
	h.Context["_selection_mechanism"] = sel.Mechanism // A/B telemetry partition
	return resp, true
}

func (s *Server) thoughtFn(plan *domain.ExecutionPlan) executer.StepFunc {
	return func(ctx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		var prompt string
		if handoff.Payload != nil && len(handoff.Payload.Data) > 0 {
			// Checkpoint coherence probe: runCheckpoint passes its question via Payload.Data.
			prompt = string(handoff.Payload.Data)
		} else {
			var contextSummary strings.Builder
			// Phase 0 fallback: context map contains step_N_result keys.
			for k, v := range handoff.Context {
				if strings.HasPrefix(k, "step_") && strings.HasSuffix(k, "_result") {
					contextSummary.WriteString(fmt.Sprintf("- %s: %s\n", k, v))
				}
			}
			// ADR-0022 Phase 3: when UseGlobalWorkspace=true, Context is nil and
			// prior step outputs are carried as ContextRefs in WorkingMemory.
			for _, ref := range handoff.WorkingMemory {
				if ref.Snippet != "" {
					contextSummary.WriteString(fmt.Sprintf("- %s: %s\n", ref.CID, ref.Snippet))
				} else if s.ContentStore != nil {
					node, err := s.ContentStore.Get(ctx, ref.CID)
					if err == nil && node != nil {
						contextSummary.WriteString(fmt.Sprintf("- %s: %s\n", ref.CID, string(node.Data)))
					}
				}
			}
			prompt = fmt.Sprintf(
				"You are the Cambrian Reasoning engine. Synthesize:\nGoal: %q\nTask: %s\nResults:\n%s\nOutput ONLY synthesis.",
				plan.Subject, plan.Steps[i].Query, contextSummary.String(),
			)
		}

		var respStr string
		var genErr error

		// The model serving a thought step is the Provider's call, resolved from the
		// declared need — no per-step model name travels on the plan any more.
		respStr, genErr = s.Planner.Generate(ctx, prompt)

		if genErr != nil {
			return nil, genErr
		}

		return &domain.Handoff{
			Payload: &domain.Payload{Data: []byte(strings.TrimSpace(respStr))},
			Context: handoff.Context,
		}, nil
	}
}

// SignalStream is the proactive neural signal endpoint. It receives Handoff
// messages from Daemon Observer agents, validates the signal and auth token,
// enriches with LTM context, and triggers proactive planning (OSS Watcher) or
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
			// OSS path: Watcher validates, enriches with LTM, and presents to Planner.

			// 1. Validate signal
			if valErr := s.Watcher.ValidateSignal(ctx, dHandoff); valErr != nil {
				_ = s.Watcher.HandleInvalidSignal(ctx, dHandoff)
				continue
			}

			// 2. Validate auth token
			inst, tokErr := s.Watcher.ValidateToken(ctx)
			if tokErr != nil {
				if dHandoff.FromAgent != "" {
					_ = s.Watcher.HandleInvalidSignal(ctx, dHandoff)
				}
				continue
			}

			// 3. Enrich with LTM context
			signalType := dHandoff.Context["_signal_type"]
			signalData := ""
			if dHandoff.Payload != nil {
				signalData = string(dHandoff.Payload.Data)
			}
			ltmCtx := s.Watcher.EnrichSignal(ctx, signalType, signalData)

			// 4. Present to Planner
			plan, planErr := s.Watcher.ProcessSignal(ctx, signalType, signalData, ltmCtx)
			if planErr != nil {
				if dHandoff.FromAgent != "" {
					_ = s.Watcher.HandleInvalidSignal(ctx, dHandoff)
				}
				continue
			}

			// Valid signal resets circuit breaker
			s.Watcher.ResetInvalidSignals(dHandoff.FromAgent)

			// 5. Notify stream if a plan was produced
			if plan != nil && len(plan.Steps) > 0 {
				event := &pb.SymbiosisEvent{
					Payload: &pb.SymbiosisEvent_AgentLog{
						AgentLog: &pb.AgentLog{
							Timestamp: time.Now().Format(time.RFC3339),
							Level:     "INFO",
							Message:   fmt.Sprintf("Proactive plan generated from signal %s by %s (%s)", signalType, inst.AgentID, inst.ID),
							AgentId:   inst.AgentID,
						},
					},
				}
				if sendErr := stream.Send(event); sendErr != nil {
					return sendErr
				}
			}
		}

		// ADR-0032: ReactiveEngine (premium) evaluates conditions and executes actions.
		// OnSignal is always fire-and-forget (returns nil). In OSS, SignalReceiver is
		// a NoOpSignalReceiver that discards the signal.
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

// executeBypassAuction is the ADR-0050 D1 React-baseline path: the user's
// input is dispatched verbatim to execution.single_agent_id, skipping
// planner/auction/DAG. Everything downstream of agent selection is identical
// to a won auction step (CallAgent seam: priming, grants, scope, telemetry).
// The "/plan " slash-prefix is stripped for parity with the auction arm,
// where the InputRouter consumes it as a Layer-1 command, not task content.
func (s *Server) executeBypassAuction(ctx context.Context, in *pb.Handoff, rawInput string) (*pb.Handoff, error) {
	agentID := s.ExecCfg.Routing.SingleAgentID
	if agentID == "" {
		return nil, status.Error(codes.FailedPrecondition,
			"bypass_auction: execution.single_agent_id is required")
	}
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

	// Mirror stepFn's session-token acquisition so cognitive agents can call
	// GenerateViaModelStream on the bypass arm exactly as on the auction arm.
	if s.LLMGateway != nil {
		sa := domain.StepAllocation{}
		if s.ModelRouter != nil && s.ModelRouter.Ollama != nil {
			sa.Winner = domain.AgentDefinition{ID: "llm:ollama:qwen3:8b"}
		}
		tokenID, _ := s.LLMGateway.Acquire(ctx, sa, 4096, 30*time.Second)
		// The bypass arm runs outside a task session, so the binding carries only the
		// agent. Registering it still matters: a KNOWN-but-unbound lease is how the
		// transport tells "an agent sent its lease" from "the operator sent a session
		// ID", and answers the former with no session rather than with the lease ID.
		if r := s.leaseResolver(); r != nil {
			r.BindLease(tokenID, domain.LeaseBinding{StepIndex: 0, AgentID: agentID})
		}
		handoff.Context["_session_token_id"] = string(tokenID)
		defer func() { _, _ = s.LLMGateway.Complete(ctx, tokenID) }()
	}

	slog.Info("⚡ BYPASS AUCTION (ADR-0050 D1)", "agent", agentID)
	resp, err := s.Auctioneer.CallAgent(ctx, agentID, handoff, "")
	if err != nil {
		return nil, fmt.Errorf("bypass_auction: agent %q: %w", agentID, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("bypass_auction: agent %q returned no handoff", agentID)
	}
	if resp.FromAgent == "" {
		resp.FromAgent = agentID
	}
	return handoffToProto(resp), nil
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

		fbResp, fbErr := s.Auctioneer.CallAgent(ctx, runnerUp.Agent.ID, fallbackHandoff, excludeID)
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
// ADR-0049 D3 moved the step result onto RecordExecution (the `step_N:` fact) and
// mutations onto synchronous `mnemonic_action` records, so re-ingesting here produced
// a duplicate row. The former prioritised path went through IngestSync, which has since
// been removed along with the rest of the LLM-importance write path.
func (s *Server) handleMemoryBarrier(
	stepCtx context.Context,
	i int,
	resp *domain.Handoff,
	kernelSync, envMutation bool,
	links []string,
) {
	// ADR-0049 D3: the step result is already recorded by RecordExecution (the
	// `step_N:` fact) and any mutations as synchronous `mnemonic_action` records — so
	// the barrier no longer re-ingests a duplicate `Step N result:` row. Retained as a
	// barrier signal only (the sync-flush guarantee for mutations is met by the
	// synchronous action save). `resp`/`links`/`stepCtx` are now unused here.
	if kernelSync || envMutation {
		slog.Info("🛡️ Memory Barrier (step result already recorded; no duplicate ingest)",
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
