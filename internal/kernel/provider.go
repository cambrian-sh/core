// Package kernel is the composition root of the application.
// It is the ONLY package that knows how the domain subsystems wire together.
// No other package should import kernel.
package kernel

import (
	"github.com/cambrian-sh/core/internal/centralexec"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/router"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	subsynaptic "github.com/cambrian-sh/core/internal/substrate/synaptic"
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
	modelRouter *llm.ProviderRegistry,
	llmProvider *llm.Provider, // ADR-0042: availability authority; the router Acquires from it
	sessionMgr *session.SessionManager,
	eventLog *subsynaptic.EventLogger,
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
		mem.VecDB,
		mem.QueryService,
		mem.Hippocampus,
		meta.EnqueueVerification(),
		meta.Auctioneer,
		watcher,
		modelRouter,
		sessionMgr,
		eventLog,
		mem.WorkspaceStage,
		llmGateway,
		observer,
		contentStore,
	)

	// ADR-0025: SceneWriterFactory produces a fresh PgSceneWriter per Execute call.
	// The factory is nil-safe — if NewPgSceneWriter returns nil (no VecDB or embedder),
	// SceneWriter on the DAGExecutor remains nil and scene writing is silently disabled.
	srv.SceneWriterFactory = func() domain.SceneWriter {
		sw := mem.NewPgSceneWriter()
		if sw == nil {
			return nil // nil interface, not nil-concrete-in-interface
		}
		return sw
	}

	// ADR-0026: wire step-level memoization cache.
	srv.StepCache = stepCache

	// ADR-0042: the server resolves agent-step models through the Provider.
	srv.Provider = llmProvider

	// ADR-0031 + ADR-0042: wire the universal InputRouter on its own PurposeRouter
	// generator, Acquired from the Provider so Layer 3 classification gets live
	// health failover (this is the path the original silent-empty-response bug
	// travelled). Previously reused aw.LLM (the planner generator).
	srv.Router = router.NewWithConfig(
		llmProvider.GeneratorFor(domain.PurposeRouter),
		cfg.RouterMinClassificationConfidence,
		cfg.RouterClassificationBodyChars,
	)

	// ADR-0057: signal receiver + watch handler are injected (no build tags).
	srv.SignalReceiver = signalRcv
	srv.WatchHandler = watchHandler

	// ADR-0037: wire the Central-Executive selection arm behind the flag.
	// Default "auction" leaves the EFE selector nil — production dispatch is
	// unchanged until an operator opts in via resource_selector=efe|auto.
	srv.SelectorMode = cfg.ResourceSelector
	srv.EFETrafficPercent = cfg.EFETrafficPercent
	if cfg.ResourceSelector == "efe" || cfg.ResourceSelector == "auto" {
		srv.ResourceSelector = centralexec.NewGatekeeperEFESelector(meta.Gatekeeper, cfg.EFEExplorationBonus)
	}

	return srv
}
