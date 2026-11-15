// Package kernel is the composition root of the application.
// It is the ONLY package that knows how the domain subsystems wire together.
// No other package should import kernel.
package kernel

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/router"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
)

// ProvideServer assembles the gRPC server from the four domain stacks.
// It is the final assembly step — no business logic lives here.
func ProvideServer(
	cfg config.ExecutionConfig,
	mem *MemoryStack,
	aw *AwarenessStack,
	meta *MetabolismStack,
	watcher *supwatcher.Watcher,
	llmProvider *llm.Provider, // ADR-0042: availability authority; the router Acquires from it
	sessionMgr *session.SessionManager,
	llmGateway subnetwork.LLMGateway,
	observer domain.TelemetryObserver,
	contentStore domain.ContentStore, // ADR-0022 Phase 1
	stepCache domain.StepCache, // ADR-0026: may be nil; nil disables step-level memoization
	// ADR-0057 (Model C): the signal receiver + watch handler are injected by the
	// composition root. OSS passes the Watcher (+ nil handler); the premium binary
	// passes a ReactiveEngine built via the app.Options hook. No build tags.
	signalRcv domain.SignalReceiver,
	watchHandler domain.WatchConfigHandler,
	// Phase 3/4: run persistence, built explicitly by the composition root (the
	// Postgres session store, or the bbolt registry when Postgres sessions are
	// unavailable) and passed as typed values — they used to be discovered by
	// asserting the AGENT REGISTRY, an assertion that once silently went false
	// (ENG-1504).
	runs domain.RunStore,
	checkpoints executer.CheckpointStore,
	// ADR-0131 D1-D3: the ONE spend ledger. The composition root built the gateway
	// with this same meter (subnetwork.NewLLMGateway requires it); here it is
	// attached to the Provider (organ calls) and the server.
	meter *llm.SpendMeter,
) *subnetwork.Server {
	srv := subnetwork.NewServer(
		aw.Planner,
		meta.Manager,
		mem.Agent,
		cfg,
		mem.QueryService,
		meta.EnqueueVerification(),
		// ADR-0100: the DAG executor selects through meta.Selector (the
		// capability-typed Dispatcher; the auction is deleted). Privileged organs keep calling
		// meta.AgentTransport directly — they bypass selection by construction.
		meta.Selector,
		watcher,
		sessionMgr,
		mem.WorkspaceStage,
		llmGateway,
		observer,
		contentStore,
	)

	// Phase 3/4: the run and checkpoint stores are constructor arguments (see above).
	// A nil one is a composition bug that would silently disable persistence and
	// resume, so it is reported at Error at boot rather than discovered at first use.
	srv.Runs = runs
	srv.Checkpoints = checkpoints
	if runs == nil {
		slog.Error("Phase 3: no RunStore was composed — runs will not persist and resume is unavailable")
	}
	if checkpoints == nil {
		slog.Error("Phase 3: no CheckpointStore was composed — checkpoints will not persist")
	}

	// No scene writer of any kind is wired: the per-step SceneWriter plumbing was
	// removed 2026-10-31 (T-20261031-per-step-scenewriter-plumbing-is-wired-into) and
	// the plan-wide scene on 2026-11-05 (decisions/scenes-cancelled.md).

	// ADR-0026: wire step-level memoization cache.
	srv.StepCache = stepCache

	// ADR-0139 C1: thought steps go to their OWN purpose, `thought`, on the
	// structured request path — so pre_model/post_model hooks, spend records and
	// traces label a kernel-synthesized answer as a deliverable, not a plan
	// (T-20261106-thought-steps-call-the-model-under-the-planner). Routing is
	// unchanged: with no `thought` role configured the Provider resolves it to the
	// planner's generator (llm.RoleFallbacks).
	srv.ThoughtLLM = llmProvider.GeneratorFor(domain.PurposeThought)

	// ADR-0042: the server resolves agent-step models through the Provider.
	srv.Provider = llmProvider

	// ADR-0131 D1-D3: ONE spend ledger, shared by the two doors every LLM call
	// passes — Provider.Acquire (every organ) and the gateway (every agent stream,
	// including the retrieval_agent's chain). execution.session.max_session_tokens
	// is its ceiling (0 = metered, never refused). The gateway received it at
	// construction (it cannot be built without one, ENG-1504: the anonymous
	// SetSpendMeter probe that could silently skip the ceiling is gone); the Provider
	// resolves the meter per call, so organ generators created earlier by
	// GeneratorFor are covered.
	if meter == nil {
		slog.Error("ADR-0131: ProvideServer got no spend meter: organ LLM calls run WITHOUT the session spend ceiling",
			"max_session_tokens", cfg.Session.MaxSessionTokens)
	}
	llmProvider.SetSpendMeter(meter)
	srv.SpendMeter = meter

	// ADR-0031 + ADR-0042: wire the universal InputRouter on its own PurposeRouter
	// generator, Acquired from the Provider so Layer 3 classification gets live
	// health failover (this is the path the original silent-empty-response bug
	// travelled). Previously reused aw.LLM (the planner generator).
	srv.Router = router.NewWithConfig(
		llmProvider.GeneratorFor(domain.PurposeRouter),
		cfg.Router.RouterMinClassificationConfidence,
		cfg.Router.RouterClassificationBodyChars,
	)

	// ADR-0057: signal receiver + watch handler are injected (no build tags).
	srv.SignalReceiver = signalRcv
	srv.WatchHandler = watchHandler

	return srv
}
