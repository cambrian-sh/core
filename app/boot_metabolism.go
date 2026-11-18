package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/routescorer"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
)

// buildMetabolism builds the awareness, metabolism and supervision stacks with
// the kernel calendar, the ADR-0131 spend meter and the LLM gateway, the managed
// Python env, the storage callbacks, the event bus and the agent transport.
func (b *kernelBuild) buildMetabolism() error {
	ctx, cfg := b.ctx, b.cfg
	reg, mem, storeHandle, llmProvider, embedder, observer := b.reg, b.mem, b.storeHandle, b.llmProvider, b.embedder, b.observer
	awarenessGen, supervisionGen := b.awarenessGen, b.supervisionGen
	aw := kernel.NewAwarenessStack(awarenessGen, reg, mem.WorkspaceStage)
	// The kernel's ONE "today" (domain.Calendar), in the organisation's timezone: the
	// planner, the plan executor (stamped on every step), replans, thought steps and
	// the chat turn handoff all date their prompts with this instance
	// (T-20261107-planner-and-chat-agent-are-not-told-today-s).
	calendar := domain.NewCalendar(organisationLocation(cfg), nil)
	aw.Planner.SetCalendar(calendar)
	// ADR-0018: the LLM Gateway is constructed HERE, before the metabolism stack,
	// so the verification worker receives it as a constructor argument and mints
	// a lease for every verifier call (ADR-0131;
	// T-20261101-verification-mints-no-lease-so-verifier-llm). It needs only
	// cfg and the ADR-0131 spend meter to exist; its streaming clients and observer
	// are attached further down, before any lease is used.
	//
	// ADR-0131 D1-D3: the ONE spend ledger (ceiling execution.session.max_session_tokens,
	// 0 = metered, never refused). It is a constructor argument of the gateway (ENG-1504):
	// the gateway cannot exist without it, so the session ceiling can no longer be
	// skipped by a setter probe that stopped matching. ProvideServer attaches the same
	// meter to the Provider and the server.
	spendMeter := llm.NewSpendMeter(cfg.Execution.Session.MaxSessionTokens, llmProvider.Ledger(), slog.Default())
	llmGateway := subnetwork.NewLLMGateway(cfg.Execution, spendMeter)
	meta := kernel.NewMetabolismStack(reg, embedder, mem.Reader(), mem.ProfileStore, mem.Agent, cfg, observer, llmGateway)
	// ADR-0157 D7.11: the managed Python env's boot check (+ build_on_boot
	// background build) before any agent boots; Python spawns wait on its gate.
	pyEnv := startPythonEnv(ctx, cfg, config.ResolveBaseDir())
	meta.Manager.SetPythonEnv(pyEnv)
	sup := kernel.NewSupervisionStack(reg, mem.ProfileStore, supervisionGen, cfg, observer)

	// 3. Wire storage callback so newly-registered agents are enqueued for profile
	// seeding (deterministic description vector + neutral prior; the ADR-0037 LLM
	// interview was deleted 2026-11-05, ADR-0139 Q3).
	storeHandle.WireProfileSeedEnqueuer(meta.ProfileSeedEnqueuer())
	// ADR-0139 D4: every remote A2A card read — boot registration and the
	// seeder's description vector — goes through the verifying fetcher
	// (a2a.signature_policy). app/agentcard_endpoint.go.
	remoteCards, err := newRemoteCardFetcher(cfg)
	if err != nil {
		return err
	}
	storeHandle.WireA2ACardFetcher(remoteCards)
	meta.ProfileSeeder.CardFetcher = remoteCards

	// 3c. Wire EventBus (ADR-0030). Both the ProfileSeeder and the Dispatcher publish to it;
	// other subsystems subscribe. A log handler makes AgentReadyEvent observable in
	// production without a dedicated subscriber (ADR-0023 D6A observability).
	eventBus := domain.NewInMemoryEventBus()
	// ADR-0047 0047-14: the LLM provider's circuit breaker publishes LLMHealthEvent
	// on an open↔closed transition for the operator feed.
	llmProvider.SetHealthEventBus(eventBus)
	eventBus.Subscribe(domain.EventTypeAgentReady, func(_ context.Context, e domain.DomainEvent) {
		if ev, ok := e.(domain.AgentReadyEvent); ok {
			slog.Info("agent_ready",
				"agent_id", ev.AgentID,
				"source_hash", ev.SourceHash,
				"trust_score", ev.TrustScore,
				"capabilities", ev.Capabilities,
			)
		}
	})

	// Experiential memory removed: the ADR-0034 scope-promotion pipeline (clustered
	// insight write-back) is no longer wired. Document ingestion (the corpus) is unaffected.
	meta.ProfileSeeder.EventBus = eventBus
	// ADR-0100 P2: the dispatcher emits the SAME SelectionEventPayload the auction
	// does (winner + candidate slate + selection cost), which is how the
	// orchestration suite scores both arms. The bus is built after the metabolism
	// stack, so this wiring cannot happen at construction — without it the
	// dispatch arm would be silently invisible to the benchmark.
	if meta.Dispatcher != nil {
		meta.Dispatcher.EventBus = eventBus
	}
	meta.VerificationWorker.EventBus = eventBus // ADR-0047 D3: VerifierRoundEvent → operator feed
	// ADR-0033: crash detection publishes DaemonCrashedEvent to this bus.
	meta.Manager.EventBus = eventBus
	// ADR-0130 D1 / ADR-0157 D4: each spawned agent receives ITS credential in
	// its process environment (CAMBRIAN_AGENT_TOKEN + CAMBRIAN_AGENT_ID) — a
	// per-instance token the InstanceManager mints at the spawn itself, so no
	// credential source is wired here.
	// ADR-0130 D4: and HOW to reach the plane — the bound address and, on a
	// pure-TLS routable bind, the TLS root — resolved from the same listener
	// policy that binds it (agent_transport.go). Without this a TLS-only
	// routable kernel could not serve its own agents: they dialled
	// localhost:<port> in plaintext.
	agentTr, agentTrWarn, err := agentTransport(cfg)
	if err != nil {
		return fmt.Errorf("ADR-0130 agent transport: %w", err)
	}
	if agentTrWarn != "" {
		slog.Warn("ADR-0130: "+agentTrWarn, "tls_cert_file", cfg.Server.TLSCertFile)
	}
	// ADR-0157 S2: kernel-only (network-denied) sandboxed instances reach the
	// same gRPC plane through a unix socket in their own socket directory;
	// served from the moment the gRPC server starts (agent_kernel_socket.go).
	// S5: on Windows every agent in the AppContainer does (it has no loopback).
	agentSockets := newAgentSocketServer()
	agentTr.KernelSocket = agentSockets.Open
	meta.Manager.InstanceManager.SetAgentTransport(agentTr)
	slog.Info("ADR-0130: spawned agents dial the kernel", "addr", agentTr.Addr,
		"tls", agentTr.TLSCAFile != "", "server_name", agentTr.TLSServerName)
	// REACT-04 / ADR-0070: daemon auto-restart with backoff + flap quarantine. Disabled
	// when daemon_restart_max_attempts is 0 (a crashed daemon then stays down).
	if cfg.Execution.Agents.DaemonRestartMaxAttempts > 0 {
		meta.Manager.RestartPolicy = agentmgr.NewDaemonRestartPolicy(
			cfg.Execution.Agents.DaemonRestartMaxAttempts,
			time.Duration(cfg.Execution.Agents.DaemonRestartWindowSeconds)*time.Second,
			time.Duration(cfg.Execution.Agents.DaemonRestartBaseBackoffMs)*time.Millisecond,
			time.Duration(cfg.Execution.Agents.DaemonRestartMaxBackoffMs)*time.Millisecond,
		)
	}

	b.aw, b.calendar, b.spendMeter, b.llmGateway, b.meta = aw, calendar, spendMeter, llmGateway, meta
	b.pyEnv, b.sup, b.eventBus, b.agentSockets = pyEnv, sup, eventBus, agentSockets
	return nil
}

// buildApprovalsAndSessions builds the approval hub, the Python package layers,
// the plan executor and the Watcher that takes both, and the session manager.
func (b *kernelBuild) buildApprovalsAndSessions() error {
	ctx, cfg := b.ctx, b.cfg
	vec, reg, pyEnv, meta, mem, aw := b.vec, b.reg, b.pyEnv, b.meta, b.mem, b.aw
	// 4. Watcher — proactive signal processing (ADR-0009)
	//
	// The operator approval hub and the plan executor are created HERE, ahead of
	// their other consumers, because the Watcher takes both as constructor
	// arguments: a daemon-signalled plan is queued on the hub and runs through the
	// kernel's plan path only when an operator approves it (ResolveHITL). The hub
	// (not toolApprovalCtrl) is passed deliberately: tools_auto_approve is a
	// decision about dangerous tools and must not auto-run daemon-initiated plans.
	// planExec is late-bound to the server below (planExec.bind).
	//
	// ADR-0142 D11: it is THE approval hub — durable, addressed by policy, the
	// answer authorised at answer time — and every approval kind rides it
	// (buildApprovalHub; the old in-memory operator hub and consent controller
	// are deleted).
	toolApproval, apprErr := buildApprovalHub(ctx, cfg, vec.Pool())
	if apprErr != nil {
		return apprErr
	}
	// ADR-0157 D7.6-D7.8 (P3): agents that declare python_packages are held
	// until an Operator approved each package (python_package cards on this
	// hub) and the kernel installed the agent's own layer. The kind handler is
	// registered here, at boot, so a card answered after a restart still
	// installs.
	pyPackages := wirePythonPackages(ctx, cfg.Execution.Agents.Python, vec.Pool(), pyEnv, toolApproval, reg.GetManifest)
	if pyPackages != nil {
		meta.Manager.SetPythonLayers(pyPackages)
	}
	planExec := &kernelPlanExecutor{}
	watcher := supwatcher.New(
		meta.Manager,
		// ENG-1339: signal enrichment reads as the signal's principal through the
		// QueryService (no principal, no priming).
		mem.QueryService,
		aw.Planner,
		toolApproval,
		planExec,
		supwatcher.WatcherConfig{
			SignalNoiseThreshold:  cfg.Execution.Supervision.SignalNoiseThreshold,
			SignalNoiseWindowSecs: cfg.Execution.Supervision.SignalNoiseWindowSecs,
			RecallMinCosine:       cfg.Execution.Memory.MemoryRelevanceThreshold,
			RecallMaxResults:      cfg.Execution.Memory.MaxMemoryResults,
		},
	)

	// 5. SessionManager — episodic memory lifecycle (ADR-0012)
	//
	// Phase 4: sessions, runs and checkpoints live in Postgres. bbolt had no indexes, no
	// foreign keys and no retention, so the operator plane full-scanned a bucket that only
	// ever grew — every Execute minted a session and nothing ever reclaimed one. Postgres is
	// already a hard boot dependency (the pgvector adapter above is fatal on failure), so
	// this adds no new operational requirement.
	//
	// The bbolt implementation stays as the other adapter behind the same ports: it is what
	// the store-level tests run against, and what a future embedded build would use.
	var sessionRepo session.SessionRepository = reg
	pgSessions, sessErr := postgres.NewPgSessionStore(ctx, vec.Pool())
	if sessErr != nil {
		slog.Warn("Phase 4: Postgres session store unavailable; falling back to bbolt "+
			"(no retention, no indexed queries)", "err", sessErr)
	} else {
		sessionRepo = pgSessions
	}
	sessionMgr := session.New(sessionRepo)

	// The per-session caller term is composed INSIDE the decision point now
	// (premium authz.Authorizer.Sessions), so there is nothing to wire here: the
	// session manager is handed to the plugin, not to the query path.

	b.toolApproval, b.pyPackages, b.planExec, b.watcher = toolApproval, pyPackages, planExec, watcher
	b.pgSessions, b.sessionMgr = pgSessions, sessionMgr
	return nil
}

// wireSessionLifecycle wires the session manager, the LLM gateway (built before
// the metabolism stack) and the CircadianRhythm that drives session eviction,
// dormancy and retention.
func (b *kernelBuild) wireSessionLifecycle() {
	cfg, opts := b.cfg, b.opts
	sessionMgr, eventBus, llmGateway, llmProvider := b.sessionMgr, b.eventBus, b.llmGateway, b.llmProvider
	observer, profiles, pgSessions := b.observer, b.profiles, b.pgSessions
	// Experiential memory removed: the EpisodicExtractor + MemoryLifecycleManager
	// (ADR-0029/0030 episodic-narrative consolidation) are no longer wired. Session
	// token eviction stays on the CircadianRhythm below; ttl still drives dormancy.
	ttl := time.Duration(cfg.Execution.Session.SessionTTLDays) * 24 * time.Hour

	// Wire SessionManager so it publishes SessionDormantEvent on state transition.
	sessionMgr.SetEventBus(eventBus)
	sessionMgr.SetTTL(ttl)
	// ADR-0090 D3: a session opened by a REGISTERED ingress carries that ingress's
	// surface, decided here and never restated by the daemon delivering later turns.
	// nil resolver (the OSS default) leaves every surface transport-derived.
	sessionMgr.SetIngressResolver(opts.IngressResolver)

	// CircadianRhythm now only handles session token eviction (ADR-0018 sweep).
	circadianRhythm := circadian.New(sessionMgr, 0)

	// ADR-0018: wire the LLM Gateway (constructed above, before the metabolism
	// stack) and its lifecycle consumers.
	llmGateway.Observer = observer
	// ADR-0139 D1 (wave 1c): every agent call is served through the Provider's
	// stack — its breaker, tracing, concurrency cap and schema layer — on the step's
	// chain (winner → fallbacks → default). The gateway used to hold a second client
	// set (NewStreamersFromGenerators) with its own 30 s health cache; both are gone.
	// The Provider's generator table reloads live, so a console-registered model is
	// servable here with no further wiring.
	llmGateway.SetStepModels(llmProvider)
	// Resilience: when a step's auction produced no model winner (e.g. no TraitModel
	// agent is registered/matched), the streaming gateway falls back to the
	// configured default model so the agent still generates — the same default that
	// already serves the organs via the broker.
	llmGateway.SetDefaultModelID("llm:" + cfg.LLMProvider.Default)
	// Agent stack v2: a profile's `model:` goes first on its agent's lease chain.
	llmGateway.SetAgentModelPreference(profileModelPreference(profiles, llmProvider.Roles))
	checkProfileModels(profiles, func(id string) bool {
		_, ok := llmProvider.Registry().Lookup(id)
		return ok
	}, llmProvider.Roles())
	circadianRhythm.SessionEvictor = llmGateway
	circadianRhythm.SessionSweepInterval = time.Duration(cfg.Execution.Session.SessionTokenSweepIntervalSeconds) * time.Second
	// Phase 2: the ACTIVE→DORMANT driver. Distinct from the token sweep above — that one
	// evicts expired per-step BudgetLeases (ADR-0018), this one ages out idle task
	// SESSIONS. Conflating the two is how the lifecycle ended up with no driver at all.
	circadianRhythm.SessionIdleTimeout = time.Duration(cfg.Execution.Session.SessionIdleTimeoutMinutes) * time.Minute
	circadianRhythm.SessionIdleSweepInterval = time.Duration(cfg.Execution.Session.SessionIdleSweepIntervalSeconds) * time.Second
	circadianRhythm.IdleSweeper = sessionMgr
	// Phase 4: retention closes the lifecycle. Only wired with the Postgres store — the
	// cascade that makes reclaiming a session reclaim its runs and checkpoints is a
	// property of the schema, not something to reimplement as three bbolt sweeps.
	if pgSessions != nil {
		circadianRhythm.RetentionPurger = pgSessions
		circadianRhythm.SessionRetention = time.Duration(cfg.Execution.Session.SessionRetentionDays) * 24 * time.Hour
		circadianRhythm.RetentionSweepInterval = time.Duration(cfg.Execution.Session.SessionRetentionSweepIntervalSeconds) * time.Second
	}

	b.circadianRhythm = circadianRhythm
}

// wirePlannerRouting applies the planner and routing arms: adaptive token
// sizing, the capability contract and vocabulary, per-capability merit, and the
// ROUTE-07 learned gatekeeper scorer.
func wirePlannerRouting(cfg *config.Config, aw *kernel.AwarenessStack, sup *kernel.SupervisionStack, meta *kernel.MetabolismStack) {
	// ADR-0018: Wire adaptive token sizing to the Planner via ProfileAggregator.
	aw.Planner.SetAdvisor(sup.ProfileAggregator)

	// ROUTE-03: enable the capability contract on the planner when the arm is on
	// (execution.capability_contract). Off ⇒ pre-ROUTE-03 planner prompt/hash.
	aw.Planner.SetCapabilityContract(cfg.Execution.Routing.CapabilityContract)
	// ROUTE-04 / ADR-0067: deterministic capability-vocabulary normalization arm.
	aw.Planner.SetCanonicalVocab(cfg.Execution.Capability.CanonicalVocab)

	// ROUTE-06 / ADR-0069: per-capability merit is read directly from ExecCfg by the
	// Gatekeeper, so there is nothing to wire here beyond announcing the arm.
	//
	// The bounded-provisional-exploration half of ROUTE-06 was REMOVED 2026-08-08. The
	// budget bounded the provisional L2 bypass and the Auctioneer was its only recorder
	// of wins; ADR-0100 P3 deleted the Auctioneer, after which Allowed always returned
	// true. Rather than give the Dispatcher a RecordWin call, the budget was deleted:
	// nothing had depended on the bound for months, and a bound that cannot bind is
	// worse than no bound because the config key reads as a guarantee. The provisional
	// bypass is now unconditional in both arm positions — the behaviour that actually
	// shipped. If exploration needs bounding later it belongs on the Dispatcher, which
	// is the thing that now knows a candidate was picked.
	if cfg.Execution.Routing.PerCapabilityMerit {
		slog.Info("ROUTE-06: per-capability merit active (tag-scoped L3 merit; provisional L2 bypass is unconditional)")
	}

	// ROUTE-07 / ADR-0076: load the learned gatekeeper scorer when the arm is on and a
	// model path is configured. Offline-trained (cmd/route07-scorer); adopted online only
	// after a published offline win. A missing/invalid model leaves the hand weights in
	// place (the arm stays inert) — never a silent zero-score.
	if cfg.Execution.Routing.LearnedScorer && cfg.Execution.Routing.LearnedScorerModelPath != "" && meta.Gatekeeper != nil {
		if f, err := os.Open(cfg.Execution.Routing.LearnedScorerModelPath); err != nil {
			slog.Warn("ROUTE-07: learned_scorer on but model unreadable — using hand weights",
				"path", cfg.Execution.Routing.LearnedScorerModelPath, "err", err)
		} else {
			model, lerr := routescorer.Load(f)
			f.Close()
			if lerr != nil {
				slog.Warn("ROUTE-07: learned scorer model load failed — using hand weights", "err", lerr)
			} else {
				meta.Gatekeeper.RouteScorer = model
				slog.Info("ROUTE-07: learned gatekeeper scorer active", "n_train", model.N,
					"weights", model.Weights)
			}
		}
	}
}

// organisationLocation is the timezone of the kernel's calendar: organisation.timezone,
// else UTC. Load validates the zone, so an error here means a config that skipped
// validation; it is logged, never silent, and the calendar falls back to UTC.
func organisationLocation(cfg *config.Config) *time.Location {
	loc, err := cfg.Organisation.Location()
	if err != nil {
		slog.Error("organisation.timezone unusable; dating prompts in UTC", "err", err)
		return time.UTC
	}
	return loc
}
