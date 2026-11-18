package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentplane"
	"github.com/cambrian-sh/core/internal/config"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/metabolism/registration"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	supgk "github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	"github.com/cambrian-sh/core/internal/supervision/verify"

	"golang.org/x/sync/errgroup"
)

// MetabolismStack is the agent lifecycle + auction layer. It owns process
// management, candidate discovery, registration-time profile seeding, and output
// verification.
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
	// because a second implementation exists. The value is the Dispatcher wrapped
	// in the yield consumer (internal/metabolism/yield, ADR-0100 D10) — a
	// decorator, not a second selector: it binds sub-goals through the same one.
	Selector domain.StepDispatcher
	// Dispatcher is the concrete selector inside Selector, always non-nil. Exposed ONLY so the
	// composition root can finish wiring it — the EventBus is built after this
	// stack, and a dispatcher that cannot emit SelectionEventPayload is
	// invisible to the orchestration suite.
	Dispatcher *dispatch.Dispatcher
	// ProfileSeeder makes newly registered agents routable: a deterministic vector
	// from the declared description and a neutral-prior profile, no LLM. It replaced
	// the ADR-0037 graded interview on 2026-11-05 (ADR-0139 Q3).
	ProfileSeeder      *registration.ProfileSeeder
	VerificationWorker *verify.VerificationWorker
	VerifierPool       *verify.VerifierPool
}

// NewMetabolismStack wires the full agent lifecycle.
//
// Parameters:
//   - reg: the domain decorator (implements AgentRegistry, AgentUpdater, TaskEventReadWriter)
//   - embedder: embeds task descriptions (semantic gate) and agent descriptions (profile seeding)
//   - profileReader: the read side the profile searcher ANN-searches agent
//     profiles through (MemoryStack.Reader(); the searcher passes ScopeSystem)
//   - profileStore: agent cognitive fingerprints (from MemoryStack)
//   - memoryAgent: for the Memory Barrier in AgentManager
//   - cfg: execution + metabolism parameters
//   - leases: the substrate LLM gateway's lease lifecycle; the verification
//     worker mints one lease per verifier call through it (ADR-0131). nil ⇒
//     verifier calls carry no lease (tests).
func NewMetabolismStack(
	reg *AgentRepoDecorator,
	embedder domain.Embedder,
	profileReader domain.VectorReader,
	profileStore memstore.ProfileStore,
	memoryAgent domain.MemoryAgent,
	cfg *config.Config,
	// Unused since 2026-11-14: its only consumer was the dispatcher's
	// cambrian_auction_no_winner_total counter, deleted by owner decision
	// (ENG-1335). The parameter stays until app/app.go (another owner's file)
	// drops the argument (T-20261114-drop-the-unused-telemetry-observer-argument-of).
	_ domain.TelemetryObserver,
	leases verify.LeaseGateway,
) *MetabolismStack {
	manager := agentmgr.NewAgentManager(reg, "localhost:"+cfg.Server.Port, memoryAgent)
	// ADR-0125: JS agent runtimes. Empty config values are fine — the instance
	// manager falls back to $PATH lookup at spawn time.
	manager.SetRuntimeExecutable(domain.RuntimeBun, cfg.Metabolism.BunExecutable)
	manager.SetRuntimeExecutable(domain.RuntimeNode, cfg.Metabolism.NodeExecutable)
	// SEC-01: spawned agents get a deny-by-default environment (OS essentials +
	// the operator's non-secret passthrough); the kernel's API keys never leak.
	manager.SetEnvPassthrough(cfg.Execution.Agents.AgentEnvPassthrough)
	// Config-derived agent env: values the kernel SETS from config rather than
	// passing through from its own environment — a knob must never depend on
	// .env (owner rule 2026-08-11). Zero values ⇒ the agent's built-in default,
	// no var set. One combined slice: SetEnvExtras REPLACES the list.
	var envExtras []string
	if k := cfg.Execution.Tools.ToolMenuK; k > 0 {
		envExtras = append(envExtras, fmt.Sprintf("CAMBRIAN_TOOL_MENU_K=%d", k))
	}
	if m := cfg.Execution.Retrieval.RerankerModel; m != "" {
		envExtras = append(envExtras, "RERANK_MODEL="+m)
	}
	// ADR-0145 D10: agents trace their own model and tool calls into the kernel's
	// run-trace directory (sdk cambrian_agent_sdk/run_trace.py).
	envExtras = append(envExtras, traceEnv(cfg.Execution.Trace)...)
	// ADR-0145 D3: the SDK's evidence re-prompt (sdk react.py, EVIDENCE_REPROMPT_ENV),
	// and the composed runtime's per-step tool-call budget (execution.workers.max_tool_rounds).
	envExtras = append(envExtras, workersEnv(cfg.Execution.Workers)...)
	// execution.content: the SDK's tool-output cap, preview, resolve window and
	// prompt budget, so agent and kernel size content by one policy.
	envExtras = append(envExtras, contentEnv(cfg.Execution)...)
	// ADR-0157 S7: the shared model cache (HF_HOME, TORCH_HOME, DOCLING_CACHE_DIR),
	// with the sandbox on or off, so an organ's weights outlive its instance's
	// scratch (config.ModelCacheEnv; resolved by app.anchorAgentSandboxPaths).
	envExtras = append(envExtras, config.ModelCacheEnv(cfg.Execution.Agents.ModelCache)...)
	if len(envExtras) > 0 {
		manager.SetEnvExtras(envExtras)
	}
	// SEC-01: cap agent memory (0 = disabled) so a runaway agent is killed at its
	// cap instead of OOMing the kernel host.
	manager.SetAgentMemoryLimitMB(cfg.Execution.Agents.AgentMemoryLimitMB)
	// ADR-0139 D8 (execution.agents.sandbox): per-trust-tier agent containment.
	// First-party unchanged; a third-party agent is refused while its controls are
	// missing on the agent launch path, unless on_unavailable is degrade.
	agentSandbox := cfg.Execution.Agents.Sandbox.ThirdParty
	manager.SetAgentContainment(agentmgr.AgentContainment{
		ThirdPartyDegrade:  !agentSandbox.Refuses(),
		ThirdPartyMemoryMB: agentSandbox.Memory(),
		Sandbox:            agentSandboxFromConfig(cfg),
	})
	slog.Info("ADR-0139 D8: agent containment by trust tier",
		"third_party_on_unavailable", map[bool]string{true: "refuse", false: "degrade"}[agentSandbox.Refuses()],
		"third_party_memory_mb", agentSandbox.Memory(),
		"sandbox_enabled", cfg.Execution.Agents.Sandbox.Enabled)
	// T-20261102-kg-extractor-cold-start-drops-the-first-kg: how long a spawned
	// agent may take to open its socket before its boot fails.
	manager.SetAgentBootTimeout(time.Duration(cfg.Execution.Agents.AgentBootTimeoutMs) * time.Millisecond)
	// Wire default model pricing for token cost estimation. ADR-0042: the default
	// generator's cost is the single source of truth (was cfg.Models[0]).
	if def := cfg.LLMProvider.DefaultGenerator(); def != nil {
		manager.DefaultInputCostPer1M = def.CostPer1MInput
		manager.DefaultOutputCostPer1M = def.CostPer1MOutput
	}

	profileSearcher := registration.NewProfileSearcher(profileReader)
	seeder := registration.NewProfileSeeder(reg, embedder, profileStore, reg)

	gatekeeper := supgk.NewGatekeeper(reg, cfg.Execution,
		supgk.WithProfiles(profileStore),
		supgk.WithEmbedder(embedder),
		supgk.WithSearcher(profileSearcher),
	)
	// ADR-0139 D6: say which merit scorer routes this kernel, so an A/B/A arm can
	// prove from the boot log that its flag took.
	slog.Info("Gatekeeper: merit mode",
		"merit_mode", cfg.Execution.Gatekeeper.ActiveMeritMode(),
		"prior_alpha", cfg.Execution.Gatekeeper.MeritPriorAlpha,
		"prior_beta", cfg.Execution.Gatekeeper.MeritPriorBeta,
		"lcb_quantile", cfg.Execution.Gatekeeper.MeritLCBQuantile,
		"unverified_weight", cfg.Execution.Gatekeeper.MeritUnverifiedWeight,
		"exploration_rate", cfg.Execution.Routing.ExplorationRate,
		"exploration_active", cfg.Execution.Gatekeeper.ActiveMeritMode() != config.MeritModeThompson)

	transport := agentplane.New(manager)
	// ADR-0150 D10: execution.lane.context reaches every spawned agent on its
	// handoff (the lane block wins over execution.content; lane_context.go).
	transport.ContextLimits = cfg.Execution.AgentContext()
	if !transport.ContextLimits.IsZero() {
		slog.Info("ADR-0150 D10: execution.lane.context stamped on agent handoffs",
			"budget_tokens", transport.ContextLimits.BudgetTokens,
			"tool_output_compact_chars", transport.ContextLimits.ToolOutputCompactChars,
			"protect_tail", transport.ContextLimits.ProtectTail,
			"keep_recent_tool_results", transport.ContextLimits.KeepRecentToolResults)
	}

	vPool := verify.NewVerifierPool(reg, profileStore, cfg.Execution.Verification.VerifierPoolThreshold, cfg.Execution.Verification.VerifierRecencyWindow)
	// Verifier admission runs the gatekeeper's L1 declaration gate (one policy), so
	// it reads the same canonical-vocabulary switch the gatekeeper does.
	vPool.CanonicalVocab = cfg.Execution.Capability.CanonicalVocab
	vWorker := verify.NewVerificationWorker(vPool, transport, reg, profileStore, profileStore, embedder, leases, verify.VerificationWorkerConfig{
		TrustBoostThreshold:   cfg.Execution.Verification.TrustBoostThreshold,
		QueueCapacity:         cfg.Execution.Verification.VerificationQueueCapacity,
		CrossVerifyRate:       cfg.Execution.Verification.CrossVerifyRate,
		VerifierRecencyWindow: cfg.Execution.Verification.VerifierRecencyWindow,
		// A verifier call is one agent turn: sized like the agent step lease.
		LeaseTokenLimit: subnetwork.StepLeaseTokenLimit,
		LeaseDuration:   subnetwork.StepLeaseDuration,
	})

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
		ExplorationRate: cfg.Execution.Routing.ExplorationRate,
	}
	// ADR-0139 D8: the dispatcher binds each plan-step lease to the agent it hands it
	// to, so a lease presented by any other agent is refused. The step lease is minted
	// before selection (server.go), so dispatch is the first place the agent is known.
	if b, ok := leases.(domain.LeaseAgentBinder); ok {
		dispatcher.Leases = b
	}
	// ADR-0041 D5 / ADR-0100 D10: an agent may YIELD a sub-goal instead of
	// answering. The yield consumer decorates the Dispatcher here, at the one place
	// the Selector is built, so every caller of it (DAG step, bypass arm, runner-up
	// fallback, plugins) resolves a yield: the sub-goal is bound by this same
	// dispatcher and the yielding agent is resumed with the result. Without it a
	// yield reached the DAG as an empty answer (core b0072cc deleted the old driver).
	var selector domain.StepDispatcher = yield.NewResolver(dispatcher, yield.Limits{
		MaxDepth:    cfg.Execution.Routing.YieldMaxDepth,
		MaxSubgoals: cfg.Execution.Routing.YieldMaxSubgoals,
	})
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
		ProfileSeeder:      seeder,
		VerificationWorker: vWorker,
		VerifierPool:       vPool,
	}
}

// Start launches the ProfileSeeder and VerificationWorker background goroutines.
func (s *MetabolismStack) Start(ctx context.Context) error {
	s.rootCtx = ctx
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.ProfileSeeder.Start(gCtx) })
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
	return func(stepCtx context.Context, taskID, agentID string, req, resp *domain.Handoff) {
		// The enqueue is bounded by the KERNEL ctx, not the step ctx (the step may
		// already be ending); only the originating session is carried over, so the
		// verifier's spend is booked to it (ADR-0131, owner decision 2026-11-01).
		ctx, cancel := context.WithTimeout(s.rootCtx, 5*time.Second)
		defer cancel()
		if sid, ok := domain.SessionIDFromContext(stepCtx); ok && sid != "" {
			ctx = domain.WithSessionID(ctx, sid)
		}
		s.VerificationWorker.Enqueue(ctx, taskID, agentID, req, resp)
	}
}

// ProfileSeedEnqueuer returns the callback that storage uses to enqueue
// newly-registered or changed agents for profile seeding.
func (s *MetabolismStack) ProfileSeedEnqueuer() func(domain.AgentDefinition) {
	return s.ProfileSeeder.Enqueue
}

// agentSandboxFromConfig is execution.agents.sandbox as the instance manager
// applies it: nil when disabled (the launch is unchanged), else the ADR-0157
// agent sandbox (S1 confinement, S2 network by tier). The kernel-state and runtime paths were resolved by the
// composition root (app.anchorAgentSandboxPaths); the host tool locations
// (bwrap, docker, the cgroup parent) are mcp.sandbox's, one fact per host.
func agentSandboxFromConfig(cfg *config.Config) *agentmgr.AgentSandbox {
	sb := cfg.Execution.Agents.Sandbox
	if !sb.Enabled {
		return nil
	}
	s := agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		Runner:           sb.RunnerOrDefault(),
		WorkRoot:         sb.WorkRoot,
		FirstPartyRefuse: sb.FirstParty.Refuses(),
		ThirdPartyRefuse: sb.ThirdParty.Refuses(),
		HidePaths:        sb.HidePaths,
		KernelStatePaths: sb.KernelStatePaths,
		KernelDir:        sb.KernelDir,
		RuntimeReadPaths: sb.RuntimeReadPaths,
		AllowlistCap:     sb.AllowlistCap,
		DockerImage:      sb.DockerImage,
		DockerUser:       sb.DockerUser,
		BwrapPath:        cfg.MCP.Sandbox.BwrapPath,
		DockerPath:       cfg.MCP.Sandbox.DockerPath,
		CgroupParent:     cfg.MCP.Sandbox.CgroupParent,
		KernelOnlyAll:    sb.KernelOnlyAll(),
		ModelCache:       cfg.Execution.Agents.ModelCache,
	})
	p := s.Policy()
	slog.Info("ADR-0157 S1: agents launch through the sandbox (D1 confinement, every tier)",
		"runner", sb.RunnerOrDefault(), "work_root", p.WorkRoot, "hidden", p.HidePaths,
		"first_party_on_unavailable", map[bool]string{true: "refuse", false: "degrade"}[sb.FirstParty.Refuses()],
		"allowlist_cap", sb.AllowlistCap,
		// ADR-0157 S2 (D3): the tiers whose agents have no network but the
		// kernel's unix socket (execution.agents.sandbox.network).
		"network", sb.NetworkOrDefault(), "kernel_only_tiers", s.KernelOnlyTiers(),
		// ADR-0157 S7: writable for first-party and system agents, read-only for third-party.
		"model_cache", cfg.Execution.Agents.ModelCache)
	return s
}
