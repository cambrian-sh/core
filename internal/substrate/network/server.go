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

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/awareness"
	"github.com/cambrian-sh/cambrian-runtime/internal/centralexec"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
	"github.com/cambrian-sh/cambrian-runtime/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/cambrian-runtime/internal/metabolism/executer"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/harness"
	session "github.com/cambrian-sh/cambrian-runtime/internal/substrate/session"
	subsynaptic "github.com/cambrian-sh/cambrian-runtime/internal/substrate/synaptic"
	supwatcher "github.com/cambrian-sh/cambrian-runtime/internal/supervision/watcher"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
// WorldScout performs bounded read-only pre-plan discovery (ADR-0051): it observes the
// current world state the request references and returns a DiscoveryReport the Planner
// shapes its plan to. nil ⇒ no Scout, Execute plans one-shot as before. An empty report
// (Scout found nothing / degraded) is likewise treated as one-shot. Satisfied by
// *AgentScoutDispatcher, which invokes the privileged Python Scout agent (ADR-0051 D1).
type WorldScout interface {
	Discover(ctx context.Context, userInput string) *domain.DiscoveryReport
}

type Server struct {
	pb.UnimplementedOrchestratorServer
	// Router is the universal input classifier (ADR-0031). When nil, Execute
	// falls back to the legacy PLAN-only path for backward compatibility.
	Router              domain.InputRouter
	Planner             Planner
	// Scout is the ADR-0051 pre-plan discovery organ. nil ⇒ one-shot planning.
	Scout               WorldScout
	Manager             *agentmgr.AgentManager
	Auctioneer          domain.Auctioneer
	MemoryAgent         domain.MemoryAgent
	ExecCfg             config.ExecutionConfig
	EnqueueVerification executer.EnqueueVerification
	Watcher             *supwatcher.Watcher

	VectorStore       domain.VectorStore
	MemorySearcher    domain.MemorySearcher
	Hippocampus       domain.ProceduralMemory
	ModelRouter       *llm.ProviderRegistry
	Provider          domain.LLMProvider // ADR-0042: agent-step model provisioning (health-guarded)
	SessionMgr        *session.SessionManager
	EventLog          *subsynaptic.EventLogger
	WorkspaceStage    domain.WorkspaceStage    // ADR-0016: may be nil
	LLMGateway        LLMGateway               // ADR-0018: may be nil; wired by kernel provider
	TelemetryObserver domain.TelemetryObserver // ADR-0019: may be nil
	ContentStore      domain.ContentStore      // ADR-0022 Phase 1: may be nil; nil disables CAS
	StepCache         domain.StepCache         // ADR-0026: may be nil; nil disables step-level memoization
	// SceneWriterFactory produces a fresh domain.SceneWriter for each Execute call.
	// ADR-0025: per-request because PgSceneWriter tracks lastSceneID for specifies edges.
	// nil = scene writing disabled.
	SceneWriterFactory func() domain.SceneWriter

	// AgentCallLogger records agent-initiated LLM calls (GenerateViaModelStream) to an
	// external observability backend. nil = disabled (no-op). OBSERVABILITYREQ REQ1.
	AgentCallLogger AgentCallLogger

	// GenWrapper decorates a raw generator with cross-cutting concerns (Langfuse
	// tracing) so thought/synthesis steps that route to a RecommendedModel are
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

	// WatchHandler provides the 4 WatchConfig CRUD RPCs. Injected by the premium
	// binary via the app.Options reactive hook; nil in OSS — RPC shells guard against
	// nil and return Unimplemented. ADR-0032 / ADR-0057.
	WatchHandler domain.WatchConfigHandler

	// ADR-0034 / REQ-SDK-007c: scope-enforced artifact storage. All nil → the
	// artifact RPCs return Unimplemented.
	ArtifactBytes    ArtifactByteStore     // CAS byte store (ArtifactVault)
	ArtifactMeta     ArtifactMetaStore     // metadata + tags persistence
	ArtifactScopes   ArtifactScopeResolver // effective-scope resolution for access decisions
	ArtifactSessions ArtifactSessionScopes // ADR-0034 Phase 2: session caller_scope (may be nil)
	ArtifactVocab    *scope.Vocabulary     // controlled classification vocabulary

	// MemoryWriter backs memory.remember() / IngestMemory (ADR-0035 C2). nil →
	// IngestMemory returns Unimplemented.
	MemoryWriter MemoryWriter

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

	// ADR-0046: the system-skill plane backing ListSkills. SkillRegistry holds the
	// discovered SKILL.md skills; SkillRetriever ranks them by relevance within the
	// agent's effective scope; SkillScope resolves that scope for gating. All nil →
	// ListSkills returns an empty menu (agents simply see no system skills).
	SkillRegistry  domain.SkillRegistry
	SkillRetriever domain.SkillRetriever
	SkillScope     domain.ToolScopeResolver

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
	eventLog *subsynaptic.EventLogger,
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
		EventLog:            eventLog,
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

	// ADR-0031: Route through InputRouter when configured.
	// The Router classifies raw input before any enrichment (mood, LTM, etc.).
	if s.Router != nil {
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

	var sessionID string
	if s.SessionMgr != nil {
		ses := loadOrCreateSession(ctx, s.SessionMgr, userInput)
		if ses != nil {
			sessionID = ses.ID
		}
	}

	// ADR-0025: FetchContext retired from planning path.
	// LTM enrichment now handled exclusively by WorkspaceStage.PrimeForPlanning
	// inside the Planner, injecting typed <FactLTM> and <NegativeLTM> sections.

	// Mood Injection: append last 3 SessionEvents as social context.
	if sessionID != "" {
		userInput = injectMoodContext(ctx, s, sessionID, userInput)
	}

	// ADR-0051: pre-plan discovery. Scout observes the world state the request references
	// and attaches its report to ctx; the Planner (via DiscoveryFromContext) shapes the plan
	// to it. An empty report (no Scout, nothing to observe, or any degrade) leaves ctx
	// untouched ⇒ one-shot planning exactly as before.
	if s.Scout != nil {
		if report := s.Scout.Discover(ctx, userInput); !report.IsEmpty() {
			ctx = domain.WithDiscovery(ctx, report)
		}
	}

	plan, err := planWithValidation(ctx, s.Planner, userInput, nil)
	if err != nil {
		return nil, err
	}

	// Logging plan summary
	slog.Info("📜 STRATEGIC PLAN", "subject", plan.Subject, "steps", len(plan.Steps))

	initialCtx := make(map[string]string)
	for k, v := range in.GetMetadata() {
		initialCtx[k] = v
	}
	initialCtx["original_prompt"] = userInput

	// Resume: hydrate checkpoint context for existing sessions.
	var startFromStep int
	if sessionID != "" && s.SessionMgr != nil {
		if cp, _, nextIdx, err := s.HydrateSession(ctx, sessionID); err == nil && cp != nil {
			for k, v := range cp {
				initialCtx[k] = v
			}
			startFromStep = nextIdx
		}
	}

	// executionID scopes all neural traces produced by this Execute call.
	executionID := newPlanID()

	// Memory Ingestion tracking for DAG Lineage
	var ingestedIDsMu sync.Mutex
	var ingestedIDs []string

	// Confidence accumulator
	var confMu sync.Mutex
	var confValues []float64

	stepFn := func(stepCtx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		step := plan.Steps[i]
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
			handoff.Context["_session_token_id"] = tokenID
			defer func() { _, _ = s.LLMGateway.Complete(stepCtx, tokenID) }()
		}

		// healingOccurred is set true by innerFn when SelfHealer injects _heal_attempt,
		// signalling that at least one retry was consumed. The Memory Barrier check
		// below uses it to force IngestSync on any healed step.
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
					lastOut := ""
					if healErr.LastError != nil {
						lastOut = healErr.LastError.Error()
					}
					_ = s.MemoryAgent.IngestNegativeEdge(stepCtx, healErr.LastError.Error(), lastOut, fmt.Sprintf("step-%d", i))
				}

				// Inter-step fallback: try runner-up candidates when winner fails.
				if s.ExecCfg.FallbackEnabled && len(runnerUps) > 0 {
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
			storeNeuralTrace(ctx, s.VectorStore, trace, newPlanID(), executionID, i, healAttempt, resp.FromAgent)
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

	planCtx, cancelPlan := context.WithTimeout(ctx, time.Duration(s.ExecCfg.PlanTimeoutMs)*time.Millisecond)
	defer cancelPlan()

	var eventWriter executer.TaskEventWriter
	if ew, ok := s.Manager.Registry.(executer.TaskEventWriter); ok {
		eventWriter = ew
	}

	var sceneWriter domain.SceneWriter
	if s.SceneWriterFactory != nil {
		sceneWriter = s.SceneWriterFactory()
	}
	executor := &executer.DAGExecutor{
		EventWriter:            eventWriter,
		EnqueueVerification:    s.EnqueueVerification,
		MemoryRecorder:         s.MemoryAgent,
		WorkspaceStage:         s.WorkspaceStage,
		ArtifactLister:         s.ArtifactMeta,     // ADR-0034: surface prior-step artifacts (scope-filtered)
		SessionScopes:          s.ArtifactSessions, // ADR-0034 Phase 2: caller_scope filter (may be nil)
		LLMGateway:             s.LLMGateway,
		Observer:               s.TelemetryObserver,
		ContentStore:           s.ContentStore,
		StepCache:              s.StepCache,
		SceneWriter:            sceneWriter,
		UseGlobalWorkspace:     s.ExecCfg.UseGlobalWorkspace,
		MaxContextSlots:        s.ExecCfg.MaxContextSlots,
		ContextRefSnippetChars: s.ExecCfg.ContextRefSnippetChars,
		ThoughtFn:              executer.StepFunc(s.thoughtFn(plan)),
		CheckpointValidator:    awareness.NewLLMCheckpointValidator(s.Planner),
		ReplanHandler:          s.replanHandler(),
		MaxReplanAttempts:      s.ExecCfg.MaxReplanAttempts,
		MaxPlanCost:            s.ExecCfg.MaxPlanCost,
		DefaultInputCostPer1M:  s.Manager.DefaultInputCostPer1M,
		DefaultOutputCostPer1M: s.Manager.DefaultOutputCostPer1M,
		CurrentSessionID:       sessionID,
		CheckpointStore:        s.executorCheckpointStore(),
		StepCachePolicies:      s.ExecCfg.StepCachePolicies,
		EventBus:               s.EventBus, // ADR-0047 0047-17: PlanStateChanged → operator feed
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

	// Proprioceptive confidence recording
	if s.Hippocampus != nil && len(confValues) > 0 {
		mean := meanConfidence(confValues)
		_ = s.Hippocampus.Store(ctx, plan, mean)
	}

	finalResult := masterCtx[finalResultKey]
	delete(masterCtx, finalResultKey)
	if sessionID != "" {
		masterCtx["_session_id"] = sessionID
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

	// ADR-0048 D4: thread the caller's session from gRPC metadata into ctx so the
	// read-gate below can identify the caller against the node's owner.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-session-id"); len(vals) > 0 && vals[0] != "" {
			ctx = domain.WithSessionID(ctx, vals[0])
		}
	}

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
	// ADR-0048 D4: take the owning session from authenticated gRPC metadata (never
	// the payload) and thread it into ctx so the ContentStore stamps it as owner.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-session-id"); len(vals) > 0 && vals[0] != "" {
			ctx = domain.WithSessionID(ctx, vals[0])
		}
	}
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
func (s *Server) useEFE(sessionID string) bool {
	if s.ResourceSelector == nil {
		return false
	}
	return centralexec.AssignVariant(s.SelectorMode, s.EFETrafficPercent, sessionID) == domain.MechanismEFE
}

// selectViaEFE binds a step via the Central-Executive selector and dispatches it
// through the Manager's CallAgent. It returns (resp, true) on success; on any
// selection or dispatch failure it returns (nil, false) so the caller falls
// through to the auction path (the EFE arm is never worse than the status quo).
func (s *Server) selectViaEFE(ctx context.Context, task *domain.AuctionTask, query string, h *domain.Handoff, winningAgentID *string) (*domain.Handoff, bool) {
	sel, err := s.ResourceSelector.Select(ctx, domain.Intent{ID: task.ID, Description: query}, nil)
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

		if recommended := plan.Steps[i].RecommendedModel; recommended != "" && s.Provider != nil {
			// ADR-0042: the step's recommended model is a prior; the Provider makes
			// the final, health-guarded choice (failing over if it is unhealthy).
			// Auction agent IDs are "llm:<id>"; strip the prefix to the generator id.
			gen, acqErr := s.Provider.Acquire(ctx, domain.LLMRequest{
				Purpose:          domain.PurposeAgentStep,
				SuggestedModelID: strings.TrimPrefix(recommended, "llm:"),
			})
			if acqErr != nil {
				respStr, genErr = s.Planner.Generate(ctx, prompt)
			} else {
				respStr, genErr = s.wrapGen(gen).Generate(ctx, prompt)
			}
		} else {
			respStr, genErr = s.Planner.Generate(ctx, prompt)
		}

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

func loadOrCreateSession(ctx context.Context, mgr *session.SessionManager, goal string) *domain.Session {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		ids := md.Get("x-session-id")
		if len(ids) > 0 && ids[0] != "" {
			ses, err := mgr.GetSession(ctx, ids[0])
			if err == nil && ses != nil && ses.Status != domain.SessionCompleted {
				return ses
			}
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
	threshold := s.ExecCfg.FallbackConfidenceThreshold * winnerConf
	if threshold == 0 {
		threshold = s.ExecCfg.FallbackConfidenceThreshold
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

// handleMemoryBarrier ingests the step result into LTM. If kernelSync or
// envMutation is set, ingestion is prioritised via a goroutine-isolated
// IngestSync; otherwise the async path is used.
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
func injectMoodContext(ctx context.Context, s *Server, sessionID, userInput string) string {
	// Plan Drift: warn if session was completed long ago.
	if s.SessionMgr != nil {
		ses, err := s.SessionMgr.GetSession(ctx, sessionID)
		if err == nil && ses != nil && !ses.CompletedAt.IsZero() {
			days := int(time.Since(ses.CompletedAt).Hours() / 24)
			if days >= s.ExecCfg.PlanDriftDays && s.ExecCfg.PlanDriftDays > 0 {
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
