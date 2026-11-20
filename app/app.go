package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
	skillaccess "github.com/cambrian-sh/core/internal/skill/access"
	skilllifecycle "github.com/cambrian-sh/core/internal/skill/lifecycle"
	"github.com/cambrian-sh/core/internal/storage"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
	"github.com/cambrian-sh/core/pkg/util"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

// maxGRPCMessageBytes caps a single unary gRPC message on the main listener. It
// exists for the operator binary-ingest lane (IngestMemory), which carries an
// entire file in one request; gRPC's 4 MiB default rejects real documents. Set to
// 512 MiB to support large uploads (~500 MB).
//
// NOTE: a message this size is buffered whole in memory on both peers — this is a
// ceiling for occasional large ingests, not a licence for routine half-gig
// payloads. The scalable path for very large files is a client-streaming (chunked)
// ingest RPC; this constant unblocks the single-message lane in the meantime.
const maxGRPCMessageBytes = 512 * 1024 * 1024

// Kernel acts as the centralized container for the Orchestrator's life support systems.
// After the stack refactor (2026-05-11c), it holds only:
//   - infrastructure primitives (Config, Registry, Store, Listener)
//   - domain stacks (Memory, Awareness, Metabolism, Supervision)
//   - runtime handles (Server, GRPC)
type Kernel struct {
	Config *config.Config
	// ConfigProvenance records which layer supplied each config key (ADR-0101 D4).
	// Backs GetConfigSchema.value_source, and the write path's "stored, but an env
	// var pins this" warning. Never nil after a successful boot.
	ConfigProvenance config.Provenance
	// ConfigStore is the durable config + secret store (ADR-0101). nil when the
	// store layer is disabled (CAMBRIAN_CONFIG_STORE=off), in which case the
	// operator plane's config writes answer Unimplemented rather than pretending
	// to persist.
	ConfigStore *storage.BoltConfigStore
	// ConfigPath is the primary config file (<baseDir>/configs/config.json). The
	// config write path locates the file layers from it (ADR-0101 D8: a console
	// write to a key a writable file sets goes into that file).
	ConfigPath string
	// Links is the identity plane's assertion store (five-planes step 2). Held on
	// the kernel so the operator plane can be given a REVIEW lane: the stores are
	// otherwise local to the build, and a queue of candidates nobody can confirm
	// is the state contract 0098 exists to end. nil without a database.
	Links domain.LinkStore
	// linkReviewLane is the operator plane's review lane (contracts 0098/0109),
	// composed by Substrate.LinkReview: the inbox reads go through the query
	// plane, so every candidate passes the plane's per-link evidence filter
	// (owner fence included) before the operator sees it
	// (T-20261107-link-review-inbox-and-entity-op-candidates-skip). nil without
	// a database.
	linkReviewLane operator.LinkWriter
	Registry       domain.AgentRegistry // domain-facing interface layer
	Store          io.Closer            // opaque storage handle — only Close() is exposed
	Memory         *kernel.MemoryStack
	Awareness      *kernel.AwarenessStack
	Metabolism     *kernel.MetabolismStack
	Supervision    *kernel.SupervisionStack
	Server         *subnetwork.Server
	Listener       net.Listener
	GRPC           *grpc.Server
	// agentSockets serves the gRPC plane on the kernel-only agents' unix
	// sockets (ADR-0157 S2); started with GRPC, stopped after it.
	agentSockets *agentSocketServer
	// serving is closed right before GRPC.Serve (ENG-2135), releasing the
	// plugins' boot-time daemon spawns (KernelServices.Serving).
	serving *servingSignal
	// runTrace is the ADR-0145 D10 run-trace file (execution.trace; nil when off).
	// Shutdown closes it after everything that writes spans has stopped, so an
	// in-process kernel releases the file (ENG-2102).
	runTrace io.Closer
	// Health is the grpc.health.v1 checker (PLAT-03 / ADR-0065): SERVING once the DB is
	// reachable, NOT_SERVING while draining. Registered on the main listener.
	Health *health.Checker

	// ADR-0047 0047-16: operator command effects bound to kernel surfaces.
	OperatorEffects operator.CommandEffects
	// ADR-0142 D12: the ShareToTeam publish effect (also the hub's "publish" handler).
	MemorySharer operator.MemorySharer
	// ADR-0047 0047-24: durable operator audit store (Postgres, in-memory fallback).
	OperatorAudit domain.AuditStore
	// Documents enumerates ingested documents by row, backing the ListDocuments RPC
	// and the "document-listing" capability. Carried on the Kernel because the store
	// handle is local to bootstrapKernel while the operator service is wired later,
	// in startKernelServices. nil ⇒ the RPC answers Unimplemented.
	Documents memory.DocumentLister
	// DocReader is the keyed, scope-enforced document read (contract 0086). Shared
	// with the plugin seam so the console and a watch resolve a reference through
	// ONE code path with one scope model — two implementations would be two answers
	// to "may this principal read this", differing by caller.
	DocReader domain.DocumentGetter
	// REACT-01 / ADR-0061: reactive dead-letter read source (the bbolt journal
	// decorator). Backs the OperatorConsole ListWatchDeadLetters RPC.
	WatchDeadLetters domain.WatchDeadLetterReader
	// REACT-05 / ADR-0071: watch observability ports (the premium ReactiveEngine, when
	// wired). Back GetWatchMetrics / BacktestWatch. nil in OSS.
	WatchMetrics    domain.WatchMetricsReader
	WatchBacktester domain.WatchBacktester
	// ExtraServices (ADR-0073) mounts downstream (premium) gRPC services on the kernel
	// server before Serve. nil in OSS. Carried from Options through bootstrapKernel.
	ExtraServices func(*grpc.Server)
	// AgentGRPCServices (ADR-0118 D3) mounts premium services on the AGENT plane,
	// keyed by fully-qualified service name. The keys route auth: exempt from the
	// operator bearer, SurfaceAgent, x-agent-id principal seeding. nil in OSS.
	AgentGRPCServices map[string]func(*grpc.Server)
	// OperatorMethodRoles are the premium plugins' declared operator-method role
	// classes, handed to the operator auth interceptors. nil in OSS.
	OperatorMethodRoles map[string]OperatorMethodRole
	// LoginObserver receives every operator Login attempt (ADR-0130 1f) — the
	// premium receipts lane chains failed ones. Observation only; nil in OSS,
	// where the operator service's structured log line is the whole record.
	LoginObserver domain.LoginObserver
	// Lifecycles are background components started at boot and drained in reverse on
	// shutdown (ADR-0074) — plugin-contributed (the reactive engine's worker pools +
	// REACT-06 scheduler) and kernel-contributed (the ADR-0084 chat worker pool). In OSS
	// this holds the chat pool when the chat lane is enabled, and is otherwise empty.
	Lifecycles []Lifecycle
	// PluginCapabilities are operator capability strings contributed by plugins (ADR-0082
	// D2). The kernel appends them to its own base set at handshake time WITHOUT
	// interpreting any of them — this is what keeps premium vocabulary (watch-*, etc.)
	// out of the OSS core. Empty in OSS.
	PluginCapabilities []string
	// PluginStatuses records each declared plugin and whether it registered. Backs the
	// Snapshot handshake's `repeated PluginInfoOp plugins` field (operator.proto,
	// ADR-0082 D9 as shipped by ADR-0089 — there is no separate ListPlugins RPC).
	// Empty in OSS.
	PluginStatuses []PluginStatus

	// Conversations is the durable Conversation/Message store (ADR-0084 D1). nil when the
	// schema has not been migrated — chat surfaces are then unavailable, but the rest of the
	// kernel runs normally.
	Conversations domain.ConversationStore
	// ChatTurns executes conversational turns on the ADR-0084 D4 worker pool. nil when the
	// chat lane is disabled (execution.chat_pool_size = 0) or unmigrated.
	ChatTurns *corechat.TurnService
	// PrincipalDirectory answers act-for's "is this a known principal?" (premium
	// authz; nil ⇒ act-for refused). PrincipalSchedules is the premium store of
	// principal-owned recurring tasks (nil ⇒ "scheduled-tasks" not advertised).
	PrincipalDirectory domain.PrincipalDirectory
	PrincipalSchedules domain.PrincipalSchedules
	// StreamRegistry is the contract-0074 stream liveness source a plugin
	// registers (RegisterStreamRegistry); a late-bound holder, never nil — until
	// a plugin registers it answers "unknown" / -1, as an unwired kernel did.
	StreamRegistry domain.StreamRegistry
	// DeadLetterRetrier replays a failed-delivery dead letter (ENG-1654); a
	// late-bound holder a plugin fills through RegisterDeadLetterRetrier.
	DeadLetterRetrier *deadLetterRetrierHolder
	// Principals is the principal registry (ADR-0142 D3) and Authenticator the
	// one bearer resolver of the operator plane and the HTTP admin routes (D1).
	Principals    domain.PrincipalRegistry
	Authenticator domain.Authenticator

	// ADR-0012: Synaptic Bridge components.
	SessionMgr      *session.SessionManager
	CircadianRhythm *circadian.CircadianRhythm
	ArtifactVault   *vault.ArtifactVault
	EventBus        *domain.InMemoryEventBus

	// PythonEnv is the kernel-managed sandbox Python env's boot check and
	// background build (ADR-0157 D7, app/python_env.go); Shutdown stops it.
	PythonEnv pythonEnvGate

	// ADR-0139 D13: the skill lifecycle (nil when its store is not wired) and the
	// deterministic curator worker (nil unless execution.skills.curator.enabled).
	SkillLifecycle *skilllifecycle.Service
	SkillCurator   *skilllifecycle.Curator
	// LedgerSweeper is the ADR-0145 D1 run ledger's retention sweeper; nil when
	// execution.ledger.enabled is off. Started with the kernel, drained by Shutdown.
	LedgerSweeper *ledger.Sweeper
	// SkillAccess is per-agent skill access (decisions/skill-access-grants.md):
	// profile skills + grants. nil only if it could not be built.
	SkillAccess *skillaccess.Service

	// ADR-0085: the access-control decision point. In OSS this is the allow-all
	// default; a premium policy plugin replaces it. The kernel holds it so every
	// enforcement point asks the SAME decision point.
	Authorizer domain.Authorizer
	// PolicyAdmin is the policy authoring surface. nil in OSS ⇒ the scope admin
	// endpoints answer 501 rather than silently accepting a boundary.
	PolicyAdmin domain.PolicyAdmin

	// ADR-0039: tool grants (operator sets grants via OperatorConsole). Operator
	// grants are durable in Postgres (0052 tool_grants, ENG-1116), merged over the
	// in-memory profile-allowlist layer.
	ToolGrants *domain.LayeredGrantsStore

	// ADR-0043: live MCP server connections. Constructed UNCONDITIONALLY since
	// contract 0097: a kernel booted with zero MCP servers must still be able to
	// hot-attach its first one from the console, and the connector is a map and
	// a client — the cost of always having it is nothing.
	MCPConnector *mcp.Connector
	// EgressProxy is the egress allowlist proxy every contained MCP server
	// with enforced declared hosts reaches the network through (ENG-688).
	// Started at boot, stopped after the connector closed its servers.
	EgressProxy *egressproxy.Broker
	// ADR-0043 D8 / ADR-0044: health/reconnect inputs for the background Watch loop.
	MCPSink    mcp.ToolSink
	MCPServers []mcp.ServerConfig
	// MCPRuntime is the LIVE server list — boot config plus every runtime save
	// (contract 0097). The operator list read and the write path share it, so a
	// console re-read after a save shows the server it just added.
	MCPRuntime *mcpRuntime
	// credentialSeams late-binds the operator plane (audit, plugin mutations)
	// the outbound-credential provider (ADR-0141; built in bootstrapKernel
	// before the MCP connector) uses, built after it; it also answers the
	// provider's org-credential writer (orgWriter).
	credentialSeams *credentialSeams
	// MCPEndpoint is the INBOUND MCP endpoint's HTTP server (ADR-0126 D3), or nil
	// when server.mcp.enabled is false. Carried here only so Shutdown can drain
	// it — it is started during bootstrap, before the errgroup the other HTTP
	// servers live in exists, because it must be serving before ConnectAll.
	MCPEndpoint *http.Server
	// mcpTasks is the endpoint's task lane (ADR-0126 D10); Shutdown cancels and
	// drains its in-flight runs after the endpoint stops accepting (AGENTS.md §3.3).
	mcpTasks *mcpTaskLane
	// MCPCoreBackends is the endpoint's core-tool binding seam (ADR-0126 D5).
	// Bound during bootstrap; carried here so a later phase (E6's
	// IdentityResolver hop, a scoped answer lane) can rebind without a restart.
	MCPCoreBackends *mcpserve.CoreBackends
	// GeneratorRuntime is the LIVE generator list + default — the generator
	// analogue of MCPRuntime (owner directive 2026-08-12: LLMs register
	// dynamically; SaveGenerator applies live, and the console read half,
	// TestGenerator and the removal guards all consult this).
	GeneratorRuntime *generatorRuntime
	// LLMGateway is the agent-plane streaming gateway, carried here so a live
	// generator save can register the new model's streaming client (the second
	// of the two parallel model tables; the broker registry is the first).
	LLMGateway *subnetwork.SubstrateLLMGateway

	// ── Contract 0072 (Wave 1) sources ───────────────────────────────────────
	// Carried on the Kernel because they are constructed in bootstrapKernel but
	// consumed later, where the operator service is wired.

	// CheckpointSource is the store backing ListSessionCheckpoints. Typed as any
	// because the concrete store differs by backend (Postgres or bbolt) and the
	// operator wiring type-asserts for the two methods it needs. nil ⇒ the
	// Checkpoints tab reports Unimplemented rather than an empty list.
	CheckpointSource any
	// LLMProvider backs the generator registry reads (breaker state, roles).
	LLMProvider *llm.Provider
	// VectorCounter returns the stored embedding count, or -1 when it cannot be
	// counted. nil ⇒ -1, which is NOT zero (see EmbeddingConfigOp.vector_count).
	VectorCounter func(ctx context.Context) int64
	// TokenSeries is the hourly token accumulator behind the spend sparkline
	// (contract 0075). Tokens only — never money.
	TokenSeries domain.TokenSeriesReader
	// Progress is the ADR-0098 progress holder. Carried here because it is built
	// before the operator feed exists, and contract 0079 needs to give it the
	// feed as a second destination once that feed is up.
	Progress *progressHolder
	// StepOutputFeed is the live step-output stream's operator-feed sink (ADR-0139
	// amendment 2026-11-06), built with the agent-plane server and attached to the
	// feed once it exists — the same late binding as Progress.
	StepOutputFeed *stepOutputFeed
	// Logs is the in-process log retention window.
	//
	// Carried on the kernel because it is created with the logger — before almost
	// everything else — and the surface that will read it is created near the end.
	// Without this the ring exists and nothing can reach it, which is a window
	// nobody can look through.
	Logs *util.LogRing
}

// mcpToolCounter returns a function counting the tools attributed to one MCP
// server, or nil when no tool catalog is available. Tool ids are namespaced by
// server, which is what makes the count derivable without a second registry.
func (k *Kernel) mcpToolCounter() func(serverID string) int {
	if k.Server == nil || k.Server.ToolExecutor == nil {
		return nil
	}
	return func(serverID string) int { return countMCPTools(k.Server.ToolExecutor.AllTools(), serverID) }
}

// countMCPTools counts the registered tools that belong to one MCP server. The
// identity is parsed with mcp.ParseToolName (mcp:<server-id>/<tool>, ADR-0043
// D3) rather than matched by prefix: the earlier "<id>." / "<id>:" prefix test
// never matched that shape, so ListMCPServers reported 0 tools for every server
// (T-20261106-listmcpservers-tool-count-is-always-0), and a prefix test on
// "mcp:<id>" would also count a server whose id merely starts with this one's.
func countMCPTools(tools []domain.SystemTool, serverID string) int {
	n := 0
	for _, t := range tools {
		if server, _, ok := mcp.ParseToolName(t.Name); ok && server == serverID {
			n++
		}
	}
	return n
}

// kernelShutdownBudget is the whole graceful shutdown's bound: 10 s for the
// sequence itself plus the agents' out/ collection wait
// (execution.agents.shutdown_timeout_ms, ENG-2095), so that wait never eats
// the time the steps after it need — the store must still close.
func kernelShutdownBudget(k *Kernel) time.Duration {
	budget := 10 * time.Second
	if k != nil && k.Metabolism != nil && k.Metabolism.Manager != nil {
		budget += k.Metabolism.Manager.ShutdownTimeout()
	}
	return budget
}

// Shutdown initiates the graceful teardown of all kernel resources.
//
// Order matters for the run ledger (ENG-2095): the metabolism stack's shutdown
// returns only after every agent instance's out/ is collected into the ledger
// (or the bound ran out), and the store — the ledger's pool — and the artifact
// vault close after it.
func (k *Kernel) Shutdown(ctx context.Context) {
	slog.Info("🧬 Kernel: Initiating graceful shutdown sequence...")

	// 0. PLAT-03 / ADR-0065: flip health to NOT_SERVING first, so probes and load
	// balancers stop routing to this kernel before it drops in-flight requests.
	if k.Health != nil {
		k.Health.Shutdown()
	}

	// 1. Stop accepting new gRPC requests
	if k.GRPC != nil {
		slog.Info("🔌 Stopping gRPC Server (GracefulStop)...")
		k.GRPC.GracefulStop()
	}

	// 2. Close network listener
	if k.Listener != nil {
		_ = k.Listener.Close()
	}
	// 2b. ADR-0157 S2: the kernel-only agents' unix sockets — the served ones
	// ended with GracefulStop above; close the never-served ones and wait.
	if k.agentSockets != nil {
		k.agentSockets.stop()
	}

	// 2a. ADR-0126: drain the inbound MCP endpoint. Before the plugin lifecycles
	// and the stacks below, because its tool handlers call into them — stopping
	// the stacks first would fail whatever an external agent had in flight
	// instead of letting it finish.
	if k.MCPEndpoint != nil {
		slog.Info("🔌 Stopping MCP endpoint (ADR-0126)...")
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = k.MCPEndpoint.Shutdown(shutCtx)
		cancel()
	}
	// The task lane's runs outlive their submit requests (WithoutCancel), so the
	// endpoint draining does not end them: cancel and wait for them here, before
	// the stacks they call into stop.
	if k.mcpTasks != nil {
		laneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		k.mcpTasks.Stop(laneCtx)
		cancel()
	}

	// 2b. ADR-0074: drain plugin lifecycles (e.g. the reactive engine's schedule timers +
	// worker pools) in reverse order, before their dependencies go away. No-op in OSS.
	for i := len(k.Lifecycles) - 1; i >= 0; i-- {
		lc := k.Lifecycles[i]
		if lc.Stop != nil {
			slog.Info("🛑 Stopping plugin lifecycle...", "name", lc.Name)
			lc.Stop()
		}
	}

	// 3. Stop domain stacks (reverse order of dependency)
	if k.Supervision != nil {
		k.Supervision.Shutdown(ctx)
	}
	if k.PythonEnv != nil {
		k.PythonEnv.Stop()
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
	if k.EgressProxy != nil {
		k.EgressProxy.Stop() // ENG-688: after the servers it serves are gone
	}
	if k.CircadianRhythm != nil {
		k.CircadianRhythm.Stop()
	}
	if k.SkillCurator != nil {
		k.SkillCurator.Stop() // ADR-0139 D13
	}
	if k.LedgerSweeper != nil {
		k.LedgerSweeper.Stop() // ADR-0145 D1
	}
	if k.ArtifactVault != nil {
		_ = k.ArtifactVault.Close()
	}

	// 3c. ENG-2102: the run-trace file, after every span writer above has stopped.
	// Its spans are flushed as written, so this only releases the handle; a span
	// arriving after Close is dropped by the writer.
	if k.runTrace != nil {
		if err := k.runTrace.Close(); err != nil {
			slog.Warn("run trace: close failed", "err", err)
		}
	}

	// 4. Drain storage
	if k.Store != nil {
		k.Store.Close()
	}

	slog.Info("✅ Kernel: Shutdown complete. System at rest.")
}

// Run is the composition root. It loads configuration from the 12-layer
// pipeline (defaults, the configs/ file layers, the ADR-0101 store and CAMBRIAN_*
// env; see config.LoadConfig and ADR-0024),
// wires every subsystem from opts + cfg, and starts the gRPC server.
func Run(ctx context.Context, opts Options) error {
	flag.Parse()

	// Resolve the config bundle against the binary, not just the cwd, so a kernel
	// spawned from another directory (benchmark supervisor, systemd, IDE) still
	// loads configs/ + .env — otherwise every layered override, including
	// nested execution overrides, silently falls back to defaults (see ResolveBaseDir).
	baseDir := config.ResolveBaseDir()

	// Load .env into the process environment before anything reads it, so API
	// keys (os.Getenv via api_key_env) and CAMBRIAN_* overrides resolve from a
	// local gitignored file. Missing file is a no-op; real env vars take priority.
	if err := config.LoadDotEnv(filepath.Join(baseDir, ".env")); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}

	// ADR-0101: the embedded config + secret store, opened BEFORE config loads
	// because it is one of config's layers. Its path derives from baseDir rather
	// than from cfg.Storage.DataDir, which would be circular — and deliberately
	// so: keeping it in the config bundle rather than the data directory means a
	// corpus reset (which truncates the data dir) cannot take an operator's
	// durable settings and every stored credential with it.
	cfgStore, err := OpenConfigStore(baseDir)
	if err != nil {
		return err
	}
	if cfgStore != nil {
		defer func() { _ = cfgStore.Close() }()
	}

	// ADR-0101 D9: a store written before generators were stored per id holds
	// one whole `llm_provider.generators` list. Convert it to per-id entries
	// before the load, persisted, so the write path never sees the old layout.
	if cfgStore != nil {
		if ids, mErr := config.MigrateStoredGenerators(cfgStore); mErr != nil {
			slog.Warn("config store: could not migrate the stored generator list to per-id entries; load merges it in memory", "err", mErr)
		} else if len(ids) > 0 {
			slog.Info("config store: migrated the stored generator list to per-id entries (ADR-0101 D9); generators declared only in files are visible again",
				"ids", ids)
		}
	}

	// Integrations: the contract-0097 whole MCP server list becomes per-id
	// entries (internal/config/store_mcp.go), for the same reason as D9.
	if cfgStore != nil {
		if ids, mErr := config.MigrateStoredMCPServers(cfgStore); mErr != nil {
			slog.Warn("config store: could not migrate the stored MCP server list to per-id entries; load merges it in memory", "err", mErr)
		} else if len(ids) > 0 {
			slog.Info("config store: migrated the stored MCP server list to per-id entries; servers declared only in mcp.json are visible again",
				"ids", ids)
		}
	}

	configPath := filepath.Join(baseDir, "configs", "config.json")
	cfg, prov, err := config.LoadConfigWithStore(configPath, configStoreOrNil(cfgStore))
	if err != nil {
		return err // ConfigError is already structured; wrapping breaks errors.As in main()
	}
	warnStoreOverrides(configPath, cfgStore)

	// Anchor the remaining relative boot paths to the same base as the config
	// bundle. Otherwise a kernel spawned from another cwd loads its config but
	// then can't find agents_dir (no system agents → their dispatchers degrade to
	// env-only) or writes data_dir under the wrong directory. When baseDir is "."
	// (the normal in-tree launch) these are byte-for-byte no-ops.
	if cfg.Metabolism.AgentsDir != "" && !filepath.IsAbs(cfg.Metabolism.AgentsDir) {
		cfg.Metabolism.AgentsDir = filepath.Join(baseDir, cfg.Metabolism.AgentsDir)
	}
	if cfg.Storage.DataDir != "" && !filepath.IsAbs(cfg.Storage.DataDir) {
		cfg.Storage.DataDir = filepath.Join(baseDir, cfg.Storage.DataDir)
	}
	// ADR-0139 D8: the sandbox work root, hidden paths and catalog file.
	anchorSandboxPaths(cfg, configPath)
	// ADR-0157 D1: the kernel-state paths confined agents never see.
	anchorAgentSandboxPaths(cfg, configPath, baseDir)

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

	// SEC-03: decide transport security BEFORE binding, so a configuration that
	// would serve a plaintext operator plane to the network fails at boot rather
	// than after the port is open.
	_, transportMode, err := transportCredentials(cfg)
	if err != nil {
		return err
	}

	// REDEMPTION: Health Check First (Fail Fast)
	addr := listenAddress(cfg)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("network (%s unavailable): %w", addr, err)
	}
	// SEC-03: the loopback+TLS mode terminates TLS at the LISTENER (first-byte
	// demux) so the same port also accepts the agent plane's plaintext h2c.
	lis, err = secureListener(lis, cfg, transportMode)
	if err != nil {
		return err
	}
	switch transportMode {
	case "tls":
		slog.Info("SEC-03: operator plane serving TLS", "addr", addr)
	case modeTLSPlusPlaintextLoopback:
		slog.Info("SEC-03: operator plane on loopback serving TLS AND plaintext (demux)",
			"addr", addr, "why", "TLS for the local forwarder, plaintext for the agent plane")
	case "plaintext-insecure-optin":
		slog.Warn("SEC-03: operator plane serving PLAINTEXT on a routable address "+
			"(server.insecure_localhost=true)", "addr", addr,
			"effect", "bearer tokens cross the network unencrypted")
	default:
		slog.Info("SEC-03: operator plane on loopback, plaintext", "addr", addr)
	}

	logResult, err := util.InitLogger(util.LogModeHeadless, cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	if logResult.File != nil {
		defer logResult.File.Close()
	}

	// ADR-0112 + ADR-0126: attach the plugin-facing named-secret seam BEFORE
	// bootstrap, not after it.
	//
	// The seam used to be attached with the other resolvers below, on the
	// reasoning that its one consumer resolves credentials per delivery long
	// after Run finishes. The MCP endpoint breaks that assumption: it starts
	// inside bootstrapKernel — deliberately, so it is serving before the 40-45 s
	// ConnectAll — and it authenticates every request against a stored
	// credential. Attached afterwards, the endpoint would answer 401 to
	// everything for the whole of boot, which is the timeout it exists to avoid
	// wearing a different error code.
	//
	// The guard is load-bearing: boxing a typed-nil *BoltConfigStore into the
	// holder's interface re-creates the trap config_store_off_test.go exists to
	// prevent.
	if cfgStore != nil {
		setNamedSecretSource(cfgStore)
	}
	// ADR-0130 D3: the one listener policy, attached for the plugin-facing
	// ServeHTTPListener before any plugin builds, so a premium HTTP ingress
	// binds exactly as the kernel's own listeners do.
	setListenerPolicy(cfg)
	defer setListenerPolicy(nil)

	// REDEMPTION: Observability First. Start Kernel immediately.
	k, err := bootstrapKernel(rootCtx, cfg, lis, opts, cfgStore)
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("bootstrap: %w", err)
	}
	// ADR-0101: carried onto the Kernel so the operator plane can read config
	// provenance and write durable settings. bootstrapKernel does not take them
	// because they are resolved before it runs — the store IS a config layer.
	k.ConfigProvenance = prov
	k.ConfigStore = cfgStore
	k.ConfigPath = configPath
	// ADR-0130 D1 / ADR-0157 D4.5: registered agents the kernel does NOT spawn
	// (remote A2A agents) get a stored credential; kernel-spawned agents
	// authenticate with per-instance tokens only, and their stored secrets are
	// retired (deleted) here — the Q4 migration. Guarded against the typed-nil
	// trap (see setNamedSecretSource).
	if cfgStore != nil {
		ensureAgentCredentials(rootCtx, cfgStore, k.Registry)
	}
	// ADR-0101 D5: let the LLM clients READ the credentials this store holds.
	//
	// Without this the store was write-only in the worst sense: SetGeneratorKey
	// encrypted a key, the console reported it installed and showed its last four,
	// and every client still called os.Getenv and nothing else -- so the endpoint
	// answered 401 while the panel said a key was configured. Nothing anywhere
	// ever read what had been saved.
	if cfgStore != nil {
		llm.SetSecretResolver(cfgStore)
		// Contract 0097: MCP connectors resolve their tokens the same way (env
		// first, store second); the connector is constructed with the store in
		// bootstrapKernel (mcpCredentialSources), so the boot connect already
		// presents a token saved from the console.
		// The ADR-0112 named-secret seam is attached ABOVE, before bootstrap —
		// see the comment there.
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), kernelShutdownBudget(k))
		defer cancel()
		k.Shutdown(shutdownCtx)
	}()
	storeCloser = k.Store

	g, gCtx := errgroup.WithContext(rootCtx)

	// REDEMPTION: Parallel Workers (including Backfill)
	startKernelServices(g, gCtx, k)

	return g.Wait()
}
