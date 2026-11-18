package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/hooks"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/telemetry"
)

// buildInfrastructure is the boot's first phase: the data directory, process
// runtime settings, telemetry, the bbolt store and agent registry, the pgvector
// adapter (and plugin schema lanes), health, the resolved decision point, the
// audit and conversation stores, the principal registry, the lifecycle hooks,
// the LLM Provider and the embedder, and the purpose-bound generators.
func (b *kernelBuild) buildInfrastructure() error {
	ctx, cfg, opts, composed, lifecycles := b.ctx, b.cfg, b.opts, b.composed, b.lifecycles
	os.MkdirAll(cfg.Storage.DataDir, 0755)

	// 0. Process-level runtime settings (config/runtime.go; all off by default):
	// the Go soft memory limit is applied before any stack allocates, and a pprof
	// listener requested on a non-loopback bind fails the boot here, before a
	// port opens. Served later from startKernelServices.
	applyMemoryLimit(cfg.Runtime, osGetenv)
	if why := pprofRefusal(cfg); why != "" {
		return fmt.Errorf("runtime diagnostics: %s", why)
	}

	// Bootstrap OTel from config (before any stack construction).
	tp, mp := initTelemetry(cfg)
	tpShutdown := func(ctx context.Context) error {
		if tp != nil {
			return tp.Shutdown(ctx)
		}
		return nil
	}
	mpShutdown := func(ctx context.Context) error {
		if mp != nil {
			return mp.Shutdown(ctx)
		}
		return nil
	}
	_ = tpShutdown
	_ = mpShutdown

	// 1. Infrastructure — storage, vector DB, LLM
	storeHandle, reg, err := kernel.BootstrapStorage(cfg)
	if err != nil {
		return err
	}

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		storeHandle.Close()
		return err
	}
	// Plugin schema lanes (Registry.AddMigrations) run right after core's, under the
	// same auto-migrate gate and database.migrations policy; with no plugin lanes (OSS)
	// this is a no-op (T-20261113-premium-has-no-schema-migration-chain-41-create).
	if err := vec.BootPluginMigrations(ctx, composed.migrations); err != nil {
		storeHandle.Close()
		return fmt.Errorf("plugin schema migrations: %w", err)
	}

	// PLAT-03 / ADR-0065: readiness = the database is reachable. Ping the pgvector pool.
	healthChecker := health.New(func(pctx context.Context) error {
		if vec == nil || vec.Pool() == nil {
			return fmt.Errorf("db pool unavailable")
		}
		return vec.Pool().Ping(pctx)
	})

	// ADR-0085: resolve the access-control decision point ONCE, here, and hand the
	// same instance to every enforcement point. A nil Authorizer means no policy
	// plugin is installed, which in OSS is not a degraded mode — unrestricted is
	// the correct and only semantics for a single-tenant open-source deployment.
	// Fail-closed is a property of the plugin, not of the kernel.
	authorizer := opts.Authorizer
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	// ADR-0084 D7 (owner decision 2026-11-14, ENG-614): a customer conversation reads
	// only customer-visible memory. ONE wrapper at the decision point, so every reader
	// below is narrowed; the profile is stamped server-side from the caller's lease.
	authorizer = authz.NewProfileNarrowingAuthorizer(authorizer, cfg.Execution.Chat.CustomerVisibleTags)

	// ADR-0047 0047-24: durable operator audit store (Postgres), reusing the
	// pgvector pool. Falls back to in-memory if the table can't be created.
	var operatorAudit domain.AuditStore
	if pgAudit, auditErr := postgres.NewPgAuditStore(ctx, vec.Pool()); auditErr != nil {
		slog.Warn("operator audit: Postgres store unavailable, using in-memory", "err", auditErr)
		operatorAudit = operator.NewInMemoryAuditStore()
	} else {
		operatorAudit = pgAudit
		// ENG-128: execution.retention — operator audit kept 365 days by default,
		// derived/cache tables opt-in; evidence and knowledge are never TTL'd.
		retentionWorker := buildRetentionWorker(cfg.Execution.Retention, pgAudit, vec.Pool())
		lifecycles = append(lifecycles, Lifecycle{Name: "retention", Start: retentionWorker.Start, Stop: retentionWorker.Stop})
	}
	// ADR-0084 D1: the durable Conversation/Message store, reusing the pgvector pool.
	// Schema is owned by migration 0002 (ADR-0064), so an unmigrated DB yields a nil store
	// and chat surfaces stay unavailable rather than the kernel failing to boot — the same
	// fail-soft posture the audit store uses. Retrieval and orchestration do not depend on
	// conversations, so a missing chat store must not take the whole kernel down.
	var conversations domain.ConversationStore
	// Held separately as the concrete type: "who came through this entry point" is
	// an aggregate query the ConversationStore interface deliberately does not
	// carry, and widening that interface would oblige every implementation and
	// every test fake to grow a method none of them use.
	var ingressTraffic domain.IngressTrafficLister
	// ADR-0142 D3/D1: the principal registry (store of record for persons) and the
	// Authenticator over it. Built here, before plugins Build, so KernelServices
	// hands plugins the registry and a plugin's NewAuthenticator is already folded
	// into opts. A failure to build the configured identity backend fails the boot:
	// falling back to another backend would be a silent change of who can sign in.
	idStores := buildPrincipalRegistry(ctx, vec.Pool())
	if convStore, convErr := postgres.NewPgConversationStore(ctx, vec.Pool()); convErr != nil {
		slog.Warn("ADR-0084: conversation store unavailable; chat surfaces disabled", "err", convErr)
	} else {
		conversations = convStore
		ingressTraffic = convStore
	}

	// ADR-0042: the centralized LLM Provider is the sole authority on model
	// availability/provisioning. System organs Acquire purpose-bound generators
	// from it (with live health failover), and since ADR-0139 wave 1c the agent-plane
	// gateway serves every agent call through it too (SetStepModels below): one
	// client set, one breaker.
	//
	// ADR-0139 D11: the lifecycle-hook chain — the operator's execution.hooks plus
	// every plugin's hooks, in one stable order. Built before the Provider so both
	// it (pre/post_model) and the ToolExecutor (pre/post_tool) take it at
	// construction. A misdeclared hook fails the boot.
	hookChain, err := hooks.NewChain(cfg.Execution.Hooks, opts.Hooks, opts.HookObserver, slog.Default())
	if err != nil {
		storeHandle.Close()
		return fmt.Errorf("execution.hooks: %w", err)
	}
	if names := hookChain.Names(); len(names) > 0 {
		slog.Info("ADR-0139 D11: lifecycle hooks installed", "hooks", names)
	}
	// The final-answer tool/arg name the native tool lane's deliverable, which the
	// post_model hooks see and may rewrite (T-20261106-post-model-hooks-never-see-
	// native-tool-call) — the same keys the live step-output stream extracts.
	llmProvider, err := llm.NewProvider(cfg.LLMProvider, slog.Default(), llm.WithHooks(hookChain),
		llm.WithFinalAnswer(cfg.Execution.StepOutput.FinalAnswerToolName(), cfg.Execution.StepOutput.FinalAnswerArgName()),
		// The control-verb arguments post_model hooks guard besides the deliverable
		// (execution.hooks.guarded_arguments; T-20261106-post-model-hooks-do-not-guard-control-verb).
		llm.WithGuardedArgs(cfg.Execution.Hooks.GuardedArgumentList()))
	if err != nil {
		storeHandle.Close()
		return fmt.Errorf("llm provider: %w", err)
	}
	embedder := &llm.OllamaEmbedder{
		BaseURL:     cfg.Embedder.Endpoint,
		Model:       cfg.Embedder.Model,
		TimeoutMs:   cfg.Embedder.TimeoutMs,
		QueryPrefix: cfg.Embedder.QueryPrefix, // ADR-0048: asymmetric retrieval (bge query instruction)
	}

	// ADR-0019: Create telemetry observer and Langfuse generator wrappers.
	observer := telemetry.NewBridge(cfg.Telemetry)

	// ADR-0042 / ADR-0057: the generator trace wrapper is an injected hook (Options).
	// OSS default is identity (no tracing); the premium binary injects a Langfuse
	// wrapper. Applied INSIDE the Provider at the Acquire chokepoint, so EVERY
	// acquired generator — including the router wired deeper in ProvideServer — is
	// wrapped by purpose.
	if opts.TraceWrapper != nil {
		llmProvider.SetTraceWrapper(opts.TraceWrapper)
	}
	// ADR-0145 D10: the decision-level run trace (execution.trace.*, default off).
	runTracer := wireRunTrace(cfg.Execution.Trace)
	if runTracer != nil {
		llmProvider.SetRunTracer(runTracer)
	}

	// Each organ Acquires a purpose-bound generator (live failover + tracing per call).
	memoryGen := llmProvider.GeneratorFor(domain.PurposeMemory)
	factsGen := llmProvider.GeneratorFor(domain.PurposeFacts) // ADR-0136 D4: read only when execution.facts.extract_enabled
	awarenessGen := llmProvider.GeneratorFor(domain.PurposePlanner)
	supervisionGen := llmProvider.GeneratorFor(domain.PurposeVerifier)

	b.lifecycles = lifecycles
	b.storeHandle, b.reg, b.vec, b.healthChecker = storeHandle, reg, vec, healthChecker
	b.authorizer, b.operatorAudit, b.conversations, b.ingressTraffic = authorizer, operatorAudit, conversations, ingressTraffic
	b.idStores, b.hookChain, b.llmProvider, b.embedder = idStores, hookChain, llmProvider, embedder
	b.observer, b.runTracer = observer, runTracer
	b.memoryGen, b.factsGen, b.awarenessGen, b.supervisionGen = memoryGen, factsGen, awarenessGen, supervisionGen
	return nil
}

// registerAgents populates the agent registry: model agents, every AgentSource
// (the built-in filesystem scan first, then plugin sources), and the orphan
// reconciles — before anything reads the population.
func (b *kernelBuild) registerAgents() {
	ctx, cfg, composed, reg := b.ctx, b.cfg, b.composed, b.reg
	// Register LLM models as TraitModel agents (registry entries the console and
	// dispatch read; the auction they once bid in is deleted, ADR-0100 P3).
	registerModelAgents(reg, cfg.LLMProvider.Generators)
	// ADR-0075: register agents from all AgentSources — the built-in filesystem scan
	// (now a system-aware FilesystemAgentSource carrying manifests) FIRST, then any
	// plugin-contributed sources. This runs BEFORE the reconciles so the orphan-eviction
	// pass sees the freshly-registered filesystem population.
	//
	// Agent stack v2: the built-in source also registers one agent per declarative
	// profile (agents/profiles/*.yaml), validated against the skills this kernel
	// serves (the same "skills" directory LoadRegistry reads below).
	builtinSource := newBuiltinFilesystemAgentSource(cfg.Metabolism.AgentsDir).withSkillTools(profileSkillIndex("skills")).
		withComposedRuntime(composedRuntimeID(cfg.Execution.Workers), composedThirdPartyRuntimeID(cfg.Execution.Workers)) // ADR-0145 D12, "" when off
	registeredAgents := registerAgentSources(ctx, reg, append([]AgentSource{builtinSource}, composed.agentSources...))
	profiles := collectProfileAgents(registeredAgents)
	reconcileProfileAgents(ctx, reg, registeredAgents)
	// ADR-0042: config is the source of truth for the model population. Eviction
	// of models dropped from config — registration above is upsert-only, so a
	// removed model would otherwise survive in the registry and keep winning the
	// auction after a restart (the qwen-after-removal orphan bug).
	reconcileModelAgents(ctx, reg, cfg.LLMProvider.Generators)
	// Same orphan class for filesystem agents: sources are upsert-only, so an agent whose
	// source file was deleted lingers in the registry. Evict those whose ExecPath no
	// longer exists on disk; A2A/dynamic agents are spared.
	reconcileFilesystemAgents(ctx, reg, func(p string) bool { _, err := os.Stat(p); return err == nil })

	b.profiles = profiles
}
