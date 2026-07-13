package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/centralexec"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/metabolism/backfill"
	ossreactive "github.com/cambrian-sh/core/internal/reactive"
	"github.com/cambrian-sh/core/internal/scope"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	subsynaptic "github.com/cambrian-sh/core/internal/substrate/synaptic"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
	supsynaptic "github.com/cambrian-sh/core/internal/supervision/synaptic"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
	"github.com/cambrian-sh/core/internal/telemetry"
	tooldiscovery "github.com/cambrian-sh/core/internal/tool/discovery"
	toolproc "github.com/cambrian-sh/core/internal/tool/proc"
	"github.com/cambrian-sh/core/pkg/util"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Kernel acts as the centralized container for the Orchestrator's life support systems.
// After the stack refactor (2026-05-11c), it holds only:
//   - infrastructure primitives (Config, Registry, Store, Listener)
//   - domain stacks (Memory, Awareness, Metabolism, Supervision)
//   - runtime handles (Server, GRPC)
type Kernel struct {
	Config      *config.Config
	Registry    domain.AgentRegistry // domain-facing interface layer
	Store       io.Closer            // opaque storage handle — only Close() is exposed
	Memory      *kernel.MemoryStack
	Awareness   *kernel.AwarenessStack
	Metabolism  *kernel.MetabolismStack
	Supervision *kernel.SupervisionStack
	Server      *subnetwork.Server
	Listener    net.Listener
	GRPC        *grpc.Server

	// ADR-0047 0047-16: operator command effects bound to kernel surfaces.
	OperatorEffects operator.CommandEffects
	// ADR-0047 0047-24: durable operator audit store (Postgres, in-memory fallback).
	OperatorAudit domain.AuditStore

	// ADR-0012: Synaptic Bridge components.
	SessionMgr         *session.SessionManager
	EventLogger        *subsynaptic.EventLogger
	SynapticWatcher    *supsynaptic.SynapticWatcher
	CircadianRhythm    *circadian.CircadianRhythm
	MemoryLifecycleMgr *circadian.MemoryLifecycleManager
	ArtifactVault      *vault.ArtifactVault
	EventBus           *domain.InMemoryEventBus

	// ADR-0034: Tag-Based Data Access Scoping.
	ScopeResolver *scope.ScopeResolver
	ScopeStore    *postgres.PgAgentScopeStore

	// ADR-0039: tool grants store (operator sets grants via the admin endpoint).
	ToolGrants *domain.InMemoryGrantsStore

	// ADR-0043: live MCP server connections (nil when no servers configured).
	MCPConnector *mcp.Connector
	// ADR-0043 D8 / ADR-0044: health/reconnect inputs for the background Watch loop.
	MCPSink    mcp.ToolSink
	MCPServers []mcp.ServerConfig
}

// Shutdown initiates the graceful teardown of all kernel resources.
func (k *Kernel) Shutdown(ctx context.Context) {
	slog.Info("🧬 Kernel: Initiating graceful shutdown sequence...")

	// 1. Stop accepting new gRPC requests
	if k.GRPC != nil {
		slog.Info("🔌 Stopping gRPC Server (GracefulStop)...")
		k.GRPC.GracefulStop()
	}

	// 2. Close network listener
	if k.Listener != nil {
		_ = k.Listener.Close()
	}

	// 3. Stop domain stacks (reverse order of dependency)
	if k.Supervision != nil {
		k.Supervision.Shutdown(ctx)
	}
	if k.Metabolism != nil {
		k.Metabolism.Shutdown(ctx)
	}
	if k.Memory != nil {
		k.Memory.Shutdown(ctx)
	}
	if k.Awareness != nil {
		k.Awareness.Shutdown(ctx)
	}

	// 3b. Stop ADR-0012 Synaptic Bridge components
	if k.MCPConnector != nil {
		k.MCPConnector.Close() // ADR-0043: close live MCP sessions
	}
	if k.SynapticWatcher != nil {
		k.SynapticWatcher.Stop()
	}
	if k.CircadianRhythm != nil {
		k.CircadianRhythm.Stop()
	}
	if k.EventLogger != nil {
		_ = k.EventLogger.Close()
	}
	if k.ArtifactVault != nil {
		_ = k.ArtifactVault.Close()
	}

	// 4. Drain storage
	if k.Store != nil {
		k.Store.Close()
	}

	slog.Info("✅ Kernel: Shutdown complete. System at rest.")
}


// Run is the composition root. It loads configuration from the 7-layer
// pipeline (configs/config.json + tuning/mcp/local layers, see ADR-0024),
// wires every subsystem from opts + cfg, and starts the gRPC server.
func Run(ctx context.Context, opts Options) error {
	flag.Parse()

	// Load .env into the process environment before anything reads it, so API
	// keys (os.Getenv via api_key_env) and CAMBRIAN_* overrides resolve from a
	// local gitignored file. Missing file is a no-op; real env vars take priority.
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}

	cfg, err := config.LoadConfig("configs/config.json")
	if err != nil {
		return err // ConfigError is already structured; wrapping breaks errors.As in main()
	}

	// Capture the store handle so the force-quit signal handler can close it
	// before os.Exit(1), preventing bbolt corruption on double-SIGTERM.
	var storeCloser io.Closer

	// Signal Re-entrancy (Force Quit)
	rootCtx, stop := context.WithCancel(ctx)
	defer stop()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("Signal received, starting graceful shutdown...", "signal", sig)
		stop()

		<-sigCh
		slog.Warn("Second signal received! FORCING IMMEDIATE EXIT.")
		if storeCloser != nil {
			_ = storeCloser.Close()
		}
		os.Exit(1)
	}()

	// REDEMPTION: Health Check First (Fail Fast)
	lis, err := net.Listen("tcp", ":"+cfg.Server.Port)
	if err != nil {
		return fmt.Errorf("network (port %s unavailable): %w", cfg.Server.Port, err)
	}

	logResult, err := util.InitLogger(util.LogModeHeadless, cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	if logResult.File != nil {
		defer logResult.File.Close()
	}

	// REDEMPTION: Observability First. Start Kernel immediately.
	k, err := bootstrapKernel(rootCtx, cfg, lis, opts)
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		k.Shutdown(shutdownCtx)
	}()
	storeCloser = k.Store

	g, gCtx := errgroup.WithContext(rootCtx)

	// REDEMPTION: Parallel Workers (including Backfill)
	startKernelServices(g, gCtx, k)

	return g.Wait()
}

func bootstrapKernel(ctx context.Context, cfg *config.Config, lis net.Listener, opts Options) (*Kernel, error) {
	os.MkdirAll(cfg.Storage.DataDir, 0755)

	// 0. Bootstrap OTel from config (before any stack construction).
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
		return nil, err
	}

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		storeHandle.Close()
		return nil, err
	}

	// ADR-0034: agent scope store + resolver. Authoritative scope lives in the
	// PostgreSQL agent_scopes table (shared with pgvector — reuse its pool); the
	// resolver caches in-memory and invalidates cross-replica via LISTEN/NOTIFY.
	scopeStore, err := postgres.NewPgAgentScopeStore(ctx, vec.Pool())
	if err != nil {
		storeHandle.Close()
		return nil, fmt.Errorf("agent scope store: %w", err)
	}
	scopeResolver := scope.NewScopeResolver(scopeStore, 0, slog.Default())

	// ADR-0047 0047-24: durable operator audit store (Postgres), reusing the
	// pgvector pool. Falls back to in-memory if the table can't be created.
	var operatorAudit domain.AuditStore
	if pgAudit, auditErr := postgres.NewPgAuditStore(ctx, vec.Pool()); auditErr != nil {
		slog.Warn("operator audit: Postgres store unavailable, using in-memory", "err", auditErr)
		operatorAudit = operator.NewInMemoryAuditStore()
	} else {
		operatorAudit = pgAudit
	}
	if err := scopeResolver.Warm(ctx); err != nil {
		slog.Warn("ADR-0034: scope resolver warm failed (continuing with cold cache)", "err", err)
	}

	// ADR-0042: the centralized LLM Provider is the sole authority on model
	// availability/provisioning. System organs Acquire purpose-bound generators
	// from it (with live health failover). The legacy ProviderRegistry is retained
	// only as the streaming-client source for the ADR-0018 gateway (streaming is
	// out of ADR-0042 scope), built from the same generator set.
	llmProvider, err := llm.NewProvider(cfg.LLMProvider, slog.Default())
	if err != nil {
		storeHandle.Close()
		return nil, fmt.Errorf("llm provider: %w", err)
	}
	providers, err := llm.NewProviderRegistryFromGenerators(cfg.LLMProvider.Generators)
	if err != nil {
		storeHandle.Close()
		return nil, fmt.Errorf("provider registry: %w", err)
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

	// Each organ Acquires a purpose-bound generator (live failover + tracing per call).
	memoryGen := llmProvider.GeneratorFor(domain.PurposeMemory)
	awarenessGen := llmProvider.GeneratorFor(domain.PurposePlanner)
	supervisionGen := llmProvider.GeneratorFor(domain.PurposeVerifier)
	metabolismGen := llmProvider.GeneratorFor(domain.PurposeInterview) // ADR-0037 interview grading

	// Register LLM models as TraitModel agents so they participate in the auction.
	registerModelAgents(reg, cfg.LLMProvider.Generators)
	// ADR-0042: config is the source of truth for the model population. Eviction
	// of models dropped from config — registration above is upsert-only, so a
	// removed model would otherwise survive in the registry and keep winning the
	// auction after a restart (the qwen-after-removal orphan bug).
	reconcileModelAgents(ctx, reg, cfg.LLMProvider.Generators)
	// Same orphan class for filesystem agents: the bbolt seeder is upsert-only, so
	// an agent whose source file was deleted lingers in the registry. Evict those
	// whose ExecPath no longer exists on disk; A2A/dynamic agents are spared.
	reconcileFilesystemAgents(ctx, reg, func(p string) bool { _, err := os.Stat(p); return err == nil })

	// 2. Domain stacks — sequential construction (dependency order)
	mem := kernel.NewMemoryStack(vec, memoryGen, embedder, cfg.Execution)
	// ADR-0048 #1: let Tier-2 commit offload a promoted fact's full body to CAS so
	// recall serves {summary + content_cid} instead of the full text.
	mem.Agent.ContentStore = storeHandle.ContentStore

	// ADR-0034 Phase 1: enable agent_scope enforcement on the agent-facing memory
	// query path. The resolver disambiguates registered-but-unprofiled (unrestricted)
	// from unknown principals (fail-closed) via the registry; the QueryService is
	// rewired with a fail-closed ScopedVectorStore. Only agent_scope (non-forgeable)
	// is enforced; caller-supplied Handoff.Context tags carry no weight until Phase 2.
	scopeResolver.SetExister(reg)
	mem.QueryService.EnableScoping(scopeResolver, scope.NewScopedVectorStore(vec, slog.Default()))

	pp := config.NewStaticPolicyProvider(cfg.Execution.HippocampusPolicies, cfg.Execution.HippocampusDefaultPolicy)
	aw := kernel.NewAwarenessStack(awarenessGen, reg, mem.Hippocampus, mem.WorkspaceStage, pp)
	meta := kernel.NewMetabolismStack(reg, embedder, vec, mem.ProfileStore, mem.Agent, cfg, observer, metabolismGen)
	sup := kernel.NewSupervisionStack(reg, mem.ProfileStore, mem.VecDB, supervisionGen, cfg, observer)

	// 3. Wire storage callback so newly-registered agents are enqueued for interview.
	storeHandle.WireInterviewEnqueuer(meta.InterviewEnqueuer())

	// 3b. Wire CapabilityClusterer as the SweepTrigger for the InterviewWorker.
	// Runs after both stacks are constructed — no stack-to-stack import needed.
	meta.InterviewWorker.SweepTrigger = sup.Clusterer

	// 3c. Wire EventBus (ADR-0030). Both InterviewWorker and Auctioneer publish to it;
	// other subsystems subscribe. A log handler makes AgentReadyEvent observable in
	// production without a dedicated subscriber (ADR-0023 D6A observability).
	eventBus := domain.NewInMemoryEventBus()
	// ADR-0047 0047-14: the LLM provider's circuit breaker publishes LLMHealthEvent
	// on an open↔closed transition for the operator feed.
	llmProvider.SetHealthEventBus(eventBus)
	eventBus.Subscribe(domain.EventTypeAgentReady, func(e domain.DomainEvent) {
		if ev, ok := e.(domain.AgentReadyEvent); ok {
			slog.Info("agent_ready",
				"agent_id", ev.AgentID,
				"source_hash", ev.SourceHash,
				"trust_score", ev.TrustScore,
				"capabilities", ev.Capabilities,
				"interview_ms", ev.InterviewMs,
			)
		}
	})

	// ADR-0034 (D11): scope promotion pipeline. The ConsolidatorAgent reads raw
	// Tier-0 docs under ScopeConsolidator (secrets/PII excluded), clusters by theme,
	// and writes k-anonymized, regex-scrubbed insights to broader scope through the
	// ScopedStoreWriter. Triggered on MemoryPressureEvent (cross-session batch, never
	// per-session — one session can't satisfy the anonymity floor). Safety rests on
	// deterministic gates (scope + counted k-floor + masker), never on the LLM.
	promoter := scope.NewPromoter(
		memory.NewConsolidatorReader(vec, 0),
		scope.NewCosineThemeClusterer(cfg.Execution.WorkspaceDriftThreshold),
		memory.NewLLMGeneralizer(memoryGen),
		domain.NewRegexPIIMasker(),
		memory.NewConsolidatorWriter(mem.WriteStore, embedder),
		scope.NewInMemoryLedger(),
		cfg.Execution.KAnonymityFloor,
		slog.Default(),
	)
	eventBus.Subscribe(domain.EventTypeMemoryPressure, func(_ domain.DomainEvent) {
		// Lookback floor = full history minus already-promoted clusters (ledger).
		go func() {
			if err := promoter.PromoteBatch(context.Background(), time.Time{}); err != nil {
				slog.Warn("ADR-0034: scope promotion batch failed", "err", err)
			}
		}()
	})
	meta.InterviewWorker.EventBus = eventBus
	meta.Auctioneer.EventBus = eventBus
	meta.VerificationWorker.EventBus = eventBus // ADR-0047 D3: VerifierRoundEvent → operator feed
	// ADR-0033: crash detection publishes DaemonCrashedEvent to this bus.
	meta.Manager.EventBus = eventBus
	// ADR-0049 §A1.2: the MemoryAgent publishes passive world_delta drift signals when a
	// read observes an entity field changed from its cached value (consumed by ADR-0051
	// Scout staleness + deferred ADR-0037 adaptive trust).
	mem.Agent.EventBus = eventBus

	// 4. Watcher — proactive signal processing (ADR-0009)
	watcher := supwatcher.New(
		meta.Manager,
		mem.Agent,
		aw.Planner,
		supwatcher.WatcherConfig{
			SignalNoiseThreshold:  cfg.Execution.SignalNoiseThreshold,
			SignalNoiseWindowSecs: cfg.Execution.SignalNoiseWindowSecs,
		},
	)

	// 5. SessionManager — episodic memory lifecycle (ADR-0012)
	sessionMgr := session.New(reg)

	// ADR-0034 Phase 2: caller_scope re-derivation. When a session carries a
	// non-forgeable caller_scope (persisted server-side), agent reads enforce
	// effective = caller_scope ∩ agent_scope, sourced from the session record —
	// never from Handoff.Context. Falls back to Phase-1 agent_scope when absent.
	mem.QueryService.EnablePhase2(scopeResolver, sessionMgr)

	// ADR-0053 Phase 0: KG²RAG one-hop chunk expansion (config-gated). The
	// pgvector adapter doubles as the ChunkTripletsStore (per-chunk h, r, t
	// extracted at write time or via the offline chunk-fill CLI). The
	// pipeline walks per-chunk triplets from the seed chunks, pulls in
	// chunks that share entities, and feeds them into the same cosine
	// re-rank. Opt-in via `execution.kg2rag_enabled` in config.json so the
	// A/B test (KG²RAG on vs off) is a config flip, not a rebuild. The
	// max-hops / max-expanded / max-entities knobs bound the expansion.
	if cfg.Execution.KG2RAGEnabled {
		mem.QueryService.EnableKG2RAG(vec,
			cfg.Execution.KG2RAGMaxHops,
			cfg.Execution.KG2RAGMaxExpanded,
			cfg.Execution.KG2RAGMaxEntities,
			cfg.Execution.KG2RAGPerEntity,
		)
		// LLM-free, structure-aware recall: seed kgExpand from entities extracted
		// from the query text (ADR-0053). Needs KG²RAG (the chunk_triplets store).
		if cfg.Execution.QueryEntitySeedingEnabled {
			mem.QueryService.EnableQueryEntitySeeding()
			slog.Info("ADR-0053: query-entity seeding ENABLED (LLM-free recall)")
		}
		// Document-local anchor promotion (companion to the deterministic anchor
		// tier). Also needs the chunk_triplets store; LLM-free. ADR-0053.
		if cfg.Execution.AnchorConstraintEnabled {
			mem.QueryService.EnableAnchorConstraint()
			slog.Info("ADR-0053: anchor constraint ENABLED (document-local anchor promotion)")
		}
	} else {
		slog.Info("ADR-0053: KG²RAG expansion DISABLED via config (kg2rag_enabled=false)")
	}

	// ADR-0054 Stage A: multi-signal blend re-rank (cosine + recency + confidence
	// + pagerank + activation). Opt-in via `execution.blend_enabled`. Reads
	// chunk_pagerank (kept fresh by the pagerank-recompute worker) + per-chunk
	// confidence; bge cross-encoder (Stage B) is a separate, later flag.
	if cfg.Execution.BlendEnabled {
		w := memory.BlendWeights{
			Cosine:         cfg.Execution.BlendWeightCosine,
			Recency:        cfg.Execution.BlendWeightRecency,
			Confidence:     cfg.Execution.BlendWeightConfidence,
			PageRank:       cfg.Execution.BlendWeightPageRank,
			Activation:     cfg.Execution.BlendWeightActivation,
			Lexical:        cfg.Execution.BlendWeightLexical,
			GraphCoherence: cfg.Execution.BlendWeightCoherence,
		}
		if (w == memory.BlendWeights{}) {
			w = memory.DefaultBlendWeights() // all-unset ⇒ ADR defaults
		}
		blender := memory.NewBlender(w)
		mem.QueryService.EnableBlend(&blender, vec) // *PgVectorAdapter implements RankSignalStore
		slog.Info("ADR-0054: Stage-A multi-signal blend ENABLED", "weights", w)
	}

	// ADR-0054 hybrid retrieval: fuse dense (vector) + sparse (lexical/full-text)
	// via RRF so exact-token chunks the embedder misses enter the pool. Opt-in via
	// execution.hybrid_search_enabled. *PgVectorAdapter implements LexicalSearcher.
	if cfg.Execution.HybridSearchEnabled {
		if lex, ok := any(vec).(memory.LexicalSearcher); ok {
			mem.QueryService.EnableHybrid(lex, cfg.Execution.HybridRRFK)
			mem.QueryService.SetLexicalWeight(cfg.Execution.HybridLexicalWeight)
			slog.Info("ADR-0054: hybrid dense+lexical retrieval ENABLED", "rrf_k", cfg.Execution.HybridRRFK, "lexical_weight", cfg.Execution.HybridLexicalWeight)
		} else {
			slog.Warn("ADR-0054: hybrid_search_enabled but vector store has no LexicalSearch; vector-only")
		}
	}

	// ADR-0054 Stage B: cross-encoder rerank of the top-K Stage-A candidates via
	// the warm reranker_agent system organ (bge cross-encoder), invoked DIRECTLY
	// through the Auctioneer (no auction) — the same privileged-organ pattern as
	// the kg_extractor. Opt-in via execution.reranker_enabled. Fail-soft: a
	// down/erroring agent leaves the Stage-A order intact. The model id is the
	// agent's RERANK_MODEL env (large = ceiling, base/v2-m3 = CPU edge).
	if cfg.Execution.NeighborWindowEnabled {
		mem.QueryService.EnableNeighborWindow()
		slog.Info("ADR-0060: neighbor-window expansion ENABLED")
	}
	if cfg.Execution.RerankerEnabled {
		mem.QueryService.EnableReranker(
			&subnetwork.RerankerDispatcher{Auctioneer: meta.Auctioneer, AgentID: "reranker_agent"},
			cfg.Execution.RerankerTopK,
			cfg.Execution.RerankerWeight,
		)
		slog.Info("ADR-0054: Stage-B cross-encoder rerank ENABLED",
			"top_k", cfg.Execution.RerankerTopK, "w_bge", cfg.Execution.RerankerWeight)
	}

	// 6. EventLogger — unified event stream for session narrative
	eventLogger := subsynaptic.New(reg)

	// 7. SynapticWatcher — background observer ingesting high-priority events to LTM
	synapticWatcher := supsynaptic.New(reg, mem.Agent)

	// 8. EpisodicExtractor + MemoryLifecycleManager (ADR-0029 + ADR-0030).
	episodicExtractor := awareness.NewEpisodicExtractor(awarenessGen, vec, domain.NewRegexPIIMasker())
	consolidationDelay := time.Duration(cfg.Execution.EpisodicConsolidationDelayMs) * time.Millisecond
	consolidator := &episodicConsolidator{
		extractor:          episodicExtractor,
		eventLogger:        eventLogger,
		consolidationDelay: consolidationDelay,
	}
	ttl := time.Duration(cfg.Execution.SessionTTLDays) * 24 * time.Hour
	mlm := circadian.NewMemoryLifecycleManager(sessionMgr, consolidator, eventBus, ttl)

	// Wire SessionManager so it publishes SessionDormantEvent on state transition.
	sessionMgr.SetEventBus(eventBus)
	sessionMgr.SetTTL(ttl)

	// CircadianRhythm now only handles session token eviction (ADR-0018 sweep).
	circadianRhythm := circadian.New(sessionMgr, 0)

	// ADR-0018: Construct LLM Gateway and wire lifecycle consumers.
	llmGateway := subnetwork.NewLLMGateway(cfg.Execution)
	llmGateway.Observer = observer
	// ADR-0018 + ADR-0042: register a streaming client for EVERY configured
	// generator (keyed "llm:<id>"), not just the local Ollama one. OpenAIClient and
	// AnthropicClient implement domain.LLMStreamer too, so cognitive agents (the
	// GenerateViaModelStream path) can be served by a cloud model, and the auction's
	// StepAllocation winner/fallbacks resolve to the right backend — including
	// cross-provider failover. Previously only the Ollama generator was registered,
	// so a config without a local model (e.g. deepseek-only) left the streaming
	// gateway with no client and every agent generate failed with
	// "model_unavailable: all candidates degraded".
	streamers, serr := llm.NewStreamersFromGenerators(cfg.LLMProvider.Generators)
	if serr != nil {
		slog.Warn("ADR-0018: some generators have no streaming client", "err", serr)
	}
	for id, s := range streamers {
		llmGateway.RegisterModelClient(id, s)
	}
	llmGateway.SetClientFactory(func(modelID string) (domain.LLMStreamer, error) {
		if s, ok := streamers[modelID]; ok {
			return s, nil
		}
		return nil, fmt.Errorf("no streaming client for model %s", modelID)
	})
	// Interview grading mints a session against a concrete model. Prefer a local
	// Ollama generator (cheap, no egress) when one is configured; otherwise use the
	// configured default generator — which is now streamable like any other.
	streamingModelKey := "llm:" + cfg.LLMProvider.Default
	if og := cfg.LLMProvider.OllamaGenerator(); og != nil {
		streamingModelKey = "llm:" + og.ID
	}
	// Resilience: when a step's auction produced no model winner (e.g. no TraitModel
	// agent is registered/matched), the streaming gateway falls back to the
	// configured default model so the agent still generates — the same default that
	// already serves the organs via the broker.
	llmGateway.SetDefaultModelID("llm:" + cfg.LLMProvider.Default)
	circadianRhythm.SessionEvictor = llmGateway
	circadianRhythm.SessionSweepInterval = time.Duration(cfg.Execution.SessionTokenSweepIntervalSeconds) * time.Second

	// ADR-0039: sandboxed-evaluation session set. The interview runner Marks each
	// minted scenario session here; the ToolExecutor consults it to auto-approve
	// dangerous tools during an unattended interview (the sandbox is the boundary,
	// not a human). Shared between the interview (write) and the executor (read).
	evalSessions := domain.NewInMemoryEvaluationSessions()

	// ADR-0037 interview grading: the graded interview executes scenarios against
	// agents, which call the budgeted GenerateViaModelStream — so it needs to mint
	// managed LLM sessions. Wire the gateway in now that it exists (it is built
	// after the metabolism stack). Model ID mirrors the registered streaming client.
	meta.SetInterviewSession(llmGateway, streamingModelKey, evalSessions)

	// ADR-0018: Wire adaptive token sizing to the Planner via ProfileAggregator.
	aw.Planner.SetAdvisor(sup.ProfileAggregator)

	// 9. ArtifactVault — content-addressable storage for agent outputs
	vaultPath := filepath.Join(cfg.Storage.DataDir, "vault")
	artifactVault := vault.NewArtifactVault(vaultPath)

	// 10. Server assembly — the consumer, not a producer
	// ADR-0032: ReactiveEngineArgs wires the premium ReactiveEngine with real deps.
	// ADR-0057: reactive signal receiver via injection (no build tags). OSS default
	// is the Watcher (LTM enrichment + Planner dispatch); the premium binary injects
	// a ReactiveEngine built from the ReactiveServices bundle.
	var signalRcv domain.SignalReceiver
	if watcher != nil {
		signalRcv = watcher
	} else {
		signalRcv = &ossreactive.NoOpSignalReceiver{}
	}
	var watchHandler domain.WatchConfigHandler
	if opts.NewSignalReceiver != nil {
		signalRcv, watchHandler = opts.NewSignalReceiver(ReactiveServices{
			Manager:    meta.Manager,
			Auctioneer: meta.Auctioneer,
			Memory:     mem.Agent,
			Planner:    aw.Planner,
			LLM:        aw.LLM,
			WatchStore: reg,
			EventBus:   eventBus,
		})
	}
	cambrianServer := kernel.ProvideServer(cfg.Execution, mem, aw, meta, watcher, providers, llmProvider, sessionMgr, eventLogger, llmGateway, observer, storeHandle.ContentStore, storeHandle.StepCache, signalRcv, watchHandler)

	// ADR-0037: honor the resource_selector flag. Until now the Central-Executive
	// EFE arm was implemented + unit-tested but never composed, so useEFE() was
	// always false and every dispatch fell to the Auctioneer regardless of config.
	// Wire the Gatekeeper-backed EFE selector here. "auction" leaves ResourceSelector
	// nil (auction only); "efe"/"auto" enables the selector and useEFE() routes per
	// AssignVariant(SelectorMode, EFETrafficPercent, sessionID).
	cambrianServer.SelectorMode = cfg.Execution.ResourceSelector
	cambrianServer.EFETrafficPercent = cfg.Execution.EFETrafficPercent
	if (cfg.Execution.ResourceSelector == "efe" || cfg.Execution.ResourceSelector == "auto") &&
		meta.Auctioneer != nil && meta.Auctioneer.Gatekeeper != nil {
		cambrianServer.ResourceSelector = centralexec.NewGatekeeperEFESelector(
			meta.Auctioneer.Gatekeeper, cfg.Execution.EFEExplorationBonus)
		slog.Info("ADR-0037: EFE resource selector wired",
			"mode", cfg.Execution.ResourceSelector,
			"efe_traffic_percent", cfg.Execution.EFETrafficPercent,
			"exploration_bonus", cfg.Execution.EFEExplorationBonus)
	}

	// OBSERVABILITYREQ REQ1 / ADR-0057: AgentCallLogger is an injected hook (Options).
	// nil in OSS (no call logging); the premium binary injects a Langfuse logger.
	// GenerateViaModelStream nil-checks the field before use.
	cambrianServer.AgentCallLogger = opts.AgentCallLogger

	// ADR-0042: agent-step (RecommendedModel) generations are Acquired from the
	// Provider, which already applies the Langfuse trace wrapper — so GenWrapper is
	// left nil to avoid double-tracing. (wrapGen is identity when GenWrapper is nil.)

	// ADR-0034 / REQ-SDK-007c: wire scope-enforced artifact storage.
	cambrianServer.ArtifactBytes = artifactVault
	cambrianServer.ArtifactMeta = reg
	cambrianServer.ArtifactScopes = scopeResolver
	cambrianServer.ArtifactSessions = sessionMgr // ADR-0034 Phase 2: session caller_scope parity
	cambrianServer.ArtifactVocab = scope.NewVocabulary(cfg.Execution.ClassificationVocabulary)

	// ADR-0035 C2: memory.remember() write-back through the ScopedStoreWriter with
	// kernel-derived classification (DefaultWriteTags) + stamped provenance.
	rememberSvc := memory.NewRememberService(mem.WriteStore, embedder, scopeResolver)
	rememberSvc.SetEventBus(eventBus) // ADR-0047 0047-15: publish MemoryWrittenEvent on save
	// ADR-0015: a remembered fact with activation 0 can never clear the recall floor
	// (cosine·α < RecallSimilarityFloor) — seed a recallable default (LoCoMo-tunable).
	rememberSvc.SetDefaultActivation(cfg.Execution.RememberDefaultActivation)
	// ADR-0052: wire the BATCHED LLM-based entity+relation extractor. One LLM
	// call per batch (default 32 facts) instead of one per ingest; ~32x fewer
	// LLM calls. The batcher is non-blocking; the doc is still saved even if
	// the queue is full.
	if mem.EdgeBatcher != nil {
		rememberSvc.SetEdgeBatcher(mem.EdgeBatcher)
	}
	// ADR-0053 D2 (revised): wire the per-chunk (h, r, t) extractor. The
	// producer is swapped in main.go above (UseExtractor): the kg_extractor
	// system agent when execution.kg_extractor_enabled is true, else the
	// LLM residue adapter.
	if mem.ChunkTripletsBatcher != nil {
		rememberSvc.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	}
	cambrianServer.MemoryWriter = rememberSvc

	// ADR-0060 D8/D9: route the gRPC IngestMemory through the
	// chunking pipeline. The IngestionManager (constructed by
	// NewMemoryStack alongside the DirectoryWatcher, ADR-0028)
	// chunks the body, mints a source-doc entity, and ingests each
	// chunk with chunk_relations.parent_entity_id set. The
	// SourceType/extension drives the chunker registry's
	// Resolve(sourceType, ext) lookup; documents with no extension
	// fall back to OptionC. The gRPC handler falls back to
	// MemoryWriter when the IngestionManager is nil (legacy path).
	if mem.IngestionManager != nil {
		cambrianServer.IngestionProcessor = mem.IngestionManager
		mem.IngestionManager.SetSceneGenEnabled(cfg.Execution.SceneGenOnIngestEnabled)
		// ADR-0053: also feed the document-ingest path's chunks to the triplet/
		// anchor extractor, so uploaded documents populate chunk_triplets (KG2RAG,
		// query-entity seeding, anchor promotion). Without this the batcher only
		// sees the RememberService path, and uploaded docs get zero triplets.
		if mem.ChunkTripletsBatcher != nil {
			mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
		}
		// ADR-0060: structure-aware ingestion — parse each document's real hierarchy
		// via the docling_agent and persist a structure graph (sections + PART_OF/NEXT
		// edges), stamping every chunk with its inherited section path. Opt-in.
		if cfg.Execution.StructureGraphEnabled {
			mem.IngestionManager.SetStructureGraph(
				&subnetwork.DoclingDispatcher{Auctioneer: meta.Auctioneer},
				vec.StructureGraphStore(),
			)
			mem.QueryService.EnableSectionScopedRetrieval(vec)
			slog.Info("ADR-0060: structure-aware ingestion ENABLED (docling parse -> structure graph)")
		}
	}

	// ADR-0041: expose the kernel embedder for the agent Local Recurrent Workspace
	// relevance ranking (the Embed RPC). Read-only; no authorization impact.
	cambrianServer.Embedder = embedder

	// ADR-0037 D10–D15 (ADR-0041 follow-up 0041-07): wire the YieldDriver so a
	// yielding agent's sub-goal is bound (via the live selector — Zero-Hardcode),
	// dispatched, and the parent resumed. Reuses the kernel embedder for the D15
	// narrowing guard. Returns nil (yields inert) when no EFE selector is wired.
	cambrianServer.YieldDriver = subnetwork.NewYieldDriver(
		cambrianServer.ResourceSelector, meta.Auctioneer, embedder, 0.15, 8)

	// ADR-0039: kernel-owned tool registry. Tools are auto-discovered from the
	// tools/ dir (no Go registration); the executor authorizes every call
	// (grant + resource policy + scope + approval) and runs it in a confined
	// Python child. Default: no grants ⇒ no system tools (fail-closed).
	toolReg := domain.NewInMemoryToolRegistry()
	toolFiles, terr := tooldiscovery.LoadRegistry("tools", toolReg)
	if terr != nil {
		slog.Warn("tool discovery failed; system tools disabled", "err", terr)
	}

	// ADR-0043: connect configured external MCP servers, discover their tools into
	// the registry as mcp:<server>/<tool>, and expose the MCP handler. Opt-in —
	// no servers configured ⇒ mcpHandler stays nil and nothing changes. A server
	// that fails to connect is skipped (graceful degradation, D8).
	var mcpHandler domain.ToolHandler
	var mcpConnector *mcp.Connector
	var mcpPricing domain.ToolPricingSource
	var mcpBudget *domain.BudgetLedger
	var mcpAuditor domain.EgressAuditor
	var mcpServers []mcp.ServerConfig
	if len(cfg.MCP.Servers) > 0 {
		mcpConnector = mcp.NewConnector()
		servers := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers))
		pricing := domain.MapPricingSource{}
		for _, s := range cfg.MCP.Servers {
			toolPolicy := make(map[string]mcp.ToolPolicy, len(s.Tools))
			for _, tc := range s.Tools {
				toolPolicy[tc.Name] = mcp.ToolPolicy{Dangerous: tc.Dangerous, DataWriteKinds: tc.DataWriteKinds}
				if tc.Pricing.Kind != "" {
					pricing["mcp:"+s.ID+"/"+tc.Name] = domain.ToolPricing{
						Kind:            domain.PricingKind(tc.Pricing.Kind),
						UnitCost:        tc.Pricing.UnitCost,
						MaxUnitsPerCall: tc.Pricing.MaxUnitsPerCall,
						ChargeOnFailure: domain.FailureCharge(tc.Pricing.ChargeOnFailure),
					}
				}
			}
			servers = append(servers, mcp.ServerConfig{
				ID: s.ID, Transport: s.Transport, Endpoint: s.Endpoint, Args: s.Args,
				AuthType: s.Auth.Type, AuthHeader: s.Auth.Header, AuthTokenEnv: s.Auth.TokenEnv,
				Tools: toolPolicy,
			})
		}
		mcpServers = servers
		for _, t := range mcpConnector.ConnectAll(ctx, servers) {
			toolReg.Register(t)
		}
		mcpHandler = &mcp.Handler{
			Sessions:    mcpConnector,
			CallTimeout: time.Duration(cfg.MCP.CallTimeoutMs) * time.Millisecond,
		}
		mcpPricing = pricing
		mcpBudget = domain.NewBudgetLedger()
		mcpBudget.DefaultCap = cfg.MCP.DefaultSessionBudget // 0 ⇒ tracked but unbounded
		mcpAuditor = mcp.SlogEgressAuditor{}
	}

	toolGrants := domain.NewInMemoryGrantsStore()
	toolApproval := domain.NewInMemoryApprovalController(60 * time.Second)
	// ADR-0039: the dangerous-tool approval controller. By default it is the
	// operator-gated controller (WatchApprovals/SubmitApprovalDecision). When
	// tools_auto_approve is set, dangerous tools run without a human decision —
	// the only sane mode for an unattended local/dev run (the operator RPCs have
	// no client). The process sandbox remains the containment boundary either way.
	var toolApprovalCtrl domain.ApprovalController = toolApproval
	if cfg.Execution.ToolsAutoApprove {
		toolApprovalCtrl = domain.AlwaysApproveController{}
	}
	cambrianServer.ToolExecutor = &domain.ToolExecutor{
		Registry:   toolReg,
		Grants:     toolGrants,
		MCPHandler: mcpHandler, // ADR-0043: nil ⇒ no MCP tools (opt-in)
		Handler: &toolproc.ProcessHandler{
			PythonExec:     cfg.Metabolism.PythonExecutable,
			ToolFiles:      toolFiles,
			DefaultTimeout: 30 * time.Second,
			// Deny-by-default env scrub, with an explicit passthrough allowlist for
			// the web tool's provider config (ADR-0040 web tool). Includes the
			// Firecrawl provider vars (local instance: base URL + optional token).
			EnvPassthrough: []string{
				"CAMBRIAN_WEB_PROVIDER", "CAMBRIAN_WEB_API_KEY", "CAMBRIAN_SEARXNG_URL",
				"CAMBRIAN_WEB_EXTRACT_PROVIDER", "CAMBRIAN_FIRECRAWL_URL",
				"CAMBRIAN_FIRECRAWL_API_KEY", "CAMBRIAN_FIRECRAWL_TIMEOUT",
			},
			// Sweep tool-created jail files into CAS so relative-path writes
			// (e.g. write_file "hello.txt") persist instead of being lost to the
			// per-call tempdir teardown; CIDs surface in the result "_artifacts".
			ContentStore: storeHandle.ContentStore,
		},
		Approval:        toolApprovalCtrl,
		EvalSessions:    evalSessions, // ADR-0037: interview sessions auto-approve dangerous tools
		EgressAuditor:   mcpAuditor,   // ADR-0043: nil ⇒ no egress auditing
		Budget:          mcpBudget,    // ADR-0043: nil ⇒ MCP calls unmetered
		Pricing:         mcpPricing,   // ADR-0043: nil ⇒ MCP calls unmetered
		Scope:           scopeResolver,
		ContentStore:    storeHandle.ContentStore,
		InlineThreshold: 65536,
		Unrestricted:    cfg.Execution.ToolsUnrestricted, // dev/trusted bypass: all agents, all tools
		Overlay:         domain.NewRunGrantOverlay(),     // ADR-0046 D6: skill-conferred run-scoped grants
		// Promote files a confined tool writes into the DURABLE artifact system
		// (vault + metadata, retrievable via GetArtifact, scope-governed) AND
		// materialize them to data/outputs so a requested file lands on disk —
		// instead of living only as a GC-eligible content-store CID.
		ArtifactBytes: artifactVault,
		ArtifactMeta:  reg,
		ArtifactTags: func(ctx context.Context, agentID string) []string {
			return scopeResolver.DefaultWriteTags(ctx, agentID)
		},
		ArtifactOutputDir: filepath.Join(cfg.Storage.DataDir, "outputs"),
		// ADR-0048 D6: feed substantive tool outputs into Tier-1/Tier-2 curation so
		// the LLM scorer can promote valuable ones to durable LTM. The pre-filter
		// skips errors/denials and outputs below the size floor.
		ToolOutput:         mem.Agent,
		ToolOutputMinBytes: 200,
	}
	cambrianServer.ApprovalHub = toolApproval
	toolApproval.Bus = eventBus // ADR-0047 0047-19: dangerous-tool approval raises HITLRaisedEvent
	// ADR-0047 0047-17/0047-18: the executor publishes PlanStateChanged to the
	// operator feed, and live executions register into one shared control hub so
	// operator PauseSession/ResumeSession can steer them.
	cambrianServer.EventBus = eventBus
	operatorControlHub := operator.NewExecutionControlHub()
	cambrianServer.ControlHub = operatorControlHub
	if cfg.Execution.ToolsUnrestricted {
		slog.Warn("ADR-0039: tools_unrestricted=true — ALL agents may call ALL tools (grant system bypassed). Trusted deployments only.")
	}
	if cfg.Execution.ToolsAutoApprove {
		slog.Warn("ADR-0039: tools_auto_approve=true — dangerous tools run WITHOUT operator approval. Trusted deployments only.")
	}

	// ADR-0044: index all discovered tools (native + MCP) for semantic retrieval,
	// then wire the retriever so ListTools(query) serves a task-relevant menu
	// instead of the whole registry. Reuses the kernel embedder + pgvector store;
	// the ToolExecutor depends only on the ToolRetriever port. Indexing is
	// best-effort — a failure degrades retrieval to the full menu, never blocks boot.
	toolIndexer := &domain.ToolIndexer{Store: vec, Embedder: embedder}
	if err := toolIndexer.IndexAll(ctx, toolReg.All()); err != nil {
		slog.Warn("ADR-0044: tool indexing failed (retrieval degraded to full menu)", "err", err)
	}
	// ADR-0044: prune tool index docs whose source is gone — an MCP server removed
	// from config is never connected this boot, so the sink never evicts its tools
	// and their stale docs would keep surfacing via find_tools. mcpConnector.ConnectAll
	// above is synchronous, so toolReg.All() already holds every native + connected
	// MCP tool; a tool whose MCP server is still CONFIGURED (merely unreachable this
	// boot) is kept so a transient outage does not churn the index.
	{
		currentTools := make(map[string]bool, len(toolReg.All()))
		for _, t := range toolReg.All() {
			currentTools[t.Name] = true
		}
		configuredMCP := make(map[string]bool, len(cfg.MCP.Servers))
		for _, s := range cfg.MCP.Servers {
			configuredMCP[s.ID] = true
		}
		reconcileIndex(ctx, vec, toolIndexer, domain.DocTypeTool, toolKeepFunc(currentTools, configuredMCP))
	}
	cambrianServer.ToolExecutor.Retriever = domain.VectorToolRetriever{
		Store: vec, Embedder: embedder, Floor: cfg.Execution.ToolRetrievalFloor,
	}

	// ADR-0051: the pre-plan Scout is the privileged Python `run_think` discovery agent
	// (`agents/system/scout_agent/` package; entry point `agent.py`), invoked directly via
	// the Auctioneer (no auction). Its
	// discovery LOOP — find_tools, memory_query, multi-modal read-only tool calls — lives in
	// the agent; this kernel side is a thin dispatcher + env grounding. Wired only when
	// scout_enabled is set (default off ⇒ one-shot baseline; the A/B falsification switch,
	// issue-008). Off ⇒ cambrianServer.Scout stays nil ⇒ Execute plans one-shot.
	if cfg.Execution.ScoutEnabled {
		cambrianServer.Scout = &subnetwork.AgentScoutDispatcher{
			Auctioneer:   meta.Auctioneer,
			ScoutAgentID: "scout_agent",
			Gateway:      llmGateway, // deliberate model allocation (not the default fallback)
			ScoutModel:   cfg.Execution.ScoutModel,
		}
		// ADR-0051 D6: confine the scout_agent principal to the operator's `discovery-safe`
		// tools — a hard ceiling that overrides tools_unrestricted (Scout fires unattended at
		// plan time). Only set when configured, so dev (unrestricted) still works; unset ⇒
		// Scout finds tools under unrestricted in dev, none in a default prod run (degrades).
		if len(cfg.Execution.DiscoverySafeTools) > 0 {
			safe := make(map[string]bool, len(cfg.Execution.DiscoverySafeTools))
			for _, name := range cfg.Execution.DiscoverySafeTools {
				safe[name] = true
			}
			cambrianServer.ToolExecutor.RestrictedTools = map[string]map[string]bool{"scout_agent": safe}
		}
		slog.Info("ADR-0051: pre-plan Scout ENABLED (Python run_think discovery agent)")
	}

	// AGENTIC_RETRIEVAL_SPEC Phase 2a: wire the LLM query-planner (retrieval_agent)
	// as the QueryService's Planner — invoked directly via the Auctioneer (no
	// auction) with a managed LLM session, the same privileged-organ + session
	// pattern as the Scout. Default off; fail-open to the single pass when the
	// agent is unreachable or the config flag is unset.
	if cfg.Execution.AgenticRetrievalEnabled {
		mem.QueryService.EnableAgenticRetrieval(&subnetwork.RetrievalDispatcher{
			Auctioneer: meta.Auctioneer,
			AgentID:    "retrieval_agent",
			Gateway:    llmGateway,
			Model:      cfg.Execution.AgenticPlannerModel, // "" ⇒ gateway default; a FAST model matters on the hot path
		}, cfg.Execution.AgenticMaxHops)
		mem.QueryService.SetHydeEnabled(cfg.Execution.HydeEnabled)
		mem.QueryService.SetIrcotEnabled(cfg.Execution.AgenticIrcotEnabled)
		mem.QueryService.SetDecomposeEnabled(cfg.Execution.AgenticDecomposeEnabled)
		slog.Info("AGENTIC_RETRIEVAL_SPEC: agentic retrieval query-planner ENABLED (retrieval_agent)", "hyde", cfg.Execution.HydeEnabled, "ircot", cfg.Execution.AgenticIrcotEnabled, "decompose", cfg.Execution.AgenticDecomposeEnabled)
	}

	// ADR-0053 D2 (revised): route write-time chunk-triplet extraction through the
	// deterministic, NO-LLM kg_extractor system agent (metadata + spacy_patterns),
	// invoked DIRECTLY via the Auctioneer (no auction) — the same privileged-organ
	// pattern as the Scout. Off (default) ⇒ the batcher keeps its LLM extractor.
	// Injected before mem.Start so the swap is in place when the drain begins.
	if cfg.Execution.KgExtractorEnabled && mem.ChunkTripletsBatcher != nil {
		mem.ChunkTripletsBatcher.UseExtractor(&subnetwork.KgExtractorDispatcher{
			Auctioneer: meta.Auctioneer,
			AgentID:    "kg_extractor_agent",
		})
		slog.Info("ADR-0053 D2: kg_extractor ENABLED (deterministic metadata + spacy_patterns organ, no LLM)")
	}

	// ADR-0046: discover authored system skills from skills/<name>/SKILL.md and
	// index them as DocTypeSkill for semantic retrieval (the analog of tool
	// discovery + indexing above). Agent skills are SDK-local and never indexed
	// here. Best-effort — a failure leaves the system-skill menu empty, never
	// blocks boot. The SkillRetriever + ListSkills wiring follows in ADR-0046-02.
	skillReg := domain.NewInMemorySkillRegistry()
	discoveredSkills, skerr := skilldiscovery.LoadRegistry("skills", skillReg)
	if skerr != nil {
		slog.Warn("ADR-0046: skill discovery failed; system skills disabled", "err", skerr)
	}
	skillIndexer := &domain.SkillIndexer{Store: vec, Embedder: embedder}
	if err := skillIndexer.IndexAll(ctx, discoveredSkills); err != nil {
		slog.Warn("ADR-0046: skill indexing failed (system skills unavailable)", "err", err)
	}
	// ADR-0046: prune skill index docs whose SKILL.md was removed from disk. Skills
	// are disk-only (rebuilt synchronously each boot), so discoveredSkills is the
	// complete current set — a simple set-diff, no MCP/unreachable caveat.
	{
		currentSkills := make(map[string]bool, len(discoveredSkills))
		for _, s := range discoveredSkills {
			currentSkills[s.Name] = true
		}
		reconcileIndex(ctx, vec, skillIndexer, domain.DocTypeSkill, func(id string) bool {
			return currentSkills[id]
		})
	}
	// ADR-0046 D2/D4/D9: wire the system-skill plane behind ListSkills. The
	// retriever reads through a fail-closed ScopedVectorStore so an agent only
	// retrieves skills its effective scope permits (the same read path as memory).
	cambrianServer.SkillRegistry = skillReg
	cambrianServer.SkillScope = scopeResolver
	cambrianServer.SkillRetriever = domain.VectorSkillRetriever{
		Store:    scope.NewScopedVectorStore(vec, slog.Default()),
		Embedder: embedder,
		Floor:    cfg.Execution.ToolRetrievalFloor,
	}

	// ADR-0043 D8 / ADR-0044 re-sync: the sink keeps the registry + the retrieval
	// index in step as servers drop and reconnect; the connector's Watch loop
	// (started as a background service) drives it. Seeded with the boot-time MCP
	// tools so a later drop knows exactly what to remove.
	var mcpSink mcp.ToolSink
	if mcpConnector != nil {
		sink := newMCPToolSink(toolReg, toolIndexer)
		sink.Seed(toolReg.All())
		mcpSink = sink
	}

	// ADR-0047 0047-16/0047-25: real CommandEffects bound to kernel surfaces.
	// TagMemory follows Amendment A1.2: operators may widen AND narrow tags, from
	// the controlled vocabulary, written through the kernel store (not a raw DB
	// write), audited by the command path.
	operatorTagVocab := scope.NewVocabulary(cfg.Execution.ClassificationVocabulary)
	operatorEffects := operator.CommandEffectsFuncs{
		TagMemoryFn: func(ctx context.Context, docID, tag string, add bool) error {
			if !operatorTagVocab.Contains(tag) {
				return fmt.Errorf("tag %q is not in the controlled vocabulary", tag)
			}
			doc, err := vec.GetByID(ctx, docID)
			if err != nil || doc == nil {
				return fmt.Errorf("tag_memory: document %s not found: %w", docID, err)
			}
			tags := stringSliceFromMeta(doc.Metadata["tags"])
			tags = applyTag(tags, tag, add)
			if doc.Metadata == nil {
				doc.Metadata = map[string]interface{}{}
			}
			doc.Metadata["tags"] = tags
			return vec.Save(ctx, doc)
		},
		SetScopeFn: func(ctx context.Context, agentID string, required, anyOf, forbidden []string) error {
			return scopeResolver.SaveScope(ctx, agentID, domain.ScopeConfig{
				RequiredTags: required, AnyOfTags: anyOf, ForbiddenTags: forbidden,
			})
		},
		RegisterSkillFn: func(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error {
			sk := domain.Skill{Name: name, Description: description, Instructions: instructions, ToolGrants: toolGrants, ScopeTags: scopeTags}
			skillReg.Register(sk)
			return skillIndexer.Index(ctx, sk)
		},
		RegisterMCPFn: func(ctx context.Context, name, command, url string) error {
			if mcpConnector == nil {
				return fmt.Errorf("mcp connector not enabled")
			}
			cfg := mcp.ServerConfig{ID: name, Transport: "stdio", Endpoint: command}
			if url != "" {
				cfg.Transport, cfg.Endpoint = "http", url
			}
			tools := mcpConnector.ConnectAll(ctx, []mcp.ServerConfig{cfg})
			if mcpSink != nil {
				mcpSink.SetServerTools(ctx, name, tools) // registers + indexes (ADR-0044)
			}
			return nil
		},
		TriggerConsolidationFn: func(_ context.Context, scope string) error {
			return eventBus.Publish(domain.MemoryPressureEvent{Trigger: "operator:" + scope})
		},
		// ADR-0054 tuning seam: hot-apply Stage-A blend weights live (no restart).
		// Merges the provided params over the current live weights; unknown keys are
		// logged + ignored. Ephemeral — config.json remains the boot default.
		SetRuntimeConfigFn: func(_ context.Context, params map[string]float64) error {
			w := mem.QueryService.CurrentBlendWeights()
			applied := 0
			for k, v := range params {
				switch k {
				case "blend_weight_cosine":
					w.Cosine = v
				case "blend_weight_lexical":
					w.Lexical = v
				case "blend_weight_coherence":
					w.GraphCoherence = v
				case "blend_weight_confidence":
					w.Confidence = v
				case "blend_weight_pagerank":
					w.PageRank = v
				case "blend_weight_recency":
					w.Recency = v
				case "blend_weight_activation":
					w.Activation = v
				default:
					slog.Warn("SetRuntimeConfig: unknown tunable ignored", "key", k, "value", v)
					continue
				}
				applied++
			}
			if applied == 0 {
				return fmt.Errorf("no known tunables in params (supported: blend_weight_{cosine,lexical,coherence,confidence,pagerank,recency,activation})")
			}
			mem.QueryService.SetBlendWeights(w)
			slog.Info("SetRuntimeConfig: blend weights hot-applied (ephemeral)", "weights", w, "applied", applied)
			return nil
		},
	}

	return &Kernel{
		OperatorEffects:    operatorEffects,
		OperatorAudit:      operatorAudit,
		Config:             cfg,
		Registry:           reg,
		Store:              storeHandle,
		Memory:             mem,
		Awareness:          aw,
		Metabolism:         meta,
		Supervision:        sup,
		Server:             cambrianServer,
		Listener:           lis,
		SessionMgr:         sessionMgr,
		EventLogger:        eventLogger,
		SynapticWatcher:    synapticWatcher,
		CircadianRhythm:    circadianRhythm,
		MemoryLifecycleMgr: mlm,
		ArtifactVault:      artifactVault,
		EventBus:           eventBus,
		ScopeResolver:      scopeResolver,
		ScopeStore:         scopeStore,
		ToolGrants:         toolGrants,
		MCPConnector:       mcpConnector,
		MCPSink:            mcpSink,
		MCPServers:         mcpServers,
	}, nil
}

func startKernelServices(g *errgroup.Group, ctx context.Context, k *Kernel) {
	// ADR-0034: subscribe to cross-replica scope invalidations (LISTEN/NOTIFY).
	if k.ScopeStore != nil && k.ScopeResolver != nil {
		if ch, err := k.ScopeStore.Subscribe(ctx); err != nil {
			slog.Warn("ADR-0034: scope invalidation subscribe failed (TTL-only revocation)", "err", err)
		} else {
			g.Go(func() error {
				k.ScopeResolver.WatchInvalidations(ctx, ch)
				return nil
			})
		}
	}

	// A. Domain stack background workers
	g.Go(func() error { return k.Memory.Start(ctx) })
	g.Go(func() error { return k.Metabolism.Start(ctx) })
	g.Go(func() error { return k.Supervision.Start(ctx) })

	// B. Synaptic Bridge background workers (ADR-0012)
	g.Go(func() error {
		k.SynapticWatcher.Start(ctx)
		return nil
	})
	g.Go(func() error {
		k.CircadianRhythm.Start(ctx)
		return nil
	})
	g.Go(func() error {
		k.MemoryLifecycleMgr.Start(ctx)
		return nil
	})

	// ADR-0043 D8 / ADR-0044: MCP server health + reconnect loop. On a drop it
	// removes the server's tools from the menu + index; on reconnect it re-discovers
	// and re-indexes (re-sync). No-op when no MCP servers are configured.
	if k.MCPConnector != nil && k.MCPSink != nil && len(k.MCPServers) > 0 {
		g.Go(func() error {
			k.MCPConnector.Watch(ctx, k.MCPServers, k.MCPSink, 30*time.Second)
			return nil
		})
	}

	// B. Async Backfill (Brain Integrity)
	g.Go(func() error {
		slog.Info("🧬 Kernel: Starting Brain Integrity verification (Backfill)...")
		backfillCfg := backfill.BackfillConfig{TimeoutMs: 60000}
		if err := backfill.RunInterviewBackfill(ctx, k.Registry, k.Memory.ProfileStore, k.Metabolism.InterviewWorker, k.Memory.Embedder, backfillCfg); err != nil {
			return fmt.Errorf("brain integrity failure: %w", err)
		}
		slog.Info("✅ Kernel: Brain Integrity verified.")
		return nil
	})

	// C. gRPC Server
	g.Go(func() error {
		// ADR-0047: Operator Transport Plane auth (D13). The interceptors are
		// method-scoped to /cambrian.OperatorConsole/* — agent-facing RPCs over
		// UDS pass through ungated. A bootstrap operator is seeded from env
		// (secure-by-default: no creds ⇒ no login). The production identity
		// backend swaps in behind the OperatorIdentity port.
		operatorIDP := operatorBootstrapIdentity()
		k.GRPC = grpc.NewServer(
			grpc.ChainUnaryInterceptor(operator.UnaryAuthInterceptor(operatorIDP)),
			grpc.ChainStreamInterceptor(operator.StreamAuthInterceptor(operatorIDP)),
		)
		pb.RegisterOrchestratorServer(k.GRPC, k.Server)
		// The sequenced operator feed for the Cambrian UI. The spool decouples the
		// synchronous EventBus from network clients; SubscribeBridge fans the
		// existing domain events into it; the projection folds plan state.
		operatorFeed := operator.NewSpool(operator.SpoolConfig{})
		operator.SubscribeBridge(k.EventBus, operatorFeed)
		operatorProjection := operator.NewProjection()
		operator.SubscribeProjection(k.EventBus, operatorProjection)
		// ADR-0047 0047-23: fork managed-proxy generation chunks onto the feed's
		// live-only token lane.
		k.Server.TokenSink = func(sessionID string, stepIndex int, text string) {
			operatorFeed.EmitEphemeral(domain.TokenChunkEvent{SessionID: sessionID, StepIndex: stepIndex, Text: text})
		}
		operatorSvc := operator.NewService(operatorFeed)
		operatorSvc.SetSnapshotSources(operatorProjection, k.SessionMgr)
		operatorSvc.SetIdentity(operatorIDP)
		// ADR-0047 D15/0047-24: mutating commands (audit-stamped, idempotent) over
		// the durable Postgres operator_audit store (in-memory fallback).
		operatorSvc.SetCommandSources(k.OperatorAudit, k.ToolGrants)
		// ADR-0047 D11: steering. The control hub is the rendezvous a live
		// DAGExecutor registers into (registration is the executor-producer side);
		// HITL resolution reuses the kernel ApprovalHub.
		operatorSvc.SetSteeringSources(k.Server.ControlHub, k.Server.ApprovalHub)
		// ADR-0047 0047-07/0047-16: remaining mutations. SetScope/RegisterSkill/
		// RegisterMCP/TriggerConsolidation are bound to real kernel surfaces;
		// TagMemory is gated on the 0047-20 decision (Unimplemented for now).
		operatorSvc.SetCommandEffects(k.OperatorEffects)
		// ADR-0047 Amendment A2 (CORE-OPS-1): operator-plane paged reads. The tool
		// catalog (whole registry, not a per-agent menu), the system-skill registry,
		// and ScopeSystem memory recall (operator sees all data, D13).
		operatorSvc.SetReadSources(k.Server.ToolExecutor, k.Server.SkillRegistry, k.Memory.QueryService)
		// ADR-0047 A2.2: operator-triggered tool execution at ScopeSystem (audited,
		// idempotent). Reuses the kernel tool reference monitor with the System bypass.
		operatorSvc.SetToolExec(k.Server.ToolExecutor)
		// ADR-0047 A2.4: operator memory ingest requests a KERNEL document ingest —
		// the same chunking-pipeline / write-back path the agent plane and benchmarks
		// use — never a raw store write. The operator principal is stamped as Author;
		// the kernel derives classification (tags are a narrow-only hint).
		operatorSvc.SetMemoryIngestor(operator.MemoryIngestorFunc(func(ctx context.Context, req operator.IngestRequest) (string, error) {
			if k.Server.IngestionProcessor != nil {
				sourceURI := req.Source
				if sourceURI == "" {
					sourceURI = "operator_ingest://" + req.SessionID
				}
				title := []rune(req.Text)
				if len(title) > 80 {
					title = title[:80]
				}
				return k.Server.IngestionProcessor.ProcessSync(ctx, domain.ExternalDocument{
					SourceURI:  sourceURI,
					SourceType: "operator_ingest",
					Title:      string(title),
					Body:       req.Text,
					Author:     req.Author,
					Timestamp:  time.Now().UTC(),
					ThreadID:   req.SessionID,
					Tags:       req.Tags,
					Importance: req.Importance,
				})
			}
			if k.Server.MemoryWriter != nil {
				return k.Server.MemoryWriter.Remember(ctx, req.Author, req.Text, req.Tags, req.Source, req.SessionID, req.Importance)
			}
			return "", fmt.Errorf("memory ingest not configured")
		}))
		// ADR-0047 A2.6: watch CRUD is premium capability-gated. k.Server.WatchHandler
		// is nil in an OSS build (⇒ Unimplemented, WatchTriggered never publishes) and
		// the premium binary injects a real handler via Options.NewSignalReceiver.
		operatorSvc.SetWatchHandler(k.Server.WatchHandler)
		// ADR-0047 D14: capability + version handshake. The UI hides surfaces this
		// build does not advertise and warns on contract-version skew.
		// ADR-0047 Amendment A2: contract bumped 0047→0048 for the CORE-OPS-1 read/
		// exec/approval surface. watches-* are advertised ONLY when the premium watch
		// handler is wired (D14) — an OSS kernel hides the Watches screen.
		operatorCaps := []string{
			"feed", "snapshot", "commands", "steering", "audit",
			"tools-read", "tools-manage", "skills-read",
			"memory-read", "memory-ingest", "tool-exec", "tool-approvals",
		}
		if k.Server.WatchHandler != nil {
			operatorCaps = append(operatorCaps, "watches-read", "watches-crud")
		}
		operatorSvc.SetHandshake("0.6.9-alpha", "0048", operatorCaps)
		// ADR-0047 0047-10: chat & steer. CreateSession is wired to the
		// SessionManager; SendMessage/Inject dispatch through the Execute path is
		// the pending executor-producer side (nil hooks ⇒ Unimplemented).
		operatorSvc.SetSessionOps(operator.SessionOpsFuncs{
			CreateFn: func(ctx context.Context, goal, parentID string) (string, error) {
				ses, err := k.SessionMgr.CreateSession(ctx, goal, parentID)
				if err != nil {
					return "", err
				}
				return ses.ID, nil
			},
			// ADR-0047 0047-21: dispatch the message through the kernel Execute path
			// (session threaded via x-session-id, the loadOrCreateSession key) and
			// return immediately — the operator watches progress on the feed.
			SendFn: func(_ context.Context, sessionID, text string) error {
				go func() {
					mdCtx := metadata.NewIncomingContext(
						context.Background(),
						metadata.Pairs("x-session-id", sessionID),
					)
					if _, err := k.Server.Execute(mdCtx, &pb.Handoff{Payload: &pb.Object{Data: []byte(text)}}); err != nil {
						slog.Warn("operator SendMessage execution failed", "session", sessionID, "err", err)
					}
				}()
				return nil
			},
		})
		pb.RegisterOperatorConsoleServer(k.GRPC, operatorSvc)
		slog.Info("🧬 Cambrian Substrate Active", "port", k.Config.Server.Port)
		return k.GRPC.Serve(k.Listener)
	})

	// D. Ingestion HTTP server (ADR-0028) — opt-in via IngestionHTTPPort > 0.
	if port := k.Config.Execution.IngestionHTTPPort; port > 0 {
		mux := http.NewServeMux()
		mux.Handle("/v1/ingest", memory.NewWebhookReceiver(
			k.Config.Execution.IngestToken,
			k.Memory.IngestionManager.Enqueue,
		))
		// ADR-0030: explicit consolidation trigger endpoint.
		mux.HandleFunc("/v1/admin/consolidate", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			_ = k.EventBus.Publish(domain.MemoryPressureEvent{
				Trigger: string(domain.ConsolidationTriggerExplicit),
			})
			w.WriteHeader(http.StatusAccepted)
		})
		// ADR-0034 (D9): set an agent's intrinsic genotype scope profile.
		// POST /v1/admin/agents/{id}/scope  body: {"required_tags":[],"any_of_tags":[],"forbidden_tags":[]}
		mux.HandleFunc("/v1/admin/agents/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Path: /v1/admin/agents/{id}/scope  OR  /v1/admin/agents/{id}/write-tags
			rest := strings.TrimPrefix(r.URL.Path, "/v1/admin/agents/")
			agentID, suffix, ok := strings.Cut(rest, "/")
			if !ok || agentID == "" || (suffix != "scope" && suffix != "write-tags" && suffix != "tool-grants") {
				http.Error(w, "expected /v1/admin/agents/{id}/{scope|write-tags|tool-grants}", http.StatusNotFound)
				return
			}
			if suffix == "tool-grants" {
				// ADR-0039: set an agent's tool grants (operator-plane).
				// Body: {"grants":[{"tool":"read_file","policy":{"filesystem":{"allow_roots":["/data"]}}}]}
				if k.ToolGrants == nil {
					http.Error(w, "tool grants store unavailable", http.StatusServiceUnavailable)
					return
				}
				var body struct {
					Grants []domain.ToolGrant `json:"grants"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "invalid tool-grants body: "+err.Error(), http.StatusBadRequest)
					return
				}
				for _, gr := range body.Grants {
					// A1.5: an operator grant must not use the AllowAll bypass sentinel
					// (that is reserved for the global tools_unrestricted mode).
					if gr.Policy.AllowAll {
						http.Error(w, "grant policy.allow_all is not permitted on a per-agent grant", http.StatusBadRequest)
						return
					}
				}
				k.ToolGrants.Set(agentID, body.Grants)
				slog.Info("ADR-0039: agent tool grants updated", "agent_id", agentID, "count", len(body.Grants))
				w.WriteHeader(http.StatusOK)
				return
			}
			if k.ScopeResolver == nil {
				http.Error(w, "scope resolver unavailable", http.StatusServiceUnavailable)
				return
			}
			if suffix == "write-tags" {
				// ADR-0035 C2: set the agent's DefaultWriteTags (write classification).
				// Body: {"default_write_tags":["company_wide","analytics"]}
				var body struct {
					DefaultWriteTags []string `json:"default_write_tags"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "invalid write-tags body: "+err.Error(), http.StatusBadRequest)
					return
				}
				if err := k.ScopeResolver.SaveWriteTags(r.Context(), agentID, body.DefaultWriteTags); err != nil {
					http.Error(w, "write-tags rejected: "+err.Error(), http.StatusBadRequest)
					return
				}
				slog.Info("ADR-0035: agent DefaultWriteTags updated", "agent_id", agentID, "tags", body.DefaultWriteTags)
				w.WriteHeader(http.StatusOK)
				return
			}
			var cfg domain.ScopeConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				http.Error(w, "invalid scope body: "+err.Error(), http.StatusBadRequest)
				return
			}
			// SaveScope runs ScopeConfig.Validate() and rejects unsatisfiable /
			// conflicting profiles before persisting (R5).
			if err := k.ScopeResolver.SaveScope(r.Context(), agentID, cfg); err != nil {
				http.Error(w, "scope rejected: "+err.Error(), http.StatusBadRequest)
				return
			}
			slog.Info("ADR-0034: agent scope profile updated", "agent_id", agentID)
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
		slog.Info("🌐 Ingestion HTTP server starting", "port", port,
			"inbox_dir", k.Config.Execution.InboxDir,
			"queue_size", k.Config.Execution.IngestionQueueSize,
			"workers", k.Config.Execution.IngestionWorkers)
		g.Go(func() error {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		})
	}
}

// mcpToolSink keeps the tool registry AND the ADR-0044 retrieval index in step as
// MCP servers (re)connect and drop. It tracks the tools currently published per
// server so a drop, or a changed tool list on reconnect, removes exactly the
// stale entries (registry de-registration = menu-gating; vector removal = re-sync).
type mcpToolSink struct {
	reg      *domain.InMemoryToolRegistry
	indexer  *domain.ToolIndexer
	mu       sync.Mutex
	byServer map[string]map[string]bool // serverID -> set of published tool names
}

func newMCPToolSink(reg *domain.InMemoryToolRegistry, indexer *domain.ToolIndexer) *mcpToolSink {
	return &mcpToolSink{reg: reg, indexer: indexer, byServer: map[string]map[string]bool{}}
}

// Seed records the MCP tools already registered at boot, so a later drop knows
// exactly which entries belong to a server.
func (s *mcpToolSink) Seed(tools []domain.SystemTool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tools {
		if sid, _, ok := mcp.ParseToolName(t.Name); ok {
			if s.byServer[sid] == nil {
				s.byServer[sid] = map[string]bool{}
			}
			s.byServer[sid][t.Name] = true
		}
	}
}

// SetServerTools replaces a server's published tools: registers + indexes the new
// set, and removes any previously-published tool no longer present.
func (s *mcpToolSink) SetServerTools(ctx context.Context, serverID string, tools []domain.SystemTool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]bool, len(tools))
	for _, t := range tools {
		next[t.Name] = true
		s.reg.Register(t)
		if err := s.indexer.Index(ctx, t); err != nil {
			slog.Warn("ADR-0044: re-index on resync failed", "tool", t.Name, "err", err)
		}
	}
	for name := range s.byServer[serverID] {
		if !next[name] {
			s.reg.Remove(name)
			_ = s.indexer.Remove(ctx, name)
		}
	}
	s.byServer[serverID] = next
}

// RemoveServerTools drops all of a server's tools from the registry and the index.
func (s *mcpToolSink) RemoveServerTools(ctx context.Context, serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.byServer[serverID] {
		s.reg.Remove(name)
		_ = s.indexer.Remove(ctx, name)
	}
	delete(s.byServer, serverID)
}

var _ mcp.ToolSink = (*mcpToolSink)(nil)

// registerModelAgents persists each ModelConfig entry as a TraitModel AgentDefinition.
// registerModelAgents persists each generator as a TraitModel AgentDefinition so
// it participates in the auction. ADR-0042: sourced from llm_provider.generators,
// agent ID = "llm:<id>" (id-keyed end to end; the server strips "llm:" to the
// generator id when Acquiring an agent-step model).
func registerModelAgents(reg *kernel.AgentRepoDecorator, generators []config.GeneratorConfig) {
	for _, g := range generators {
		agentID := "llm:" + g.ID
		def := domain.AgentDefinition{
			ID:              agentID,
			Name:            g.Model,
			Description:     fmt.Sprintf("LLM inference provider: %s / %s. Capabilities: %v", g.Provider, g.Model, g.Capabilities),
			Runtime:         domain.RuntimeBinary,
			Trait:           domain.TraitModel,
			Provisional:     false,
			ManifestVersion: "1.0.0",
		}
		if err := reg.SetAgent(def); err != nil {
			slog.Warn("registerModelAgents: failed to register model", "id", agentID, "err", err)
		}
		slog.Info("Registered model agent", "id", agentID, "trait", domain.TraitModel)
	}
}

// registryReconciler is the minimal registry surface the startup reconcilers
// need: list the catalogue and evict an orphan. *kernel.AgentRepoDecorator
// satisfies it; a fake satisfies it in tests.
type registryReconciler interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
	domain.AgentPruner
}

// reconcileModelAgents evicts TraitModel agents no longer declared in
// config.Generators. registerModelAgents only UPSERTS the current generators, so
// a model removed from config would otherwise linger in the registry (bbolt) and
// keep winning the auction after a restart — its persisted merit beats a
// cold-start replacement, and it routes to a backend that may be gone (the
// "model_unavailable: all candidates degraded" failure mode). Config is the
// declarative source of truth for the model population, so any Trait==model agent
// whose id is not "llm:<generator-id>" is an orphan and is pruned. Non-model
// agents are untouched — they reconcile against their own sources (filesystem /
// A2A) and must never be pruned here.
func reconcileModelAgents(ctx context.Context, reg registryReconciler, generators []config.GeneratorConfig) {
	declared := make(map[string]bool, len(generators))
	for _, g := range generators {
		declared["llm:"+g.ID] = true
	}
	agents, err := reg.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("reconcileModelAgents: list agents failed; skipping prune", "err", err)
		return
	}
	for _, a := range agents {
		if a.Trait != domain.TraitModel || declared[a.ID] {
			continue
		}
		if err := reg.DeleteAgent(a.ID); err != nil {
			slog.Warn("reconcileModelAgents: prune failed", "id", a.ID, "err", err)
			continue
		}
		slog.Info("reconcileModelAgents: pruned orphaned model no longer in config", "id", a.ID)
	}
}

// reconcileFilesystemAgents evicts agents whose local source file no longer
// exists. The bbolt seeder (storage.Seed) is upsert-only — it seeds new agent
// files and updates changed ones but never removes an agent whose *.py / sidecar
// manifest was deleted, so the stale record keeps competing in the auction (the
// same orphan class as models, with the filesystem as the declarative source).
//
// Provenance scoping is critical: this prunes ONLY filesystem-sourced agents
// (those carrying a local ExecPath). It deliberately spares
//   - TraitModel agents      — reconciled against config by reconcileModelAgents;
//   - RuntimeA2A agents       — registered dynamically at runtime with no local
//     source file, so absence-on-disk says nothing about their liveness;
//   - any record without an ExecPath — nothing to check against.
//
// exists reports whether an agent's source path is still present (injected for
// testability; os.Stat at the call site).
func reconcileFilesystemAgents(ctx context.Context, reg registryReconciler, exists func(path string) bool) {
	agents, err := reg.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("reconcileFilesystemAgents: list agents failed; skipping prune", "err", err)
		return
	}
	for _, a := range agents {
		if a.Trait == domain.TraitModel || a.Runtime == domain.RuntimeA2A || a.ExecPath == "" {
			continue
		}
		fullPath := filepath.Join(a.Dir, a.ExecPath)
		if exists(fullPath) {
			continue
		}
		if err := reg.DeleteAgent(a.ID); err != nil {
			slog.Warn("reconcileFilesystemAgents: prune failed", "id", a.ID, "err", err)
			continue
		}
		slog.Info("reconcileFilesystemAgents: pruned agent whose source file is gone",
			"id", a.ID, "exec_path", a.ExecPath)
	}
}

// docTypeLister enumerates persisted index documents of one DocType. The
// concrete *postgres.PgVectorAdapter satisfies it; a fake satisfies it in tests.
// Deliberately narrow (boot-only) so the VectorStore port and its fakes are
// untouched.
type docTypeLister interface {
	ListIDsByType(ctx context.Context, docType string) ([]string, error)
}

// docRemover drops one index document by id. *domain.ToolIndexer and
// *domain.SkillIndexer satisfy it (Remove(ctx, name)).
type docRemover interface {
	Remove(ctx context.Context, id string) error
}

// toolKeepFunc decides whether a persisted tool doc id is still legitimate during
// the boot index reconcile: a native/connected tool (present in currentTools) or
// an MCP tool whose server is still configured (kept across a transient outage so
// a momentarily-unreachable server's tools are not churned out of the index). A
// tool whose MCP server was removed from config — or a stale native tool no longer
// on disk — is not kept and gets pruned.
func toolKeepFunc(currentTools, configuredMCP map[string]bool) func(id string) bool {
	return func(id string) bool {
		if currentTools[id] {
			return true
		}
		if sid, _, ok := mcp.ParseToolName(id); ok {
			return configuredMCP[sid]
		}
		return false
	}
}

// reconcileIndex evicts persisted index documents (DocType{Tool,Skill}) that the
// freshly-built registry no longer backs. keep(id) reports whether a persisted id
// is still legitimate.
//
// Why a boot reconcile is needed even though the MCP sink already prunes at
// runtime: the sink only evicts tools for a server it actually CONNECTS. A server
// removed from config entirely is never connected this boot, so SetServerTools /
// RemoveServerTools never run for it and its tool docs from a previous run linger
// and stay rankable by find_tools. keep also preserves docs whose source is
// configured but merely unreachable this boot, so a transient outage does not
// churn the index (its tools re-sync when the Watch loop reconnects).
func reconcileIndex(ctx context.Context, lister docTypeLister, remover docRemover, docType string, keep func(id string) bool) {
	ids, err := lister.ListIDsByType(ctx, docType)
	if err != nil {
		slog.Warn("reconcileIndex: list failed; skipping prune", "doc_type", docType, "err", err)
		return
	}
	for _, id := range ids {
		if keep(id) {
			continue
		}
		if err := remover.Remove(ctx, id); err != nil {
			slog.Warn("reconcileIndex: prune failed", "doc_type", docType, "id", id, "err", err)
			continue
		}
		slog.Info("reconcileIndex: pruned orphaned index doc no longer in registry",
			"doc_type", docType, "id", id)
	}
}

// episodicConsolidator adapts the EpisodicExtractor to the circadian.SessionConsolidator interface.
// It applies the consolidation delay (giving Tier-2 time to drain) and loads narrative events
// before running episodic extraction. ADR-0029 + ADR-0030.
type episodicConsolidator struct {
	extractor          *awareness.EpisodicExtractor
	eventLogger        *subsynaptic.EventLogger
	consolidationDelay time.Duration
}

func (c *episodicConsolidator) Consolidate(ctx context.Context, sess domain.Session) error {
	if c.consolidationDelay > 0 {
		select {
		case <-time.After(c.consolidationDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	events, _ := c.eventLogger.GetRecentEvents(ctx, sess.ID, 200)
	return c.extractor.ExtractAndSave(ctx, awareness.EpisodicExtractionInput{
		Session: sess,
		Events:  events,
	})
}

func initTelemetry(cfg *config.Config) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	if cfg.Telemetry.OTLPEndpoint == "" && cfg.Telemetry.PrometheusPort == 0 && !cfg.Telemetry.EnableStdoutExporter {
		return nil, nil // ADR-0057 (D11): telemetry off by default — stay silent.
	}
	// ADR-0057 (D11): announce telemetry activation for transparency (only when enabled).
	slog.Info("telemetry enabled — the runtime will export traces/metrics as configured",
		"otlp_endpoint", cfg.Telemetry.OTLPEndpoint,
		"prometheus_port", cfg.Telemetry.PrometheusPort,
		"stdout_exporter", cfg.Telemetry.EnableStdoutExporter)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.Telemetry.TraceSamplingRate)),
	)
	otel.SetTracerProvider(tp)

	var mpOpts []sdkmetric.Option
	if cfg.Telemetry.EnableStdoutExporter {
		stdoutExp, err := stdoutmetric.New(stdoutmetric.WithPrettyPrint())
		if err != nil {
			slog.Warn("telemetry: failed to create stdout metric exporter", "err", err)
		} else {
			mpOpts = append(mpOpts, sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(stdoutExp, sdkmetric.WithInterval(10*time.Second)),
			))
		}
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)
	return tp, mp
}

// operatorBootstrapIdentity builds the V1 operator-plane identity (ADR-0047 D13).
// It seeds a single bootstrap operator from CAMBRIAN_OPERATOR_USER /
// CAMBRIAN_OPERATOR_PASSWORD (role from CAMBRIAN_OPERATOR_ROLE, default
// "operator"). Secure-by-default: with no env creds the table is empty and no
// login can succeed. The production identity backend replaces this behind the
// OperatorIdentity port with no interceptor change.
func operatorBootstrapIdentity() *operator.StaticIdentity {
	users := map[string]struct {
		Password string
		Role     operator.Role
	}{}
	if u, p := os.Getenv("CAMBRIAN_OPERATOR_USER"), os.Getenv("CAMBRIAN_OPERATOR_PASSWORD"); u != "" && p != "" {
		role := operator.Role(os.Getenv("CAMBRIAN_OPERATOR_ROLE"))
		if role != operator.RoleViewer {
			role = operator.RoleOperator
		}
		users[u] = struct {
			Password string
			Role     operator.Role
		}{Password: p, Role: role}
	}
	return operator.NewStaticIdentity(users)
}

// stringSliceFromMeta coerces a document metadata value into a []string (tags
// may round-trip through JSON as []interface{}). ADR-0047 0047-25.
func stringSliceFromMeta(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// applyTag adds or removes a tag from a set, de-duplicating. ADR-0047 A1.2.
func applyTag(tags []string, tag string, add bool) []string {
	out := make([]string, 0, len(tags)+1)
	found := false
	for _, t := range tags {
		if t == tag {
			found = true
			if !add {
				continue // remove
			}
		}
		out = append(out, t)
	}
	if add && !found {
		out = append(out, tag)
	}
	return out
}
