package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentplane"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/metabolism/interview"
	supgk "github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	"github.com/cambrian-sh/core/internal/supervision/verify"

	"golang.org/x/sync/errgroup"
)

// MetabolismStack is the agent lifecycle + auction layer. It owns process
// management, candidate discovery, bidding, interviewing, and output verification.
//
// Biologically: this is cellular metabolism — energy management, replication,
// and quality control.
type MetabolismStack struct {
	rootCtx    context.Context // set by Start; scopes EnqueueVerification closures
	Manager    *agentmgr.AgentManager
	Gatekeeper *supgk.Gatekeeper
	// AgentTransport owns the agent connection pool and CallAgent. Privileged
	// system organs (kg_extractor, docling, reranker) call agents through it
	// DIRECTLY, bypassing selection entirely — they are kernel infrastructure,
	// not candidates. Named Auctioneer until ADR-0100 P3; it never selected
	// anything, so the old name claimed a responsibility it did not have.
	AgentTransport *agentplane.Transport
	// Selector is the step-selection mechanism the DAG executor uses. Since
	// ADR-0100 P3 there is exactly one: the capability-typed Dispatcher. The
	// port stays an interface so a future mechanism has somewhere to land, not
	// because a second implementation exists.
	Selector domain.StepDispatcher
	// Dispatcher is the concrete Selector, always non-nil. Exposed ONLY so the
	// composition root can finish wiring it — the EventBus is built after this
	// stack, and a dispatcher that cannot emit SelectionEventPayload is
	// invisible to the orchestration suite.
	Dispatcher         *dispatch.Dispatcher
	InterviewWorker    *interview.InterviewWorker
	VerificationWorker *verify.VerificationWorker
	VerifierPool       *verify.VerifierPool
	interviewRunner    *scenarioRunner // nil unless the graded interview is wired
}

// NewMetabolismStack wires the full agent lifecycle.
//
// Parameters:
//   - reg: the domain decorator (implements AgentRegistry, AgentUpdater, TaskEventReadWriter)
//   - llm: embedder for semantic gate and interview scenarios
//   - vec: the vector store (for InterviewSearcher)
//   - profileStore: agent cognitive fingerprints (from MemoryStack)
//   - memoryAgent: for the Memory Barrier in AgentManager
//   - cfg: execution + metabolism parameters
func NewMetabolismStack(
	reg *AgentRepoDecorator,
	embedder domain.Embedder,
	vec domain.VectorStore,
	profileStore memstore.ProfileStore,
	memoryAgent domain.MemoryAgent,
	cfg *config.Config,
	observer domain.TelemetryObserver,
	interviewGen domain.Generator,
) *MetabolismStack {
	manager := agentmgr.NewAgentManager(reg, cfg.Metabolism.PythonExecutable, "localhost:"+cfg.Server.Port, memoryAgent)
	// SEC-01: spawned agents get a deny-by-default environment (OS essentials +
	// the operator's non-secret passthrough); the kernel's API keys never leak.
	manager.SetEnvPassthrough(cfg.Execution.Agents.AgentEnvPassthrough)
	// Config-derived agent env: values the kernel SETS from config rather than
	// passing through from its own environment — a knob must never depend on
	// .env (owner rule 2026-08-11). 0 ⇒ the SDK's built-in default, no var set.
	if k := cfg.Execution.Tools.ToolMenuK; k > 0 {
		manager.SetEnvExtras([]string{fmt.Sprintf("CAMBRIAN_TOOL_MENU_K=%d", k)})
	}
	// SEC-01: cap agent memory (0 = disabled) so a runaway agent is killed at its
	// cap instead of OOMing the kernel host.
	manager.SetAgentMemoryLimitMB(cfg.Execution.Agents.AgentMemoryLimitMB)
	// Wire default model pricing for token cost estimation. ADR-0042: the default
	// generator's cost is the single source of truth (was cfg.Models[0]).
	if def := cfg.LLMProvider.DefaultGenerator(); def != nil {
		manager.DefaultInputCostPer1M = def.CostPer1MInput
		manager.DefaultOutputCostPer1M = def.CostPer1MOutput
	}

	interviewSearcher := interview.NewInterviewSearcher(vec)
	iWorker := interview.NewInterviewWorker(reg, embedder, profileStore, reg)

	gatekeeper := supgk.NewGatekeeper(reg, cfg.Execution,
		supgk.WithProfiles(profileStore),
		supgk.WithEmbedder(embedder),
		supgk.WithSearcher(interviewSearcher),
	)

	transport := agentplane.New(manager)

	iWorker.Requester = transport
	iWorker.EventWriter = reg

	vPool := verify.NewVerifierPool(reg, profileStore, cfg.Execution.Verification.VerifierPoolThreshold, cfg.Execution.Verification.VerifierRecencyWindow)
	vWorker := verify.NewVerificationWorker(vPool, transport, reg, profileStore, profileStore, embedder, verify.VerificationWorkerConfig{
		TrustBoostThreshold:   cfg.Execution.Verification.TrustBoostThreshold,
		QueueCapacity:         cfg.Execution.Verification.VerificationQueueCapacity,
		CrossVerifyRate:       cfg.Execution.Verification.CrossVerifyRate,
		VerifierRecencyWindow: cfg.Execution.Verification.VerifierRecencyWindow,
	})

	// ADR-0037 interview grading: an LLM generates questions, the agent answers
	// them for real (ScenarioRunner over CallAgent), and answers are graded by the
	// HybridJudge — the agent VerifierPool when it can serve, else an inline kernel
	// LLM judge (bootstrap-safe). The mean grade seeds the agent's cold-start
	// routing prior. Wired only when an LLM generator is available.
	var iRunner *scenarioRunner
	// DisableInterviews (benchmark/eval-only) skips the graded LLM interview so
	// the planner is not starved for LLM throughput; agents keep the neutral
	// cold-start prior. Manifest capabilities (L1 + planner vocab) are unaffected.
	if interviewGen != nil && !cfg.Execution.Agents.DisableInterviews {
		iRunner = &scenarioRunner{caller: transport}
		iWorker.Examiner = &interview.Examiner{
			Questions: interview.LLMQuestionGenerator{Gen: interviewGen},
			Runner:    iRunner,
			Judge: interview.HybridJudge{
				Pool:   poolJudge{pool: vPool, requester: transport},
				Inline: &interview.InlineLLMJudge{Gen: interviewGen},
			},
		}
	}

	// ADR-0100: capability-typed dispatch is THE selection mechanism. The auction
	// and the EFE selector are gone; the agent transport is still constructed because
	// it owns the connection pool CallAgent needs and the privileged organs call
	// through it, but it never selected anything.
	dispatcher := &dispatch.Dispatcher{
		Gatekeeper:      gatekeeper,
		Caller:          transport,
		Manifests:       manager,
		Profiles:        profileStore,
		Boots:           manager,
		ExecCfg:         cfg.Execution,
		Observer:        observer,
		ExplorationRate: cfg.Execution.Routing.ExplorationRate,
	}
	var selector domain.StepDispatcher = dispatcher
	slog.Info("ADR-0100: selection mechanism = DISPATCH (capability-typed; zero RPCs, only the winner is booted)",
		"capability_contract", cfg.Execution.Routing.CapabilityContract,
		"capability_resolution", cfg.Execution.Capability.CapabilityResolution,
		"cheap_energy_max", cfg.Execution.Routing.DispatchCheapEnergyMax)
	return &MetabolismStack{
		Manager:            manager,
		Gatekeeper:         gatekeeper,
		AgentTransport:     transport,
		Selector:           selector,
		Dispatcher:         dispatcher,
		InterviewWorker:    iWorker,
		VerificationWorker: vWorker,
		VerifierPool:       vPool,
		interviewRunner:    iRunner,
	}
}

// SetInterviewSession injects the managed-LLM-session minter the graded interview
// needs to execute scenarios against agents (ADR-0018). The LLM gateway is built
// after this stack at the composition root, so it is wired in post-construction.
// Without it, an interview scenario's CallAgent would carry no session token and
// the agent's generate() would be rejected with UNAUTHENTICATED.
func (s *MetabolismStack) SetInterviewSession(gw interviewSessionGateway, primaryModelID string, eval domain.EvaluationSessionMarker) {
	if s.interviewRunner == nil {
		return
	}
	s.interviewRunner.gw = gw
	s.interviewRunner.primaryModelID = primaryModelID
	s.interviewRunner.eval = eval
}

// Start launches InterviewWorker and VerificationWorker background goroutines.
func (s *MetabolismStack) Start(ctx context.Context) error {
	s.rootCtx = ctx
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.InterviewWorker.Start(gCtx) })
	g.Go(func() error { return s.VerificationWorker.Start(gCtx) })
	return g.Wait()
}

// Shutdown kills all active agents and stops background workers.
//
// Daemons go FIRST, and through StopDaemon rather than a bare kill. StopDaemon marks the
// exit as expected, so REACT-04's crash watcher ignores it; killing them as ordinary
// instances instead looks like a crash, and the supervisor helpfully respawns a daemon
// moments before the kernel dies — which is exactly how a restart used to strand one.
// A stranded poller then holds its integration's token and answers the replacement with
// 409 indefinitely.
func (s *MetabolismStack) Shutdown(ctx context.Context) {
	s.Manager.StopAllDaemons()
	if err := s.Manager.KillAllAgents(ctx); err != nil {
		slog.Warn("MetabolismStack: agent cleanup failed", "err", err)
	}
	slog.Info("🔥 MetabolismStack: shutdown complete")
}

// EnqueueVerification returns the closure DAGExecutor uses to enqueue
// verification requests. It is a method so the caller does not reach into
// MetabolismStack's internals.
func (s *MetabolismStack) EnqueueVerification() executer.EnqueueVerification {
	return func(taskID, agentID string, req, resp *domain.Handoff) {
		ctx, cancel := context.WithTimeout(s.rootCtx, 5*time.Second)
		defer cancel()
		s.VerificationWorker.Enqueue(ctx, taskID, agentID, req, resp)
	}
}

// InterviewEnqueuer returns the callback that storage uses to enqueue
// newly-registered agents for interviewing.
func (s *MetabolismStack) InterviewEnqueuer() func(domain.AgentDefinition) {
	return s.InterviewWorker.Enqueue
}

// interviewSessionGateway is the managed-LLM-session minter the interview needs
// (a narrow view of SubstrateLLMGateway). Acquire issues a session token bound to
// a model StepAllocation; Complete frees it. Mirrors the per-step minting the
// Server does for normal dispatch (server.go).
type interviewSessionGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error)
	Complete(ctx context.Context, leaseID domain.LeaseID) (llm.TokenUsage, error)
}

// scenarioRunner adapts the agent transport's CallAgent to interview.ScenarioRunner:
// it dispatches an interview question as a real task to the agent and returns the
// answer (the gradeable capability signal). Because the agent will call the
// budgeted GenerateViaModelStream, the runner first mints a managed LLM session
// and injects its token into the handoff (the same _session_token_id the Server
// stamps for normal steps) — without it the agent's generate() is rejected with
// UNAUTHENTICATED and every interview scores 0.
type scenarioRunner struct {
	caller         domain.AgentCaller
	gw             interviewSessionGateway // nil until SetInterviewSession; nil ⇒ no token (agent generate will fail)
	primaryModelID string
	// eval flags the minted session as a sandboxed evaluation so the ToolExecutor
	// auto-approves dangerous tools during the interview (no operator is present).
	// nil ⇒ interview tool calls still hit the operator approval gate.
	eval domain.EvaluationSessionMarker
}

func (r *scenarioRunner) RunScenario(ctx context.Context, agent domain.AgentDefinition, question string, deadline time.Time) (string, int, error) {
	cctx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		cctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	h := &domain.Handoff{
		ToAgent: agent.ID,
		Payload: &domain.Payload{Type: "text", Data: []byte(question)},
		Context: map[string]string{"_step_index": "0", "task_id": agent.ID + "-interview"},
	}
	if r.gw != nil {
		sa := domain.StepAllocation{Winner: domain.AgentDefinition{ID: r.primaryModelID}}
		// Generous TTL so a slow/uncapped interview LLM call is not evicted mid-stream
		// (the session is freed promptly by the deferred Complete on return anyway).
		if tokenID, err := r.gw.Acquire(cctx, sa, 4096, 1*time.Hour); err == nil {
			h.Context["_session_token_id"] = string(tokenID)
			// Flag the session as a sandboxed evaluation so dangerous tools the
			// agent calls during this scenario auto-approve (no operator is present
			// in an unattended interview). Unmarked on completion alongside Complete.
			if r.eval != nil {
				r.eval.Mark(tokenID)
				defer r.eval.Unmark(tokenID)
			}
			defer func() { _, _ = r.gw.Complete(context.Background(), tokenID) }()
		}
	}
	start := time.Now()
	resp, err := r.caller.CallAgent(cctx, agent.ID, h, "")
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		return "", latency, err
	}
	answer := ""
	if resp != nil && resp.Payload != nil {
		answer = string(resp.Payload.Data)
	}
	return answer, latency, nil
}

// poolJudge adapts the VerifierPool + VerifyRequester to interview.PoolJudge.
// It picks an eligible verifier and asks it to grade the interview answer. When
// no verifier is eligible (the bootstrap case) Select returns an error and the
// HybridJudge falls back to its inline kernel judge.
type poolJudge struct {
	pool      *verify.VerifierPool
	requester domain.VerifyRequester
}

func (p poolJudge) GradeViaPool(ctx context.Context, agentID, question, answer string) (domain.VerifyResponse, error) {
	task := &domain.DispatchTask{ID: agentID + "-interview", Description: question}
	verifier, err := p.pool.Select(ctx, task, agentID, nil)
	if err != nil {
		return domain.VerifyResponse{}, err
	}
	return p.requester.VerifyOutput(ctx, *verifier, domain.VerifyRequest{
		TaskID:        task.ID,
		OriginalQuery: question,
		WinnerOutput:  answer,
		WinnerAgentID: agentID,
	})
}
