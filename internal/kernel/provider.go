// Package kernel is the composition root of the application.
// It is the ONLY package that knows how the domain subsystems wire together.
// No other package should import kernel.
package kernel

import (
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/router"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
	"log/slog"
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
) *subnetwork.Server {
	srv := subnetwork.NewServer(
		aw.Planner,
		meta.Manager,
		mem.Agent,
		cfg,
		// VecDB (raw tier 0, AGENTS.md §2.2): the server's only use of this store is
		// writing agent neural traces (network.storeNeuralTrace). NO ADR permits it:
		// whether traces are memory (→ mem.WriteStore, classified like their
		// context) or telemetry (→ the run ledger / trace sink, store removed) is an
		// open owner decision on T-20261113-agent-neural-traces-full-prompt-response-are.
		// Do not add any other use of this store to the server.
		mem.VecDB,
		mem.QueryService,
		mem.Hippocampus,
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

	// Phase 3: the run store. The registry (AgentRepoDecorator) satisfies domain.RunStore;
	// the compile-time assertion in repository_decorator.go is what keeps this assertion
	// from silently going false the way the checkpoint one did.
	if cs, ok := meta.Manager.Registry.(executer.CheckpointStore); ok {
		srv.Checkpoints = cs
	} else {
		slog.Warn("Phase 3: registry does not implement CheckpointStore — checkpoints will not persist")
	}
	if rs, ok := meta.Manager.Registry.(domain.RunStore); ok {
		srv.Runs = rs
	} else {
		slog.Warn("Phase 3: registry does not implement domain.RunStore — runs will not persist and resume is unavailable")
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
	// is its ceiling (0 = metered, never refused). Wired here because this is the
	// one place that holds the Provider, the gateway and the server together; the
	// Provider resolves the meter per call, so organ generators created earlier by
	// GeneratorFor are covered.
	meter := llm.NewSpendMeter(cfg.Session.MaxSessionTokens, llmProvider.Ledger(), slog.Default())
	llmProvider.SetSpendMeter(meter)
	if gw, ok := llmGateway.(interface{ SetSpendMeter(*llm.SpendMeter) }); ok {
		gw.SetSpendMeter(meter)
	}
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
