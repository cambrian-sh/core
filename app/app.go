package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/awareness"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/hooks"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/backfill"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/metabolism/routescorer"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/ratelimit"
	ossreactive "github.com/cambrian-sh/core/internal/reactive"
	"github.com/cambrian-sh/core/internal/sandbox"
	skillaccess "github.com/cambrian-sh/core/internal/skill/access"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
	skilllifecycle "github.com/cambrian-sh/core/internal/skill/lifecycle"
	"github.com/cambrian-sh/core/internal/storage"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	"github.com/cambrian-sh/core/internal/supervision"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
	"github.com/cambrian-sh/core/internal/telemetry"
	tooldiscovery "github.com/cambrian-sh/core/internal/tool/discovery"
	toolproc "github.com/cambrian-sh/core/internal/tool/proc"
	"github.com/cambrian-sh/core/pkg/util"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
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
	// linkReview reads the review inbox through the query plane, so every
	// candidate passes the plane's per-link evidence filter (owner fence
	// included) before the operator sees it
	// (T-20261107-link-review-inbox-and-entity-op-candidates-skip).
	linkReview  *postgres.PgQueryPlane
	Registry    domain.AgentRegistry // domain-facing interface layer
	Store       io.Closer            // opaque storage handle — only Close() is exposed
	Memory      *kernel.MemoryStack
	Awareness   *kernel.AwarenessStack
	Metabolism  *kernel.MetabolismStack
	Supervision *kernel.SupervisionStack
	Server      *subnetwork.Server
	Listener    net.Listener
	GRPC        *grpc.Server
	// agentSockets serves the gRPC plane on the kernel-only agents' unix
	// sockets (ADR-0157 S2); started with GRPC, stopped after it.
	agentSockets *agentSocketServer
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
	// Principals is the principal registry (ADR-0142 D3) and Authenticator the
	// one bearer resolver of the operator plane and the HTTP admin routes (D1).
	Principals    domain.PrincipalRegistry
	Authenticator domain.Authenticator

	// ADR-0012: Synaptic Bridge components.
	SessionMgr      *session.SessionManager
	CircadianRhythm *circadian.CircadianRhythm
	ArtifactVault   *vault.ArtifactVault
	EventBus        *domain.InMemoryEventBus

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

// Shutdown initiates the graceful teardown of all kernel resources.
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
	// ADR-0130 D1: every registered agent has a credential before the first
	// spawn. Guarded against the typed-nil trap (see setNamedSecretSource).
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

// cfgStore is the ADR-0101 config store opened by Run (nil when it could not be
// opened, and in tests): the MCP connector and the outbound-credential provider
// are constructed over it here, before the boot connect.
func bootstrapKernel(ctx context.Context, cfg *config.Config, lis net.Listener, opts Options, cfgStore *storage.BoltConfigStore) (*Kernel, error) {
	// ADR-0074: fold the compile-time plugin set into the effective Options (signal
	// receiver, extra gRPC services, trace wrappers) + the lifecycle set consumed at
	// boot/shutdown. No-op when Options.Plugins is empty (OSS default).
	composed, err := applyPlugins(opts)
	if err != nil {
		return nil, err
	}
	opts = composed.opts
	lifecycles := composed.lifecycles

	os.MkdirAll(cfg.Storage.DataDir, 0755)

	// 0. Process-level runtime settings (config/runtime.go; all off by default):
	// the Go soft memory limit is applied before any stack allocates, and a pprof
	// listener requested on a non-loopback bind fails the boot here, before a
	// port opens. Served later from startKernelServices.
	applyMemoryLimit(cfg.Runtime, osGetenv)
	if why := pprofRefusal(cfg); why != "" {
		return nil, fmt.Errorf("runtime diagnostics: %s", why)
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
		return nil, err
	}

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		storeHandle.Close()
		return nil, err
	}
	// Plugin schema lanes (Registry.AddMigrations) run right after core's, under the
	// same auto-migrate gate and database.migrations policy; with no plugin lanes (OSS)
	// this is a no-op (T-20261113-premium-has-no-schema-migration-chain-41-create).
	if err := vec.BootPluginMigrations(ctx, composed.migrations); err != nil {
		storeHandle.Close()
		return nil, fmt.Errorf("plugin schema migrations: %w", err)
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
		return nil, fmt.Errorf("execution.hooks: %w", err)
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
		return nil, fmt.Errorf("llm provider: %w", err)
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
		withComposedRuntime(composedRuntimeID(cfg.Execution.Workers)) // ADR-0145 D12, "" when off
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

	// ADR-0110: the kind registry, validated as a unit — a malformed spec, a
	// duplicate kind, or a policy nobody registered refuses the boot rather
	// than silently deriving under a different policy than a kind declared.
	//
	// ADR-0136 A1.6: built HERE, above NewMemoryStack, because the kernel's own fact
	// layer lives on the substrate. Core declares its memory_fact kind and the
	// valid_time_supersession authority itself, appended to the plugin-contributed
	// ones (plugins were composed at the top of bootstrapKernel), so the OSS binary
	// carries them with no premium import (ADR-0057). A plugin that also declares
	// memory_fact fails boot here (duplicate kind).
	kindSpecs := append(append([]domain.KindSpec(nil), opts.KnowledgeKinds...), domain.MemoryFactKindSpec())
	authorities := append(append([]domain.ResolutionAuthority(nil), opts.ResolutionAuthorities...), domain.ValidTimeSupersessionAuthority{})
	if _, kerr := domain.NewKindRegistry(kindSpecs, authorities); kerr != nil {
		return nil, fmt.Errorf("knowledge kind registry (ADR-0110): %w", kerr)
	}
	// ADR-0106: the substrate store, ONE instance shared by the kernel fact path (wrapped
	// in the enforcing writer/reader inside NewMemoryStack) and the plugin seam
	// (KernelServices.Knowledge, below).
	// Five-planes step 2: the link verb registry, on the same terms. A duplicate
	// verb or a redeclared seed is a boot refusal rather than two producers
	// disagreeing at write time about whether a verb is symmetric.
	//
	// Two declarers, folded here and validated once (D-W5-1). A PLUGIN declares
	// the verbs it emits by name; the CONFIG declares the verbs a deployment's
	// own sources carry, which no plugin could know. Config goes last so a
	// collision between the two names the operator's entry as the duplicate —
	// the one they can edit — rather than the plugin's.
	relSpecs := append(append([]domain.RelationSpec(nil), opts.RelationSpecs...), cfg.RelationSpecs()...)
	if _, rerr := domain.NewRelationRegistry(relSpecs); rerr != nil {
		return nil, fmt.Errorf("link relation registry (five-planes step 2; deployment verbs are declared "+
			"in the kernel config's \"relations\" section, e.g. configs/config.json): %w", rerr)
	}
	// ADR-0149 D2: the two declarers above are the BOOT declarations (the base,
	// version 1 on a fresh database); the live ontology seeds them into
	// ontology_declarations and replays every namespace's applied changes over
	// them. From here on the stores ask it for a namespace's current registries
	// per write, so an operator's change takes effect without a restart.
	ontologies, oerr := LoadOntology(ctx, vec.Pool(), domain.OntologyDoc{Kinds: kindSpecs, Relations: relSpecs}, authorities, authorizer)
	if oerr != nil {
		return nil, fmt.Errorf("ontology declarations (ADR-0149): %w", oerr)
	}
	onto := ontologies.Live
	// ADR-0147 D15: the substrate's stores and its one query plane, composed
	// through app.NewSubstrate — the same function premium's end-to-end tests
	// build them with, so there is one construction path.
	substrate := NewSubstrate(vec.Pool(), onto, onto)
	knowledgeStore := substrate.Knowledge
	factReader := postgres.NewPgFactItemReader(knowledgeStore, "default", cfg.Embedder.Model, cfg.Embedder.Dimensions)
	// ADR-0138 document lifecycle over the knowledge store's pool. It also owns the
	// document retag (ENG-489): the document row, its chunks' tag cache and its
	// memory_fact items move together in one transaction, so TagMemory and the
	// share-to-team publish below retag through it, never through the raw adapter.
	docLifecycle := postgres.NewPgDocumentLifecycle(knowledgeStore)
	factsSubstrate := kernel.FactSubstrate{
		Store:         knowledgeStore,
		Reader:        factReader,
		Inspector:     factReader, // operator-plane fact RPCs (ENG-459)
		Lister:        knowledgeStore,
		EmbedModelID:  cfg.Embedder.Model,
		AssessorModel: factsAssessorModel(llmProvider.Roles()),
		// ADR-0136 A2.4: the extractor version on every marker names this model.
		GeneratorModel: kernel.FactsGeneratorModel(cfg.LLMProvider, llmProvider.Roles()),
		// ADR-0138: document identity checks and the version chain, over the same pool;
		// its retirement erases fact items through this knowledge store in one transaction.
		Documents: docLifecycle,
	}

	// 2. Domain stacks — sequential construction (dependency order)
	// ENG-1504: the raw adapter's capabilities as typed fields, bound once to the
	// concrete adapter, so a capability it stops implementing is a compile error here
	// instead of a feature NewMemoryStack silently switches off.
	memBackend := kernel.NewMemoryBackend(vec)
	// ADR-0137 S2 (ENG-411 stage A): the raw memory eraser over the document lifecycle.
	// NewMemoryStack wraps it in the S3 enforcing decorator (only with
	// execution.erasure.enabled) and exposes only that, MemoryStack.Eraser().
	// S7: plugin participants erase their evidence-keyed rows in the same transaction;
	// observers (the premium signed receipt) are told after commit.
	erasureStore := postgres.NewPgErasureStore(docLifecycle,
		postgres.WithErasureAsyncDeadline(cfg.Execution.Erasure.AsyncDeadline()),
		postgres.WithErasureParticipants(opts.ErasureParticipants...),
		postgres.WithErasureObserver(opts.ErasureObserver))
	memBackend.Eraser = erasureStore
	mem := kernel.NewMemoryStack(memBackend, memoryGen, embedder, cfg.Execution, authorizer, cfg.Chunker, factsGen, factsSubstrate)
	// ADR-0137 S6 (ENG-411 stage C): the two-phase forget service every forget door
	// shares (agent-plane PreviewForget/Forget, operator-plane PreviewForgetMemory/
	// ForgetMemory, the forget_memory MCP tool). Built over MemoryStack.Eraser() — the
	// enforcing chokepoint, nil when execution.erasure.enabled is off, in which case
	// every door answers "erasure disabled".
	//
	// ENG-1948: the agent query form (PreviewForget.query, forget_memory `query`)
	// resolves free text with the caller's own recall — mem.QueryService, the same
	// searcher QueryMemory serves — so it sees exactly what that caller's search sees.
	// ENG-1953: it runs that pass without spreading (QueryService.SearchMatches) so
	// each hit keeps its cosine, floors hits on it (execution.erasure.preview_min_cosine)
	// and caps the list (preview_max_candidates).
	forgetSvc := forget.New(mem.Eraser(), forget.Config{
		PreviewTTL: cfg.Execution.Erasure.PreviewTTL(),
		PolicyFingerprint: fmt.Sprintf("agent_erase=%t;max_targets=%d",
			cfg.Execution.Erasure.AgentEraseEnabled, cfg.Execution.Erasure.MaxTargets()),
		Searcher:      mem.QueryService,
		SnippetChars:  cfg.Execution.Erasure.PreviewSnippet(),
		MinCosine:     cfg.Execution.Erasure.PreviewMinCos(),
		MaxCandidates: cfg.Execution.Erasure.PreviewMaxCands(),
	})
	if cfg.Execution.Erasure.Enabled && cfg.Execution.Erasure.AgentEraseEnabled {
		slog.Info("ENG-1953: forget query preview bounds",
			"min_cosine", cfg.Execution.Erasure.PreviewMinCos(),
			"max_candidates", cfg.Execution.Erasure.PreviewMaxCands())
	}
	// ADR-0137 S5: the one drainer of the erasure outbox (CAS deletes of erased evidence
	// and source-document bodies; request completion; tombstone retention). Started only
	// with erasure on — off, nothing writes the outbox.
	if cfg.Execution.Erasure.Enabled {
		ew := erasureworker.New(erasureStore, storeHandle.ContentEraser, erasureworker.Config{
			Poll:               cfg.Execution.Erasure.WorkerPoll(),
			MaxAttempts:        cfg.Execution.Erasure.WorkerAttempts(),
			TombstoneRetention: cfg.Execution.Erasure.TombstoneRetention(),
		}, slog.Default())
		lifecycles = append(lifecycles, Lifecycle{Name: "erasure-worker", Start: ew.Start, Stop: ew.Stop})
		slog.Info("ADR-0137: erasure worker configured",
			"poll", cfg.Execution.Erasure.WorkerPoll(), "max_attempts", cfg.Execution.Erasure.WorkerAttempts(),
			"async_deadline", cfg.Execution.Erasure.AsyncDeadline(),
			"tombstone_retention_days", cfg.Execution.Erasure.TombstoneRetentionDays)
	}
	// ADR-0103 D3/D7: wire the decision-provenance seam. nil observer (the OSS default)
	// leaves the emit site a nil check. The fingerprint is computed ONCE here, not per
	// query: it describes configuration that cannot change without a restart, so hashing
	// it on the retrieval path would be pure cost. The embedder is identified by model
	// name — its dimension is implied by the model and enforced by the pgvector schema,
	// so there is no separate dimension field to fold in.
	mem.QueryService.SetDecisionObserver(opts.DecisionObserver, cfg.Execution.RetrievalFingerprint(cfg.Embedder.Model))
	// ADR-0118 D5: the fact-lane substrate consultant. nil (the OSS default)
	// leaves the consult site a nil check and behaviour bit-identical.
	mem.QueryService.SetSubstrateConsultant(opts.SubstrateConsultant)
	// ADR-0048 #1: let Tier-2 commit offload a promoted fact's full body to CAS so
	// recall serves {summary + content_cid} instead of the full text.
	mem.Agent.ContentStore = storeHandle.ContentStore

	// ADR-0085: the agent-facing memory query path runs through the fail-closed read
	// chokepoint, UNCONDITIONALLY. There is no flag: the kernel always asks, and what
	// the answer is depends only on which decision point is installed.
	//
	// That wiring now happens inside kernel.NewMemoryStack, which passes the enforcing
	// read store and this same authorizer to NewQueryService as constructor arguments.
	// It used to be an EnableAuthorization call HERE, which made the security property
	// depend on this one line in this one function being reached — correct, but only by
	// convention, and the failure mode was a silent unfiltered read rather than an error.

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
		return nil, err
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
	eventBus.Subscribe(domain.EventTypeAgentReady, func(e domain.DomainEvent) {
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
	// ADR-0130 D1: each spawned agent receives ITS credential in its process
	// environment (CAMBRIAN_AGENT_TOKEN + CAMBRIAN_AGENT_ID), resolved per
	// spawn through the late-bound named-secret seam.
	meta.Manager.InstanceManager.SetAgentCredentialSource(agentCredentialEnv)
	// ADR-0130 D4: and HOW to reach the plane — the bound address and, on a
	// pure-TLS routable bind, the TLS root — resolved from the same listener
	// policy that binds it (agent_transport.go). Without this a TLS-only
	// routable kernel could not serve its own agents: they dialled
	// localhost:<port> in plaintext.
	agentTr, agentTrWarn, err := agentTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("ADR-0130 agent transport: %w", err)
	}
	if agentTrWarn != "" {
		slog.Warn("ADR-0130: "+agentTrWarn, "tls_cert_file", cfg.Server.TLSCertFile)
	}
	// ADR-0157 S2: kernel-only (network-denied) sandboxed instances reach the
	// same gRPC plane through a unix socket in their own socket directory;
	// served from the moment the gRPC server starts (agent_kernel_socket.go).
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
		return nil, apprErr
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

	// ADR-0053 Phase 0: KG²RAG one-hop chunk expansion (config-gated). The
	// pgvector adapter doubles as the ChunkTripletsStore (per-chunk h, r, t
	// extracted at write time or via the offline chunk-fill CLI). The
	// pipeline walks per-chunk triplets from the seed chunks, pulls in
	// chunks that share entities, and feeds them into the same cosine
	// re-rank. Opt-in via `execution.kg2rag_enabled` in config.json so the
	// A/B test (KG²RAG on vs off) is a config flip, not a rebuild. The
	// max-hops / max-expanded / max-entities knobs bound the expansion.
	if cfg.Execution.Retrieval.KG2RAGEnabled {
		mem.QueryService.EnableKG2RAG(memBackend.ChunkTriplets,
			cfg.Execution.Retrieval.KG2RAGMaxHops,
			cfg.Execution.Retrieval.KG2RAGMaxExpanded,
			cfg.Execution.Retrieval.KG2RAGMaxEntities,
			cfg.Execution.Retrieval.KG2RAGPerEntity,
		)
		// LLM-free, structure-aware recall: seed kgExpand from entities extracted
		// from the query text (ADR-0053). Needs KG²RAG (the chunk_triplets store).
		if cfg.Execution.Retrieval.QueryEntitySeedingEnabled {
			mem.QueryService.EnableQueryEntitySeeding()
			slog.Info("ADR-0053: query-entity seeding ENABLED (LLM-free recall)")
		}
		// Document-local anchor promotion (companion to the deterministic anchor
		// tier). Also needs the chunk_triplets store; LLM-free. ADR-0053.
		if cfg.Execution.Retrieval.AnchorConstraintEnabled {
			mem.QueryService.EnableAnchorConstraint()
			slog.Info("ADR-0053: anchor constraint ENABLED (document-local anchor promotion)")
		}
	} else {
		slog.Info("ADR-0053: KG²RAG expansion DISABLED via config (kg2rag_enabled=false)")
	}

	// ADR-0054 Stage A: multi-signal blend re-rank (cosine + recency + confidence
	// + pagerank + activation). On by default; `execution.retrieval.blend_enabled`
	// = false turns it off. Reads
	// chunk_pagerank (kept fresh by the pagerank-recompute worker) + per-chunk
	// confidence; bge cross-encoder (Stage B) is a separate, later flag.
	if cfg.Execution.Retrieval.BlendEnabled {
		w := memory.BlendWeights{
			Cosine:         cfg.Execution.Retrieval.BlendWeightCosine,
			Recency:        cfg.Execution.Retrieval.BlendWeightRecency,
			Confidence:     cfg.Execution.Retrieval.BlendWeightConfidence,
			PageRank:       cfg.Execution.Retrieval.BlendWeightPageRank,
			Activation:     cfg.Execution.Retrieval.BlendWeightActivation,
			Lexical:        cfg.Execution.Retrieval.BlendWeightLexical,
			GraphCoherence: cfg.Execution.Retrieval.BlendWeightCoherence,
		}
		if (w == memory.BlendWeights{}) {
			w = memory.DefaultBlendWeights() // all-unset ⇒ ADR defaults
		}
		blender := memory.NewBlender(w)
		mem.QueryService.EnableBlend(&blender, vec) // *PgVectorAdapter implements RankSignalStore
		curve := memory.RecencyCurve{
			Decay:    cfg.Execution.Retrieval.BlendRecencyMode == "decay",
			HalfLife: time.Duration(cfg.Execution.Retrieval.BlendRecencyHalfLifeHours * float64(time.Hour)),
		}
		mem.QueryService.SetRecencyCurve(curve)
		slog.Info("ADR-0054: Stage-A multi-signal blend ENABLED", "weights", w,
			"recency_mode", map[bool]string{false: "minmax", true: "decay"}[curve.Decay],
			"recency_half_life_h", cfg.Execution.Retrieval.BlendRecencyHalfLifeHours)
	}

	// ADR-0136 ranking v2 (Phase 0): D1 calibrated Stage A and D3 query-conditioned
	// time (D2, a per-document cap, was rejected by the owner 2026-11-03 and removed).
	// Wired unconditionally — every switch defaults off and the zero RankingV2 is the
	// old ranking — so the boot line below states the EFFECTIVE value of each, which
	// is what an experiment arm's expect_log checks. D1 and the D3 features act inside
	// Stage A, so they need blend_enabled; D3's explicit window acts without it. The
	// line says which applies.
	rv2 := cfg.Execution.Retrieval
	fc := cfg.Execution.Facts
	mem.QueryService.SetRankingV2(memory.RankingV2{
		Calibrated:         rv2.StageACalibrated,
		LexicalSaturationK: rv2.LexicalSaturationK,
		TemporalIntent:     rv2.TemporalIntentEnabled,
		TemporalFitWeight:  rv2.TemporalFitWeight,
		// ADR-0136 D4/D5: the fact lane and its features (execution.facts).
		FactLane:     fc.LaneEnabled,
		FactLaneTopK: fc.LaneTopK,
		// ADR-0136 A4: the document-scoped lane (0 = off). Acts only inside the lane.
		FactLaneScopedDocuments: fc.LaneScopedDocuments,
		FactLaneScopedTopK:      fc.LaneScopedTopK,
		FactMatchWeight:         fc.FactMatchWeight,
		Supersession:            fc.SupersessionEnabled,
		CurrentFactWeight:       fc.CurrentFactWeight,
		// ENG-432: fact-reached chunks rank on max(own, matched fact) cosine. Off by
		// default (owner 2026-11-15) until its A/B/A.
		FactChunkCosineMax: fc.LaneChunkCosineMax,
	})
	slog.Info(fmt.Sprintf("ranking v2: stage_a_calibrated=%t temporal_intent_enabled=%t",
		rv2.StageACalibrated, rv2.TemporalIntentEnabled),
		"lexical_saturation_k", rv2.LexicalSaturationK,
		"temporal_fit_weight", rv2.TemporalFitWeight,
		"blend_enabled", rv2.BlendEnabled)
	if (rv2.StageACalibrated || rv2.TemporalIntentEnabled) && !rv2.BlendEnabled {
		slog.Warn("ranking v2: stage_a_calibrated / temporal_intent_enabled change Stage-A scoring, which is OFF (blend_enabled=false); only the explicit-range window of temporal intent applies")
	}
	if rv2.TemporalIntentEnabled && rv2.BlendWeightRecency == 0 {
		slog.Info("ranking v2: temporal intent order (latest/current/earliest) sets the recency direction, but blend_weight_recency=0, so only the temporal-fit feature and explicit windows take effect")
	}
	// ADR-0136 D4/D5: the fact layer's boot line, the expect_log anchor of the fact arms.
	// It states the EFFECTIVE switches: extraction needs the adapter's fact store, the
	// lane needs the fact reader, so a switch that cannot act is logged false with why.
	factsExtract := fc.ExtractEnabled && mem.FactExtractor != nil
	factsLane := fc.LaneEnabled && mem.QueryService.HasFactReader()
	factsStore := "none"
	if mem.FactWriter != nil {
		factsStore = "substrate"
	}
	// ADR-0136 A1.11: the boot line carries store=substrate and the effective
	// assessments switch (it needs supersession), the expect_log anchors of the
	// adr0136-gate-facts arms.
	factsAssess := fc.AssessmentsEnabled && fc.SupersessionEnabled && factsExtract
	// ADR-0136 A4: the scoped lane's EFFECTIVE N (it runs inside the lane, so 0 when the
	// lane cannot act). APPENDED to the line so the existing expect_log prefixes match.
	factsScopedDocs := 0
	if factsLane {
		factsScopedDocs = fc.LaneScopedDocuments
	}
	// ENG-490: the extractor version and A2 budgets are appended (fact_boot_line.go).
	factsVersion := ""
	if factsExtract {
		factsVersion = mem.FactExtractor.ExtractorVersion()
	}
	slog.Info(factLayerBootLine(factLayerBoot{
		Extract: factsExtract, Lane: factsLane, Supersession: fc.SupersessionEnabled, Assessments: factsAssess,
		Store: factsStore, ScopedDocuments: factsScopedDocs, ScopedTopK: fc.LaneScopedTopK,
		ExtractorVersion: factsVersion, MaxCallsPerDocument: fc.MaxCallsPerDocument,
		MaxFactsPerSource: fc.MaxFactsPerSource, MaxPromptChars: fc.MaxPromptChars,
	}),
		"single_attributes", fc.SingleAttributes,
		"multi_attributes", fc.MultiAttributes,
		"cardinality_min_votes", fc.CardinalityMinVotes,
		"item_embedding_model", cfg.Embedder.Model,
		"item_embedding_dims", cfg.Embedder.Dimensions,
		"lane_top_k", fc.LaneTopK,
		"fact_match_weight", fc.FactMatchWeight,
		"current_fact_weight", fc.CurrentFactWeight,
		"lane_chunk_cosine_max", fc.LaneChunkCosineMax && factsLane,
		"fuzzy_candidates", fc.FuzzyCandidates,
		"sync_ingest_wait_for_facts", cfg.Execution.Ingestion.SyncIngestWaitForFacts,
		"generator_role", string(domain.PurposeFacts))
	if (fc.ExtractEnabled && !factsExtract) || (fc.LaneEnabled && !factsLane) {
		slog.Warn("fact layer: a switch is on but no knowledge substrate was composed; it has no effect")
	}
	if fc.LaneScopedDocuments > 0 && !factsLane {
		slog.Warn("fact layer: lane_scoped_documents needs lane_enabled (and a knowledge substrate); the scoped lane is off",
			"lane_scoped_documents", fc.LaneScopedDocuments)
	}
	if fc.LaneChunkCosineMax && !factsLane {
		slog.Warn("fact layer: lane_chunk_cosine_max needs lane_enabled (and a knowledge substrate); it has no effect")
	}
	if fc.AssessmentsEnabled && !factsAssess {
		slog.Warn("fact layer: assessments_enabled needs extract_enabled and supersession_enabled; no assessment call is made")
	}
	if fc.SupersessionEnabled && len(fc.SingleAttributes) == 0 && !fc.AssessmentsEnabled {
		slog.Info("fact layer: supersession_enabled with no declared single_attributes and assessments off: only change-language cues and negations close a value (no cardinality can be learned without assessments)")
	}
	if fc.SupersessionEnabled && !fc.ExtractEnabled {
		slog.Info("fact layer: supersession runs inside extraction, which is off; only the lane's reading of already-stored chains applies")
	}
	if fc.LaneEnabled && fc.SupersessionEnabled && !rv2.TemporalIntentEnabled {
		slog.Info("fact layer: current_fact and the history chain need retrieval.temporal_intent_enabled (D3 order); without it the lane only annotates supersession")
	}
	if fc.LaneEnabled && !rv2.BlendEnabled {
		slog.Info("fact layer: fact_match / current_fact are Stage-A features and blend_enabled=false; fact-reached chunks still enter as primary hits")
	}
	if rv2.StageACalibrated && rv2.BlendWeightPageRank > 0 {
		slog.Info("ranking v2: calibrated PageRank is the interim corpus-relative scale x*N/(x*N+1), not a global percentile (T-20261103-adr-0136-d1-pagerank-feature-is-an-interim-x-n)")
	}

	// ADR-0054 hybrid retrieval: fuse dense (vector) + sparse (lexical/full-text)
	// via RRF so exact-token chunks the embedder misses enter the pool. Opt-in via
	// execution.hybrid_search_enabled. The lexical half is the typed MemoryBackend
	// field (ENG-1504), not a runtime probe of the adapter.
	if cfg.Execution.Retrieval.HybridSearchEnabled {
		mem.QueryService.EnableHybrid(memBackend.Lexical, cfg.Execution.Retrieval.HybridRRFK)
		mem.QueryService.SetLexicalWeight(cfg.Execution.Retrieval.HybridLexicalWeight)
		slog.Info("ADR-0054: hybrid dense+lexical retrieval ENABLED", "rrf_k", cfg.Execution.Retrieval.HybridRRFK, "lexical_weight", cfg.Execution.Retrieval.HybridLexicalWeight)
	}

	// ADR-0054 Stage B: cross-encoder rerank of the top-K Stage-A candidates via
	// the warm reranker_agent system organ (bge cross-encoder), invoked DIRECTLY
	// through the agent transport (no selection) — the same privileged-organ pattern as
	// the kg_extractor. Opt-in via execution.reranker_enabled. Fail-soft: a
	// down/erroring agent leaves the Stage-A order intact. The model id is the
	// agent's RERANK_MODEL env (large = ceiling, base/v2-m3 = CPU edge).
	if cfg.Execution.Retrieval.NeighborWindowEnabled {
		mem.QueryService.EnableNeighborWindow()
		slog.Info("ADR-0060: neighbor-window expansion ENABLED")
	}
	if cfg.Execution.Retrieval.DocumentCompletionEnabled {
		mem.QueryService.EnableDocumentCompletion(cfg.Execution.Retrieval.DocumentCompletionDocuments, cfg.Execution.Retrieval.DocumentCompletionMaxChunks)
		slog.Info("ENG-430: document/session completion ENABLED",
			"documents", cfg.Execution.Retrieval.DocumentCompletionDocuments, "max_chunks", cfg.Execution.Retrieval.DocumentCompletionMaxChunks)
	}
	if cfg.Execution.Retrieval.ChronologicalContextEnabled {
		mem.QueryService.EnableChronologicalContext()
		slog.Info("ADR-0134 D5a: chronological answer context ENABLED (passages ordered and dated by event time)")
	}
	if cfg.Execution.Retrieval.RerankerEnabled {
		mem.QueryService.EnableReranker(
			&subnetwork.RerankerDispatcher{Caller: meta.AgentTransport, AgentID: "reranker_agent"},
			cfg.Execution.Retrieval.RerankerTopK,
			cfg.Execution.Retrieval.RerankerWeight,
		)
		slog.Info("ADR-0054: Stage-B cross-encoder rerank ENABLED",
			"top_k", cfg.Execution.Retrieval.RerankerTopK, "w_bge", cfg.Execution.Retrieval.RerankerWeight)
	}

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

	// 9. ArtifactVault — content-addressable storage for agent outputs
	vaultPath := filepath.Join(cfg.Storage.DataDir, "vault")
	artifactVault := vault.NewArtifactVault(vaultPath)

	// 10. Server assembly — the consumer, not a producer
	// ADR-0032: ReactiveEngineArgs wires the premium ReactiveEngine with real deps.
	// ADR-0057: reactive signal receiver via injection (no build tags). OSS default
	// is the Watcher (LTM enrichment + Planner dispatch); the premium binary injects
	// a ReactiveEngine built from the KernelServices bundle.
	var signalRcv domain.SignalReceiver
	if watcher != nil {
		signalRcv = watcher
	} else {
		signalRcv = &ossreactive.NoOpSignalReceiver{}
	}
	// The capability bundle handed to plugins. Built once so the ADR-0082 D12 Build phase
	// and the ADR-0057 NewSignalReceiver hook see exactly the same services.
	// ADR-0098: plugins are Built before the chat lane exists, so the progress channel is
	// wired through holders that are handed over now and filled in below.
	progressSink := &progressHolder{}
	progressOut := &progressDelivererHolder{}
	// Act-for conversations and principal-owned scheduled tasks (scheduled_tasks.go):
	// premium contributes the directory and the store at Build; the kernel binds the
	// owned-turn runner once the chat lane exists.
	principalSchedules := &principalSchedulesHolder{}
	streamRegistry := &streamRegistryHolder{}
	principalDirectory := &principalDirectoryHolder{}
	ownedTurns := &ownedTurnRunner{}
	// ADR-0141: the credential provider's late-bound seams. Created here, before
	// plugins Build, so KernelServices.ResolveCredential / OrgCredentials can be
	// handed out now and answer once startKernelServices builds the provider.
	credSeams := &credentialSeams{directory: principalDirectory.get, approvals: toolApproval}

	// Late-bound on purpose: the ingestion processor is part of the server, which is
	// constructed AFTER this bundle. The plugin holds this pointer from Build and
	// calls it at request time, by which point the processor is populated — the same
	// shape plugin-contributed reactive action executors use. Constructing it here with a nil
	// processor and filling it in later is what keeps the bundle order-independent
	// rather than adding a second undeclared ordering dependency.
	// ENG-494: the unscoped ingestor (handed to the reactive signal receiver) writes
	// as source:reactive:<doc source>; buildPlugins gives each plugin its own,
	// named after its manifest id.
	memIngestor := &kernelMemoryIngestor{connector: reactiveIngestConnector}
	// PREM-0032 start_plan: late-bound for the same reason — the server whose
	// Execute path runs plans is constructed after this bundle; bound below.
	// (planExec itself is declared above the Watcher, which also runs plans.)
	// One reader, shared by the plugin seam and the operator RPC.
	// mem.Reader() is the stack's tier-1 fail-closed read chokepoint (AGENTS.md §2.1:
	// NewMemoryStack is the only place it is built; T-20261113-enforcingvectorstore-is-constructed-outside).
	docReader := newKernelDocumentReader(mem.Reader(), authorizer) // RESOLVED authorizer: it turns the principal into the predicate the by-id read needs (ENG-1610)

	// ADR-0105/0108: the evidence foundation's pieces, built ONCE here so the
	// plugin seam, the ingest chokepoint and the outbox consumer share one
	// ingestor and one store. Construction is fail-loud: with the flag on, a
	// kernel that cannot preserve evidence must not boot into silently-not-
	// preserving it.
	// The live ontology (onto: the kind and link-verb registries per namespace,
	// ADR-0149 D2) and the substrate store (knowledgeStore) are built above
	// NewMemoryStack (ADR-0136 A1.6).

	var evIngestor *evidence.Ingestor
	// The substrate's stores (app.NewSubstrate, above). The identity plane
	// (five-planes steps 1+2, migration 0016) sits beside the others so every lane
	// that mints an entity or asserts a link goes through the one chokepoint that
	// enforces the plane's refusals. ADR-0111/ADR-0118: the closed query AST over
	// all of them is built ONCE, so the plugin query seam, the names-and-counts
	// catalog seam and the operator review inbox all read through the same fenced
	// plane; it reads the identity plane too, so `entity` / `why` / expand_aliases
	// answer over the stores the producers write to.
	evStore := substrate.Evidence
	eventStore := substrate.Events
	entityStore := substrate.Entities
	linkStore := substrate.Links
	identityScans := substrate.IdentityScans
	queryPlane := substrate.Query
	if cfg.Execution.Ingestion.EvidenceCaptureEnabled {
		var everr error
		evIngestor, everr = substrate.Ingestor(storeHandle.ContentStore)
		if everr != nil {
			return nil, fmt.Errorf("evidence capture (ADR-0105): %w", everr)
		}
	}

	// ADR-0127 CL-2: the consent/ladder prompt hub. Constructed here — before
	// the plugin seam — so a conversation-surface plugin (Telegram, console)
	// can subscribe via Watch and answer via Submit; consulted by the tool
	// executor pre-dispatch below. With nothing subscribed every
	// prompt-requiring contributed step refuses: the sealed fail-closed
	// default, not a degradation.
	// ADR-0142 D11: a VIEW over the one approval hub (default rule: self — the
	// beneficiary answers), not a second implementation.
	workerConsent := toolApproval.Consent()
	// The hub's recovery + expiry sweeper: pending approvals left by a previous
	// process expire (and publish) once their window passes.
	lifecycles = append(lifecycles, Lifecycle{Name: "approval-hub", Start: toolApproval.Start, Stop: toolApproval.Stop})

	// The knowledge proposal -> approval -> commit bridge
	// (T-20261107-knowledge-substrate-extensions-for-case). It holds the RAW knowledge
	// store because it IS the kernel path that stamps actors: a candidate is asserted by
	// the proposer the transport authenticated, a commit by the approver the hub
	// authenticated (its knowledge_commit handler is registered here, at boot, so an
	// item approved after a restart still commits). Served on the agent plane as
	// Orchestrator/ProposeKnowledge (cambrianServer.Knowledge below).
	// The bridge refuses cited evidence the proposer may not read (and unknown ids
	// with the same answer, so the refusal is no existence oracle). Without an
	// evidence store it refuses EVERY citation, so the store is wired whenever it
	// exists; a nil *PgEvidenceStore must not be wrapped in the interface (it would
	// panic on Get) (T-20261107-propose-knowledge-link-proposals-do-not-check).
	var kbOpts []knowledgecommit.Option
	if evStore != nil {
		kbOpts = append(kbOpts, knowledgecommit.WithEvidence(evStore))
	}
	knowledgeBridge, kbErr := knowledgecommit.New(knowledgeStore, onto, authorizer, toolApproval, linkStore, slog.Default(), kbOpts...)
	if kbErr != nil {
		return nil, fmt.Errorf("knowledge commit bridge: %w", kbErr)
	}
	// ADR-0149 D2: agents draft ontology changes; a person approves. The drafts
	// bridge registers the ontology_change handler here, at boot (an item approved
	// after a restart still applies — as its approver), audits every transition
	// through the operator audit seam, and records denials and expiries on a
	// reconcile loop. Served on the agent plane as Orchestrator/ProposeOntologyChange
	// (cambrianServer.OntologyDrafts below).
	ontologyDrafts, odErr := ontology.NewDrafts(vec.Pool(), onto, toolApproval, credSeams.recordAudit, slog.Default())
	if odErr != nil {
		return nil, fmt.Errorf("ontology drafts: %w", odErr)
	}
	lifecycles = append(lifecycles, Lifecycle{Name: "ontology-drafts", Start: ontologyDrafts.Start, Stop: ontologyDrafts.Stop})

	kernelSvc := KernelServices{
		Principals: idStores.registry,
		// ADR-0085: the policy plugin owns its own tables and needs to tell a
		// registered-but-unprofiled agent from an unknown principal, so it gets the
		// pool, the registry existence check, and the session caller term. It gets
		// nothing else about the kernel.
		SetProgressSink: progressSink.set,
		DeliverProgress: progressOut.deliver,
		WorkerConsent:   workerConsent,
		Approvals:       toolApproval, // ADR-0142 D11
		SQL:             vec.Pool(),
		// ADR-0104 D6.2: a plugin resolves a reference by ASKING, never by holding
		// a store. The reader runs Authorizer.ReadFilter for the plugin's principal,
		// so the plugin says who it is and the kernel decides what it may see.
		//
		// Wired with the RESOLVED authorizer, not opts.Authorizer. Two reasons, and
		// both were checked rather than assumed: plugin composition has already run
		// by here (`opts = composed.opts`), so a policy plugin's decision point
		// governs plugin reads too; and the resolved value carries the OSS
		// AllowAll default, where a raw nil would make this reader refuse every
		// read in exactly the deployment for which unrestricted is correct.
		// The ENFORCING store, not the raw adapter: the decorator IS the enforcement
		// point, and handing over `vec` would skip it. It also searches every
		// idLookupTable, which the raw `documents`-only read did not — chunks holds
		// the overwhelming majority of rows and every `source_doc:` entity.
		// ADR-0104 D3: one write path. A plugin that receives content puts it in the
		// brain through the SAME pipeline as every other ingest, rather than beside
		// it — which is what makes the content chunked, structured, extracted and
		// retrievable instead of merely detected-over and dropped.
		Ingestor:    memIngestor,
		Documents:   docReader,
		AgentExists: reg.HasAgent,
		// Contract 0074: enumeration, not just existence. AgentExists answers "is
		// this one real?"; ListPrincipals needs "which ones are", to find a policy
		// linked to a principal that is not.
		Agents: reg,
		// The one WRITE the registry exposes to a plugin, bounded per plugin in
		// buildPlugins to its own id namespace and to non-system agents. It
		// exists so a plugin that gains a unit at runtime — a second Telegram
		// bot, say — can actually run it, instead of recording it and waiting
		// for a restart nobody mentioned.
		RegisterPipelineLister:    func(l domain.PipelineLister) { pipelineListers.add(l) },
		RegisterPipelineDryRunner: func(r domain.PipelineDryRunner) { pipelineDryRunners.add(r) },
		RegisterPipelineAuthor:    func(a domain.PipelineAuthor) { pipelineAuthors.add(a) },
		RegisterPipelineWriter:    func(w domain.PipelineWriter) { pipelineWriters.add(w) },
		RegisterPipelineLifecycle: func(l domain.PipelineLifecycle) { pipelineLifecycles.add(l) },
		RegisterIngressLister:     func(l domain.IngressLister) { ingressListers.Store(&l) },
		// The write half was DECLARED on this bundle and never wired — so the
		// authz plugin's registration was skipped by its own nil-guard and
		// svc.DeregisterIngress silently no-opped. A nil-guarded seam whose
		// producer side is missing fails in the quietest possible way, which
		// is why both halves are wired on adjacent lines now.
		RegisterIngressDeregistrar:    func(d domain.IngressDeregistrar) { ingressDeregistrars.Store(&d) },
		RegisterIngressRegistrar:      func(r domain.IngressRegistrar) { ingressRegistrars.Store(&r) },
		RegisterIngressSchemaDeclarer: func(d domain.IngressSchemaDeclarer) { ingressSchemaDeclarers.Store(&d) },
		RegisterTurnRouter:            func(r domain.TurnRouter) { turnRouters.Store(&r) },
		RegisterPrincipalSchedules:    principalSchedules.set,
		RegisterStreamRegistry:        streamRegistry.set,
		RegisterPrincipalDirectory:    principalDirectory.set,
		RunOwnedTurn:                  ownedTurns.run,
		RegisterAgent: func(def domain.AgentDefinition) error {
			return reg.SetAgentWithManifest(def, nil)
		},
		// Who has come through an entry point, from the DURABLE conversation
		// record. The decision journal cannot answer that: it is in-memory and
		// only records when something asks the decision point, so a turn that
		// answers a greeting leaves no trace at all.
		IngressTraffic: ingressTraffic,
		// Read-only: so a plugin can tell whether the surface its traffic will
		// arrive on has been registered, without being able to register one.
		Ingresses:     opts.IngressResolver,
		SessionScopes: sessionMgr,
		// ADR-0106: the substrate's typed item/resolution boundary. A plugin
		// producing or consuming knowledge items goes through this port so no
		// consumer ever grows SQL against substrate tables.
		//
		// ADR-0136 A1: the kernel-reserved memory_fact kind is fenced off this
		// seam (it would bypass both fact chokepoints); plugins read memory_fact
		// resolutions through the SCOPED query plane (A1-Q6).
		//
		// ENG-449 (owner decision 2026-11-15): item WRITES go through the
		// EnforcingKnowledgeStore chokepoint — ClassifyWrite for the principal on ctx,
		// the item's (evidence) classification as the hint. buildPlugins wraps this
		// per plugin with knowledgeSourceFallback, so a write made with no principal
		// runs as source:<plugin>:<namespace> rather than as no one.
		//
		// The designated-actor guard (inside it) refuses a plugin item that would be a
		// designated principal's word or carry the kernel's commit marker, so for a kind
		// resolved by designated_actor the knowledge_commit handler is the only writer
		// of its committed value (T-20261107-knowledge-substrate-extensions-for-case).
		Knowledge: NewPluginKnowledgeSeam(knowledgeStore, onto, authorizer),
		// ADR-0108 D2: the typed event/observation boundary — exact reads over
		// stored rows, nothing embedded.
		// Withdrawing an entry organ (ADR-0090). Resolved at CALL time through the
		// same holder the reader uses, so a plugin registered in any order can
		// still deregister what it owns.
		DeregisterIngress: func(ctx context.Context, agentID string) error {
			p := ingressDeregistrars.Load()
			if p == nil || *p == nil {
				return nil // no registry in this build; nothing to withdraw from
			}
			return (*p).DeregisterIngress(ctx, agentID)
		},
		// Declaring what an ingress's items carry (ADR-0117), resolved at call
		// time like its siblings so plugin build order does not matter.
		DeclareIngressSchema: func(ctx context.Context, agentID string, fields []domain.IngressSchemaField) error {
			p := ingressSchemaDeclarers.Load()
			if p == nil || *p == nil {
				return nil // no registry in this build; nothing to declare on
			}
			return (*p).DeclareIngressSchema(ctx, agentID, fields)
		},
		// Declaring an entry organ (ADR-0090 D2), resolved at call time like its
		// siblings. A no-op without a registry, which keeps an OSS kernel and a
		// registry-less build working exactly as before.
		RegisterIngress: func(ctx context.Context, reg domain.IngressRegistration) error {
			p := ingressRegistrars.Load()
			if p == nil || *p == nil {
				return nil // no registry in this build; nothing to register with
			}
			return (*p).RegisterIngress(ctx, reg)
		},
		RetireIngressPipeline: func(ctx context.Context, agentID string) error {
			p := ingressPipelineRetirers.Load()
			if p == nil || *p == nil {
				return nil
			}
			return (*p)(ctx, agentID)
		},
		TracePipelinePayloads:  cfg.Execution.LLM.TracePipelinePayloads,
		PipelineDrainerEnabled: cfg.Execution.Pipelines.DrainerEnabled,
		Events:                 eventStore,
		Entities:               entityStore,
		Links:                  linkStore,
		// The identity producers' scan port (ENG-1393): retirements and scope
		// applied inside, so premium never scans substrate tables with raw SQL.
		IdentityScans: identityScans,
		// The verb vocabulary, so a plugin that takes a link verb from an
		// OPERATOR (the ingress studio's mapping confirm) can refuse an
		// undeclared one where a human is present, instead of leaving the
		// store to refuse every delivery later with nobody watching.
		Relations: onto,
		// ADR-0149 D2: the live declarations — read, propose/apply, and the
		// violations report AS a principal (scope resolved here, never chosen
		// by the plugin).
		Ontology: ontologies.Seam,
		// The approver's full view of an agent draft (the approval carries only
		// a bounded preview).
		OntologyDrafts: ontologyDrafts,
		// ADR-0149 D6/D7: the object tables' reads, as a principal (scope
		// resolved here, per property in the reader).
		Objects: substrate.ObjectsSeam(authorizer),
		// ADR-0111/ADR-0118: the closed query AST over all of the above, scoped.
		// The seam takes a principal, never a predicate; the RESOLVED authorizer
		// decides scope (same reasoning as the Documents seam above).
		QueryKnowledge: authz.QueryKnowledgeFunc(authorizer, queryPlane, slog.Default()),
		// The names-and-counts reads the AST has no shape for (explore surface,
		// ask grounding), through the same plane and the same scope resolution
		// (T-20261107-ask-knowledge-catalog-reads-entity-ids-and).
		KnowledgeCatalog: authz.KnowledgeCatalogFunc(authorizer, queryPlane, slog.Default()),
		Manager:          meta.Manager,
		Dispatcher:       meta.Selector, // plugins that select get the selector
		Planner:          aw.Planner,
		// The real DAG executor for plugin-generated plans (PREM-0032 start_plan),
		// reached through the Orchestrator.Execute path — see kernelPlanExecutor.
		PlanExecutor: planExec,
		LLM:          aw.LLM,
		WatchStore:   reg,
		// Authored pipelines, stored beside the watches they replace.
		PipelineStore: reg,
		EventBus:      eventBus,
		Journal:       reg, // REACT-01 / ADR-0061: durable reactive execution (bbolt).
		// ADR-0080: let a direct-dispatch consumer (chat manager) provision a managed-LLM
		// session token, the way the planner path does at server.go:493. Nil gateway ⇒ no-op.
		// The lease is AGENT-UNBOUND: no agent may present it (ADR-0139 D8) until the
		// consumer binds it (the core chat lane does, via TurnService.SetLeaseBinder).
		AcquireLLMToken: func(ctx context.Context, tokenLimit int, ttl time.Duration) (string, func(), error) {
			return acquireKernelLLMToken(ctx, llmGateway, "", tokenLimit, ttl)
		},
		// ADR-0139 D8: the same lease, bound to agentID at mint — what a plugin that
		// hands the lease to an agent must use
		// (T-20261104-leases-minted-with-no-agentid-are-usable-by-any).
		AcquireAgentLLMToken: func(ctx context.Context, agentID string, tokenLimit int, ttl time.Duration) (string, func(), error) {
			if agentID == "" {
				return "", func() {}, errors.New("acquire agent LLM token: agent id is required")
			}
			return acquireKernelLLMToken(ctx, llmGateway, agentID, tokenLimit, ttl)
		},
	}
	if evStore != nil {
		// The read half, so a lane holding an EvidenceID can reach its content
		// hash — the step that turns an id into bytes.
		kernelSvc.EvidenceStore = evStore
	}
	if evIngestor != nil {
		kernelSvc.EvidenceIngest = evIngestor.Ingest
		// ADR-0112 §6: the raw-delivery lane's split of the same ordering
		// contract — stage bytes now, journal a CID, ingest on the watch side.
		kernelSvc.StageEvidenceContent = evIngestor.Stage
		kernelSvc.FetchEvidenceContent = evIngestor.FetchStaged
	}
	// ADR-0112: name-at-a-time credential resolution for plugin lanes. Reads
	// through the late-bound holder Run attaches the config store to; before
	// attachment (or with the store off) it answers ok=false.
	kernelSvc.ResolveNamedSecret = resolveNamedSecret
	// ADR-0127 CL-5: machine-token verification for the broker's ingest lane,
	// through the same late-bound store the MCP endpoint authenticates against.
	// The owner becomes domain.PersonPrincipal(owner) — a `user` (ADR-0142 D3) —
	// exactly as the endpoint's WorkerOwner hop makes it (mcpserve middleware), so
	// both lanes name one principal.
	kernelSvc.VerifyMachineToken = func(machine, token string) (domain.PrincipalRef, bool) {
		owner, ok := mcpserve.VerifyMachineToken(resolveNamedSecret, machine, token)
		if !ok {
			return domain.PrincipalRef{}, false
		}
		return domain.PersonPrincipal(owner), true
	}
	// ADR-0112 §15: model-pinned generators for plugin lanes whose model is
	// operator configuration (the studio's drafter), resolved per call.
	kernelSvc.GeneratorForModel = llmProvider.GeneratorForModel
	kernelSvc.StoreNamedSecret = storeNamedSecret
	kernelSvc.ClearNamedSecret = clearNamedSecret
	kernelSvc.NamedSecretStatus = namedSecretStatus
	// ADR-0141 phase 3: outbound credentials for plugin consumers (an
	// ingress's poller authenticating to its source) come from the account
	// provider, late-bound (it is built in startKernelServices).
	kernelSvc.ResolveCredential = credSeams.resolveCredential
	kernelSvc.OrgCredentials = credSeams.orgWriter
	// ADR-0142 stage 2: premium operator planes (the org directory) run their
	// commands through the OperatorConsole mutation contract and audit store —
	// the same late-bound seams the credential provider gets.
	kernelSvc.OperatorCaller = operator.CallerFromContext
	kernelSvc.RunOperatorMutation = credSeams.runMutation
	kernelSvc.RecordAudit = credSeams.recordAudit

	// ADR-0082 D12: the Build phase — plugins construct their runtime objects now that the
	// stacks exist and before anything is served. Runs in dependency order; a plugin that
	// implements no Builder is skipped.
	if err := buildPlugins(composed.built, kernelSvc); err != nil {
		return nil, err
	}
	// REC-02: capabilities a plugin could only decide once it saw the deployment.
	// Collected AFTER every Build, so a plugin reports what it actually got rather
	// than what its build could in principle contribute — the record lane used to
	// advertise itself on a kernel with no Postgres, which made "no record lane
	// here" indistinguishable from "this kernel is broken".
	composed.capabilities = append(composed.capabilities, liveCapabilities(composed.built)...)

	// ADR-0084 D4: the OSS chat lane. A bounded pool of stateless session workers replaces
	// one process per conversation: process count becomes a configured constant instead of a
	// consequence of how many people are talking. Disabled by default (chat_pool_size = 0),
	// and skipped when the conversation store is unavailable — a chat lane with nowhere to
	// persist transcripts would lose them on restart, which is the failure this design exists
	// to remove.
	var chatTurns *corechat.TurnService
	// The one record of which live plan a chat turn delegated (turn_delegation.go):
	// the turn's end cancels it, and the turn's stall watchdog reads its progress
	// (T-20261106-a-chat-turn-that-fails-deadline-exceeded-or,
	// T-20261106-chat-stall-detection-cancels-a-turn-whose). Handed to the server
	// below; inert until a chat turn delegates.
	turnDelegations := subnetwork.NewTurnDelegations()
	if cfg.Execution.Chat.ChatPoolSize > 0 && conversations != nil {
		agentID := cfg.Execution.Chat.ChatPoolAgentID
		if agentID == "" {
			agentID = "chat_agent"
		}
		pool := agentpool.New(meta.Manager, agentpool.Config{
			AgentID:        agentID,
			Size:           cfg.Execution.Chat.ChatPoolSize,
			QueueSize:      cfg.Execution.Chat.ChatPoolQueueSize,
			AcquireTimeout: time.Duration(cfg.Execution.Chat.ChatPoolAcquireTimeoutSeconds) * time.Second,
			StreamPrefix:   "chat",
		})
		chatTurns = corechat.NewTurnService(conversations, pool, kernelSvc.AcquireLLMToken)
		// A scheduled task's firing runs its turn here, as its owner.
		ownedTurns.bind(conversations, chatTurns, func() domain.PrincipalDirectory {
			return principalDirectoryOrRegistry(principalDirectory.get(), idStores.registry)
		})
		// ADR-0084 D2: stamp each turn's conversation onto its lease, so work the turn
		// delegates to the planner opens a session linked back to the exchange that
		// ordered it — resolved server-side, never named by the agent.
		// ADR-0139 D8: and to the pool's worker agent, so no other agent can present it.
		chatTurns.SetLeaseBinder(llmGateway, agentID)
		// ADR-0098: always install the holder — the emission sites then need no nil check,
		// and whether anything is listening is decided later by whether a plugin filled it.
		chatTurns.SetProgressSink(progressSink)
		// The same holder answers "when did this conversation last do anything?", which is
		// what lets a wedged turn be cut loose instead of holding a worker and a lease for
		// the full TurnTimeout while reporting nothing.
		//
		// A model call in flight on the turn's lease counts too: a long unary
		// GenerateWithTools call emits no progress until it returns, so progress alone
		// read a healthy 60-90 s generation as a stall and cancelled it. The gateway
		// resolves conversation → lease through the binding SetLeaseBinder (above)
		// makes the turn stamp, and reports "now" while any call is outstanding. A turn
		// with no call in flight and no progress for the window is still cut loose
		// (T-20261105-slow-model-reliability-hardcoded-20-s).
		//
		// And the progress of the plan the turn delegated counts: its step leases are
		// bound to the run, not the conversation, so neither source above saw a plan
		// busy in a 90 s code-sandbox step, and the turn was cancelled while it worked.
		// turnDelegations reads each delegated plan's own liveness probe (step
		// boundaries, tool calls and approvals in flight, model calls on its leases).
		chatTurns.SetLivenessProbe(corechat.LatestActivity(progressSink.LastActivity, llmGateway.ConversationActivity, turnDelegations.ConversationActivity))
		// Both bounds are operator config (execution.chat.chat_turn_timeout_seconds /
		// chat_stall_timeout_seconds); 0 keeps the package default
		// (T-20261107-chat-turn-timeout-is-hardcoded-at-240-s).
		chatTurns.TurnTimeout = secondsOr(cfg.Execution.Chat.ChatTurnTimeoutSeconds, corechat.DefaultTurnTimeout)
		chatTurns.StallTimeout = secondsOr(cfg.Execution.Chat.ChatStallTimeoutSeconds, corechat.DefaultStallTimeout)
		// The language of the fixed failure sentences when the user's words name none
		// (execution.chat.chat_default_language). Not silent when unknown: the catalogue
		// would fall back to English and the operator should know their value was not used.
		if dl := cfg.Execution.Chat.ChatDefaultLanguage; dl != "" && !corechat.SupportedLanguage(dl) {
			slog.Warn("execution.chat.chat_default_language has no message catalogue; using English",
				"language", dl)
		}
		chatTurns.DefaultLanguage = cfg.Execution.Chat.ChatDefaultLanguage
		// A turn's end — reply, deadline, stall, failure, closed conversation — ends
		// the work it delegated and withdraws the approvals that work is waiting on.
		chatTurns.SetDelegationEnder(turnDelegations)
		// ADR-0090 D8: a reply to a conversation that arrived through an ingress goes
		// back out through that ingress. The address is resolved from the conversation
		// record, never supplied by the agent, and the registration is re-checked at
		// delivery time so revoking an ingress stops conversations already bound to it.
		deliverySvc := ingress.NewDeliveryService(
			conversations, opts.IngressResolver, ingress.NewDaemonTransport(meta.Manager))
		// ADR-0090 D8.1 (ENG-1172): two-phase delivery — every reply is recorded as a
		// prepared intent in the kernel's bbolt store before it is sent, confirmed
		// after, and redriven (or marked ambiguous) after a crash. No duplicates, no
		// silent drops.
		deliverySvc.SetJournal(reg)
		lifecycles = append(lifecycles, Lifecycle{
			Name:  "delivery-redrive",
			Start: deliverySvc.Start,
			Stop:  deliverySvc.Stop,
		})
		chatTurns.SetDeliverer(deliverySvc)
		// ADR-0098 D2: the same resolved, re-authorised envelope carries progress.
		progressOut.set(deliverySvc.DeliverProgress)
		lifecycles = append(lifecycles, Lifecycle{
			Name: "chat-pool",
			Start: func(ctx context.Context) {
				if err := pool.Start(ctx); err != nil {
					slog.Error("ADR-0084: chat pool failed to start; chat lane unavailable", "err", err)
				}
			},
			Stop: pool.Stop,
		})
	} else if cfg.Execution.Chat.ChatPoolSize > 0 {
		slog.Warn("ADR-0084: chat pool configured but the conversation store is unavailable; chat lane disabled")
	}

	// ADR-0127 CL-2: the conversation consent bridge — the OSS production
	// subscriber on the worker-consent hub. It renders approve/choose_machine
	// prompts (and parking notices) as plain text into the ORDERING
	// conversation through the chat lane's own Emit — store first, then the
	// ADR-0090 D8 delivery seam — and the inbound gate below consumes the
	// beneficiary's matching reply as the answer. Pure mechanism, default-on
	// wherever the chat lane runs; without a chat lane there is no
	// conversation to render into and the controller's fail-closed refusals
	// stand exactly as CL-2 shipped them.
	var consentBridge *ingress.ConsentBridge
	if chatTurns != nil {
		consentBridge = ingress.NewConsentBridge(workerConsent, chatTurns,
			time.Duration(cfg.MCP.WorkerConsentTimeoutMs)*time.Millisecond) // 0 ⇒ the controller's 120 s
		lifecycles = append(lifecycles, Lifecycle{
			Name:  "consent-bridge",
			Start: consentBridge.Start,
			Stop:  consentBridge.Stop,
		})
	}

	var watchHandler domain.WatchConfigHandler
	if opts.NewSignalReceiver != nil {
		signalRcv, watchHandler = opts.NewSignalReceiver(kernelSvc)
	}
	// REACT-05 / ADR-0071: the premium ReactiveEngine (signalRcv) also implements the
	// watch-observability ports; capture them (nil for the OSS receiver) so the operator
	// plane can wire GetWatchMetrics / BacktestWatch.
	var watchMetricsReader domain.WatchMetricsReader
	var watchBacktester domain.WatchBacktester
	if mr, ok := signalRcv.(domain.WatchMetricsReader); ok {
		watchMetricsReader = mr
	}
	if bt, ok := signalRcv.(domain.WatchBacktester); ok {
		watchBacktester = bt
	}
	// Phase 3/4: runs and checkpoints follow sessions into Postgres, so a run, its plan
	// and its checkpoints live in one place under one cascade. When the Postgres session
	// store is unavailable the bbolt registry (*kernel.AgentRepoDecorator) serves both.
	// Both are typed here and passed to the server (ENG-1504) — they used to be found by
	// asserting the agent registry inside ProvideServer and then overridden here.
	var runStore domain.RunStore = reg
	var checkpointStore executer.CheckpointStore = reg
	if pgSessions != nil {
		runStore, checkpointStore = pgSessions, pgSessions
	}
	cambrianServer := kernel.ProvideServer(cfg.Execution, mem, aw, meta, watcher, llmProvider, sessionMgr, llmGateway, observer, storeHandle.ContentStore, storeHandle.StepCache, signalRcv, watchHandler, runStore, checkpointStore, spendMeter)
	// ADR-0137 S6: the agent-plane PreviewForget / Forget / ListMyErasures doors.
	cambrianServer.Forgetter = forgetSvc
	cambrianServer.Delegations = turnDelegations
	cambrianServer.RunTracer = runTracer // ADR-0145 D10: step outputs + kernel tool calls
	// Orchestrator/ProposeKnowledge: candidates and commit requests (knowledgeBridge above).
	cambrianServer.Knowledge = knowledgeBridge
	// Orchestrator/ProposeOntologyChange: agent drafts (ontologyDrafts above).
	cambrianServer.OntologyDrafts = ontologyDrafts
	// ADR-0139 amendment 2026-11-06: the live step-output stream (step_output_wiring.go).
	// Built here, unconditionally, so the MCP endpoint below can subscribe to it; the
	// operator feed attaches its sink when services start.
	stepOutFeed := &stepOutputFeed{}
	cambrianServer.StepOutput = buildStepOutput(cfg.Execution.StepOutput, hookChain, llmGateway, reg, stepOutFeed)
	// PREM-0032: from here a plugin's start_plan runs on this server's real DAG path.
	planExec.bind(cambrianServer)
	// ADR-0098: the agent-facing plane reports memory searches and tool calls against the
	// conversation its caller's lease was issued under, so a slow turn shows movement
	// rather than one static line. Installed unconditionally — the holder is inert until
	// a plugin fills it.
	cambrianServer.Progress = progressSink
	// The operator-facing activity stream (append-only, names the tool). A plugin
	// subscribes via Registry.AddAgentActivityObserver; nil when none did, and the
	// tool path then emits nothing.
	cambrianServer.Activity = opts.AgentActivityObserver

	// ADR-0090: a signal from a REGISTERED ingress is a conversational turn, so it is
	// routed to the chat lane before the signal pipeline sees it — the planner never
	// receives external chat traffic (ADR-0080 D4). nil resolver or no chat lane and
	// this stays nil, leaving every signal on its ordinary path.
	if opts.IngressResolver != nil && chatTurns != nil && conversations != nil {
		inbound := ingress.NewInboundService(conversations, opts.IngressResolver, chatTurns)
		// Contract 0077. Without this the registry records nothing and resolves
		// nobody: every sender stays the ingress daemon's principal, the unbound
		// worklist is permanently empty, and blocking a sender has no effect on
		// the path that would carry them.
		inbound.SetIdentityResolver(opts.IdentityResolver)
		inbound.SetPrincipals(idStores.registry)
		// A plugin may shape what happens around an admitted turn. Resolved at
		// CALL time, so whether the plugin built before or after this line cannot
		// silently leave chat unrouted.
		inbound.SetRouter(deferredTurnRouter{holder: &turnRouters})
		// ADR-0127 CL-2: the beneficiary's reply to a pending consent prompt
		// is consumed as the ANSWER before the turn runs. Guarded so a nil
		// *ConsentBridge never becomes a non-nil interface — without the
		// bridge the hook stays nil and chat is byte-identical to today.
		if consentBridge != nil {
			inbound.SetConsentGate(consentBridge)
		}
		// FIVE-PLANES-BUILD Wave-3 C1 (seam S7): chat was the ONE entry point
		// whose traffic never reached the archive. Wired to the same ingestor
		// every other lane writes through, so a captured message obeys ADR-0105's
		// ordering contract rather than a second, chat-shaped copy of it.
		//
		// evIngestor is nil when evidence capture is disabled; then NO capture
		// is constructed and the inbound service runs without one (chat turns
		// still work, they are just not archived).
		if evIngestor != nil {
			chatCapture := ingress.NewChatCapture(evIngestor.Ingest, eventStore)
			// The identity plane, so an author and a thread are entities the
			// `entity` op can name and not just strings inside a role edge.
			chatCapture.SetEntityStore(entityStore)
			inbound.SetCapture(chatCapture)
		} else {
			slog.Warn("five-planes C1: evidence capture is disabled, so chat messages will not be archived; the turn lane is unaffected")
		}
		cambrianServer.IngressInbound = inbound
	}
	// Which ENTRY POINT a conversation arrived on. Wired independently of the
	// inbound service because it is read on the way OUT — when an agent working a
	// chat turn calls back into the kernel — and a chat turn carries no task
	// session, so without it the turn is authorised as an ordinary agent call and
	// the ingress's own policy never applies.
	if conversations != nil && opts.IngressResolver != nil {
		cambrianServer.ConvSurfaces = ingress.NewConversationSurfaces(conversations, opts.IngressResolver)
	}

	// OBSERVABILITYREQ REQ1 / ADR-0057: AgentCallLogger is an injected hook (Options).
	// nil in OSS (no call logging); the premium binary injects a Langfuse logger.
	// GenerateViaModelStream nil-checks the field before use.
	cambrianServer.AgentCallLogger = opts.AgentCallLogger

	// ADR-0042: generations are Acquired from the
	// Provider, which already applies the Langfuse trace wrapper — so GenWrapper is
	// left nil to avoid double-tracing. (wrapGen is identity when GenWrapper is nil.)

	// REQ-SDK-007c: wire artifact storage behind the decision point.
	cambrianServer.ArtifactBytes = artifactVault
	cambrianServer.ArtifactMeta = reg
	cambrianServer.Authz = authorizer

	// ADR-0035 C2: memory.remember() write-back through the write chokepoint, with
	// the classification derived by the decision point + stamped provenance.
	// RememberService was REMOVED 2026-07-31. It was the raw-store-write path for
	// ingest, and nothing routed to it: IngestMemory and the operator ingestor both
	// go through the chunker, which is the only way memory is written. Its two
	// unique behaviours moved to the paths that actually run — MemoryWrittenEvent to
	// IngestionManager, and the unknown-principal fail-closed check to the ingest
	// handler.

	// ADR-0060 D8/D9: route the gRPC IngestMemory through the
	// chunking pipeline. The IngestionManager (constructed by
	// NewMemoryStack alongside the DirectoryWatcher, ADR-0028)
	// chunks the body, mints a source-doc entity, and ingests each
	// chunk with chunk_relations.parent_entity_id set. The
	// SourceType/extension drives the chunker registry's
	// Resolve(sourceType, ext) lookup; documents with no extension
	// fall back to OptionC.
	//
	// There is NO legacy fallback any more: the gRPC handler and the operator
	// ingestor both FAIL when this is absent rather than writing to the store
	// directly. A raw write produced an un-chunked row with no source-document
	// entity and different metadata keys, so "how is memory written" had two
	// answers selected by a queue-size setting.
	if mem.IngestionManager != nil {
		cambrianServer.IngestionProcessor = mem.IngestionManager
		// ADR-0104 D3: bind the plugin ingestor HERE, where the processor actually
		// becomes non-nil — not merely after the server is constructed. Binding at
		// construction captured a nil and every drift message logged
		// "ingestion pipeline not configured" while detection carried on, which is
		// precisely the half-working state the warning exists to make visible.
		memIngestor.processor = mem.IngestionManager
		// ADR-0047 D3: the operator feed. This publisher lived only on
		// RememberService, which sat on the raw-write fallback — a path that could
		// not fire — so MemoryWrittenEvent had a wired consumer and no reachable
		// producer, and the operator memory feed was silent.
		mem.IngestionManager.SetEventBus(eventBus)
		// ADR-0053: also feed the document-ingest path's chunks to the triplet/
		// anchor extractor, so uploaded documents populate chunk_triplets (KG2RAG,
		// query-entity seeding, anchor promotion). Without this the batcher only
		// sees the RememberService path, and uploaded docs get zero triplets.
		if mem.ChunkTripletsBatcher != nil {
			mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
		}
		// T-20261102-async-kg-triplet-extraction-30-chunks-s-lags: say whether sync
		// ingests wait for their KG triplets, so a benchmark arm's boot-log check
		// can confirm the flag took. The setting itself reached the IngestionManager
		// through ingestionConfigFrom; this line only reports it, and warns when it
		// cannot take effect because no triplets batcher exists.
		if ing := cfg.Execution.Ingestion; ing.SyncIngestWaitForKG {
			if mem.ChunkTripletsBatcher != nil {
				slog.Info("SYNC_INGEST_WAIT_FOR_KG: ENABLED (a sync ingest returns after its chunks' KG triplets are persisted)",
					"timeout_ms", ing.SyncIngestKGWaitTimeoutMs, "kg_extractor", ing.KgExtractorEnabled)
			} else {
				slog.Warn("SYNC_INGEST_WAIT_FOR_KG: set but has NO EFFECT: no chunk-triplets batcher (the vector store is not a ChunkTripletsStore)")
			}
		}
		// ADR-0136 D4: the same report for the fact layer's read-your-writes flag, the
		// expect_log anchor of a fact-ingest arm (adr0136-gate G1).
		if ing := cfg.Execution.Ingestion; ing.SyncIngestWaitForFacts {
			if mem.FactExtractor != nil {
				slog.Info("SYNC_INGEST_WAIT_FOR_FACTS: ENABLED (a sync ingest returns after its document's facts are stored)",
					"timeout_ms", ing.SyncIngestFactsWaitTimeoutMs)
			} else {
				slog.Warn("SYNC_INGEST_WAIT_FOR_FACTS: set but has NO EFFECT: no fact extractor (execution.facts.extract_enabled is off, or no knowledge substrate was composed)")
			}
		}
		// ADR-0093: the source-document entity is recorded on every ingest through the
		// ENFORCING document store, which NewMemoryStack now composes and hands to the
		// IngestionManager itself (MemoryStack.DocumentStore; AGENTS.md §2.1/§2.3 —
		// T-20261113-memory-tier-stores-are-composed-outside). It used to be attached
		// here by a post-construction SetDocumentStore over the same vec and authorizer.
		// Deliberately NOT behind the structure-graph flag below: `documents.tags` is the
		// authoritative classification every access decision reads.
		// ADR-0060: structure-aware ingestion — parse each document's real hierarchy
		// via the docling_agent and persist a structure graph (sections + PART_OF/NEXT
		// edges), stamping every chunk with its inherited section path. Opt-in.
		if cfg.Execution.Retrieval.StructureGraphEnabled {
			mem.IngestionManager.SetStructureGraph(
				&subnetwork.DoclingDispatcher{Caller: meta.AgentTransport},
				vec.StructureGraphStore(),
			)
			mem.QueryService.EnableSectionScopedRetrieval(vec)
			slog.Info("ADR-0060: structure-aware ingestion ENABLED (docling parse -> structure graph)")
		}
		// ADR-0105: the knowledge substrate's evidence foundation. Opt-in. Every
		// ingested document's original bytes become durable, content-addressed
		// evidence (+ outbox work item) BEFORE chunking or any other semantic step.
		if evIngestor != nil {
			mem.IngestionManager.SetEvidenceCapture(evIngestor)
			slog.Info("ADR-0105: evidence capture ENABLED (content-first evidence + outbox on every ingest)")
		}
	}

	// ADR-0108 D3: the transformation stage. Starts ONLY when evidence is being
	// captured AND at least one transformer registered — a consumer with no
	// consumers, or one whose archive is never written, is the unwired trap.
	if evIngestor != nil && len(opts.EvidenceTransformers) > 0 {
		consumer, cerr := substrate.Consumer(storeHandle.ContentStore, opts.EvidenceTransformers)
		if cerr != nil {
			return nil, fmt.Errorf("evidence consumer (ADR-0108): %w", cerr)
		}
		go consumer.Run(ctx)
	}

	// ADR-0041: expose the kernel embedder for the agent Local Recurrent Workspace
	// relevance ranking (the Embed RPC). Read-only; no authorization impact.
	cambrianServer.Embedder = embedder
	// Contract 0075: the hourly token series behind the spend sparkline. ONE
	// accumulator shared by every execution — the event writer is resolved per
	// Execute, so a per-call accumulator would reset the series on every plan.
	tokenSeries := telemetry.NewTokenSeries()
	cambrianServer.TokenRecorder = tokenSeries

	// ADR-0039: kernel-owned tool registry. Tools are auto-discovered from the
	// tools/ dir (no Go registration); the executor authorizes every call
	// (grant + resource policy + scope + approval) and runs it in a confined
	// Python child. Default: no grants ⇒ no system tools (fail-closed).
	toolReg := domain.NewInMemoryToolRegistry()
	toolFiles, terr := tooldiscovery.LoadRegistry("tools", toolReg, cfg.Execution.Tools.ToolEffectsStrict)
	if terr != nil {
		slog.Warn("tool discovery failed; system tools disabled", "err", terr)
	}

	// ADR-0126 D3: the INBOUND MCP endpoint, started here — immediately before
	// ConnectAll, and that order is the decision, not an accident. ConnectAll is
	// synchronous (it dials the servers concurrently since 2026-11-02, so it lasts
	// as long as the SLOWEST server, not their sum; it was 25-45 s sequential) while
	// the kernel looks up; an external agent
	// running `claude mcp add` against a booting kernel must find a live endpoint
	// rather than a timeout. It is also after buildPlugins, so every published
	// handler is constructed before the first request can reach one.
	// The four read-only core tools (ADR-0126 D5) join the plugin-published
	// surface here. Their backends bind NOW — the memory stack and the document
	// reader are already built well above — so the endpoint serves real answers
	// from its first request; the CoreBackends indirection stays because
	// mcpserve cannot import the composition root, and because a deployment
	// missing a piece (no memory pipeline) must degrade per tool, not refuse to
	// serve. Typed-nil discipline: an interface field is assigned only when the
	// concrete value is non-nil, so a nil *QueryService can never masquerade as
	// a live searcher.
	mcpCoreBackends := mcpserve.NewCoreBackends()
	{
		var searchLane mcpserve.MemorySearcher
		var answerLane mcpserve.MemoryAnswerer
		if qs := mem.QueryService; qs != nil {
			searchLane = qs
			// ADR-0081: the answer lane is only meaningful when agentic retrieval
			// is wired — the same gate the operator plane uses. Without it,
			// ask_memory degrades to an extractive answer rather than failing.
			if cfg.Execution.Retrieval.AgenticRetrievalEnabled {
				answerLane = qs
			}
		}
		var docLister memory.DocumentLister
		if vec != nil {
			docLister = vec
		}
		mcpCoreBackends.Bind(searchLane, answerLane, docLister, docReader)
	}
	// ADR-0127 CL-1 (kernel half): the worker hub — the held-poll transport
	// core and the live FleetSource the tool executor resolves contributed
	// menus through. Constructed before the endpoint because poll_step /
	// report_step (machine-only) publish over it; the fleet is EMPTY until a
	// worker's broker polls, so no menu advertises a contributed tool that
	// cannot be served (REC-02). The broker itself is a separate premium
	// binary (owner ruling 2026-08-20) and is not here.
	workerHub := domain.NewWorkerHub()
	// CL-2: the lane-specific relay deadline (mcp.worker_call_timeout_ms);
	// unset falls back to mcp.call_timeout_ms — the CL-1 behaviour preserved.
	workerRelayTimeoutMs := cfg.MCP.WorkerCallTimeoutMs
	if workerRelayTimeoutMs <= 0 {
		workerRelayTimeoutMs = cfg.MCP.CallTimeoutMs
	}
	workerHub.CallTimeout = time.Duration(workerRelayTimeoutMs) * time.Millisecond
	slog.Info("ADR-0127: contribution lane armed (CL-2: consent enforcement + selection ladder + parking; fleet empty until workers poll)",
		"relay_timeout_ms", workerRelayTimeoutMs,
		"consent_timeout_ms", cfg.MCP.WorkerConsentTimeoutMs,
		"park_deadline_ms", cfg.MCP.WorkerParkDeadlineMs)

	// ADR-0126 D10 phase 4: the task lane — submit_task / get_task_status. The
	// dispatch closure is byte-for-byte the operator lane's shape (SendFn /
	// planSubmitter): the pre-opened session rides x-session-id into the ONE
	// Execute entrypoint, so there is no second task engine behind the endpoint.
	// The lane's in-memory index is also the production seeder of the task
	// beneficiary (cambrianServer.TaskOwners): lease → task session → owner
	// principal → the owner's contributed fleet in attending agents' menus.
	mcpTaskLane := newMCPTaskLane(sessionMgr, authorizer,
		func(ctx context.Context, sessionID, task string) (string, map[string]string, error) {
			mdCtx := metadata.NewIncomingContext(ctx,
				metadata.Pairs("x-session-id", sessionID))
			resp, execErr := cambrianServer.Execute(mdCtx, &pb.Handoff{Payload: &pb.Object{Data: []byte(task)}})
			if execErr != nil {
				return "", nil, execErr
			}
			return string(resp.GetPayload().GetData()), resp.GetMetadata(), nil
		})
	cambrianServer.TaskOwners = mcpTaskLane
	// ADR-0139 D14: principal memory notes — one service for both planes. The run
	// principal is resolved from kernel-held facts on the lease (conversation owner,
	// then task beneficiary, then execution.memory_notes.default_principal).
	cambrianServer.MemoryNotes = buildMemoryNotes(ctx, cfg.Execution.MemoryNotes, vec.Pool(), authorizer)
	cambrianServer.MemoryNotesPrincipal = memoryNotesPrincipal(cfg.Execution.MemoryNotes.DefaultPrincipal,
		conversations, sessionMgr, mcpTaskLane)
	// ADR-0140 D4: the person a tool call is FOR — the SAME lease → principal
	// resolution as the notes (one derivation, not two), so a per_user
	// integration acts with that person's own credential.
	cambrianServer.RunPrincipal = cambrianServer.MemoryNotesPrincipal
	// The display name of that person, shown to each plan step with the principal
	// id it acts for (T-20261107-delegated-steps-name-the-conversation-owner-in).
	if idStores.registry != nil {
		cambrianServer.PrincipalNames = idStores.registry
	}
	cambrianServer.Calendar = calendar
	// The conversation owner ALONE (no beneficiary/default fallback): its read
	// predicate narrows a chat-lane agent's reads and calls
	// (T-20261106-conversations-cannot-be-opened-as-a-given).
	cambrianServer.ConversationOwner = conversationOwnerOf(conversations, sessionMgr)
	// ENG-614: the conversation profile rides the same lease lookup; the decision
	// point (authz.ProfileNarrowingAuthorizer, above) narrows customer conversations.
	cambrianServer.ConversationProfile = conversationProfileOf(conversations, sessionMgr)
	cambrianServer.PrivateByDefault = cfg.Privacy.PrivateByDefaultOn() // ADR-0142 D12

	mcpEndpoint, err := startMCPEndpoint(cfg,
		append(append(append(mcpserve.CoreTools(mcpCoreBackends), mcpserve.TaskTools(mcpTaskLane, mcpTaskOutput(cambrianServer.StepOutput, cfg.Execution.StepOutput))...),
			domain.ContributionLaneTools(workerHub)...), composed.publishedTools...),
		authorizer, opts.IdentityResolver, idStores.registry)
	if err != nil {
		return nil, err
	}

	// ADR-0043: connect configured external MCP servers, discover their tools into
	// the registry as mcp:<server>/<tool>, and expose the MCP handler. Opt-in —
	// no servers configured ⇒ mcpHandler stays nil and nothing changes. A server
	// that fails to connect is skipped (graceful degradation, D8).
	// Contract 0097: the whole MCP substrate is constructed UNCONDITIONALLY —
	// connector, handler, budget, auditor — because a kernel booted with zero
	// servers must be able to hot-attach its first one from the console, and
	// none of these cost anything while idle.
	//
	// ADR-0141 / T-20261113-mcp-credential-source-and-secret-resolver-are: the
	// connector's credential sources (the ADR-0101 store, the premium account
	// provider for per-user and org credentials) are constructor arguments, so
	// they are built FIRST — the provider factory runs here, before the boot
	// ConnectAll below, which used to run before either was installed. The
	// provider reaches the connector and the runtime lazily (they are built
	// below). A provider failure disables per-user accounts loudly rather than
	// failing the boot: per_user tools then fail closed with "not connected",
	// never with another credential.
	var (
		mcpConnector *mcp.Connector
		mcpRT        *mcpRuntime
	)
	credSource, credErr := buildCredentialSource(opts.NewCredentialSource, cfg, cfgStore,
		func() *mcpRuntime { return mcpRT }, func() *mcp.Connector { return mcpConnector }, credSeams)
	if credErr != nil {
		slog.Error("ADR-0141: per-user accounts NOT enabled", "err", credErr)
	} else if credSource != nil {
		slog.Info("ADR-0141: outbound credential provider installed")
	}
	mcpConnector = mcp.NewConnector(mcpCredentialSources(cfg, cfgStore, credSource), mcpConnectorOptions(cfgStore)...)
	// Integrations: every health change of every server goes onto the operator
	// feed (OperatorEvent.integration_health). Publish is non-blocking.
	mcpConnector.SetHealthObserver(func(h mcp.ServerHealth) {
		_ = eventBus.Publish(domain.MCPServerHealthEvent{
			ServerID: h.ServerID, State: h.State, LastHandshake: h.LastHandshake,
			ToolCount: h.ToolCount, LastError: h.LastError, Restarts: h.Restarts, ChangedAt: h.ChangedAt,
		})
	})
	mcpPricingMap := domain.MapPricingSource{}
	// ADR-0139 D8: one trust-tier containment policy (mcp.sandbox + the
	// integration catalog's tiers), resolved onto every server built below.
	sbPolicy := sandboxPolicyFromConfig(cfg.MCP.Sandbox, loadSandboxCatalog(cfg.MCP.CatalogFile))
	servers := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers)+len(composed.mcpServers))
	// ADR-0075: plugin-contributed MCP servers connect alongside the config ones.
	for _, s := range composed.mcpServers {
		toolPolicy := make(map[string]mcp.ToolPolicy, len(s.Tools))
		for _, tc := range s.Tools {
			toolPolicy[tc.Name] = mcp.ToolPolicy{Dangerous: tc.Dangerous, DataWriteKinds: tc.DataWriteKinds}
		}
		servers = append(servers, mcp.ServerConfig{
			ID: s.ID, Transport: s.Transport, Endpoint: s.Endpoint, Args: s.Args,
			AuthType: s.AuthType, AuthHeader: s.AuthHeader, AuthTokenEnv: s.AuthTokenEnv,
			Tools:               toolPolicy,
			AllowPrivateNetwork: s.AllowPrivateNetwork,
			// A plugin's server ships with the plugin: first-party.
			Sandbox: sbPolicy.Resolve(s.ID, "", "", true, sandbox.ServerSpec{}),
		})
		slog.Info("ADR-0075: registering plugin MCP server", "id", s.ID, "transport", s.Transport)
	}
	for _, s := range cfg.MCP.Servers {
		// ADR-0085 D2: a domain-bound integration tags every tool it advertises, so a
		// policy has something to act on. Without this, dynamically discovered tools
		// arrive untagged and no predicate can restrict them.
		if len(s.ClassificationTags) > 0 {
			slog.Info("ADR-0085: MCP server tools classified",
				"server", s.ID, "tags", s.ClassificationTags)
		}
		for _, tc := range s.Tools {
			if tc.Pricing.Kind != "" {
				mcpPricingMap["mcp:"+s.ID+"/"+tc.Name] = domain.ToolPricing{
					Kind:            domain.PricingKind(tc.Pricing.Kind),
					UnitCost:        tc.Pricing.UnitCost,
					MaxUnitsPerCall: tc.Pricing.MaxUnitsPerCall,
					ChargeOnFailure: domain.FailureCharge(tc.Pricing.ChargeOnFailure),
				}
			}
		}
		servers = append(servers, mcpServerFromConfig(s, sbPolicy))
	}
	// The runtime list holds EVERY configured server, enabled or not, so the
	// console can list (and re-enable) a disabled one; only enabled servers are
	// dialled and watched (T-20261105-mcp-servers-have-no-enabled-flag-every).
	mcpRT = &mcpRuntime{servers: append([]mcp.ServerConfig(nil), servers[:len(composed.mcpServers)]...), sandbox: sbPolicy}
	for _, s := range composed.mcpServers {
		mcpRT.setSource(s.ID, mcpSourcePlugin)
	}
	enabledMCP := make([]mcp.ServerConfig, 0, len(servers))
	enabledMCP = append(enabledMCP, servers[:len(composed.mcpServers)]...)
	for _, s := range cfg.MCP.Servers {
		mcpRT.upsertConfig(s, mcpSourceFile) // store-vs-file is derived from durable state at read time
		if s.IsEnabled() {
			enabledMCP = append(enabledMCP, mcpServerFromConfig(s, sbPolicy))
		} else {
			slog.Info("MCP server disabled; not started", "server", s.ID)
		}
	}
	mcpServers := enabledMCP
	for _, t := range mcpConnector.ConnectAll(ctx, enabledMCP) {
		toolReg.Register(t)
	}
	mcpHandler := domain.ToolHandler(&mcp.Handler{
		Sessions:    mcpConnector,
		CallTimeout: time.Duration(cfg.MCP.CallTimeoutMs) * time.Millisecond,
	})
	mcpPricing := domain.ToolPricingSource(mcpPricingMap)
	mcpBudget := domain.NewBudgetLedger()
	mcpBudget.DefaultCap = cfg.MCP.DefaultSessionBudget // 0 ⇒ tracked but unbounded
	mcpAuditor := domain.EgressAuditor(mcp.SlogEgressAuditor{})

	// ENG-1116: operator grants survive a restart (Postgres 0052 tool_grants;
	// in memory only without a database); profile allowlist grants are an
	// in-memory layer re-derived below on every boot.
	toolGrants, err := buildToolGrants(ctx, vec.Pool())
	if err != nil {
		return nil, err
	}
	// toolApproval is declared above the Watcher (it also gates proactive plans).
	// ADR-0039: the dangerous-tool approval controller. By default it is the
	// operator-gated controller (WatchApprovals/SubmitApprovalDecision). When
	// tools_auto_approve is set, dangerous tools run without a human decision —
	// the only sane mode for an unattended local/dev run (the operator RPCs have
	// no client). The process sandbox remains the containment boundary either way.
	var toolApprovalCtrl domain.ApprovalController = toolApproval
	if cfg.Execution.Tools.ToolsAutoApprove {
		toolApprovalCtrl = domain.AlwaysApproveController{}
	}
	// ADR-0049 A2.3: score each step outcome against the merit the ProfileAggregator
	// already maintains. Wired unconditionally — the gate it feeds is what is
	// default-off, not the measurement, so the surprise distribution accumulates on
	// outcome records and can be used to tune the floor from data rather than taste.
	cambrianServer.SurpriseOracle = &supervision.MeritSurpriseOracle{Profiles: mem.ProfileStore}

	// ADR-0154: the one durable action record (nil without migration 0046), shared
	// by the plan-step idempotency cache and the subtask registry (ENG-1428). Built
	// before the executor: the idempotency cache is one of its constructor controls.
	actionRecords := buildActionRecordStore(ctx, vec.Pool())
	// AGENTS.md §2.3 (T-20261113-domain-toolexecutor-has-no-constructor-its): the
	// tool reference monitor's security controls are constructor-injected —
	// Registry, Grants and Authz are required (authorizer is AllowAllAuthorizer{}
	// in OSS, set explicitly above); approval, the profile ceiling, the
	// tools_unrestricted bypass and the ADR-0145 D9/D14 controls are read from
	// config here, in the one place the executor is built.
	//
	// The per-lease controls (ADR-0145 D12 compositions, ADR-0150 subtask
	// registry) are built from config here too and handed to wireComposedWorkers
	// and wireSubtasks below, which bind leases into them.
	execLeases := buildExecutorLeases(cfg.Execution, actionRecords)
	// ENG-917: the outbound identifier guard, read from
	// execution.tools.outbound_identifier_guard (default on) and handed to the
	// constructor like every other control.
	outboundGuard, err := outboundIdentifierGuard(cfg.Execution.Tools.OutboundIdentifierGuard)
	if err != nil {
		return nil, fmt.Errorf("tool executor: %w", err)
	}
	toolControls := execLeases.controls(evidenceToolControls(cfg.Execution, actionRecords, domain.ToolExecutorControls{
		Registry: toolReg,
		Grants:   toolGrants,
		Authz:    authorizer, // ADR-0085: data-store regime + (Phase 2) effect classes
		Approval: toolApprovalCtrl,
		// Agent stack v2: each profile agent's tool allowlist is a hard ceiling,
		// held under tools_unrestricted and over skill-conferred grants.
		Ceiling:      profileToolCeiling(profiles),
		Unrestricted: cfg.Execution.Tools.ToolsUnrestricted, // dev/trusted bypass: all agents, all tools
		// ENG-917: egress_send calls carrying personal identifiers go to approval.
		OutboundIdentifiers: outboundGuard,
	}))
	toolExecutor, err := domain.NewToolExecutor(toolControls, domain.ToolExecutor{
		MCPHandler: mcpHandler, // ADR-0043: nil ⇒ no MCP tools (opt-in)
		// ADR-0127: contributed local:<machine>/<tool> steps resolve through
		// the hub's live fleet and relay through it, D8-fenced. The executor
		// stays the reference monitor; the hub is only transport.
		Fleet:        workerHub,
		LocalHandler: domain.WorkerRelayHandler{Hub: workerHub},
		// CL-2 (ADR-0127 D6/D7): the pre-dispatch consent gate and the
		// selection ladder's ask channel; and the parking window for steps
		// whose target machine is offline (0 ⇒ 24h).
		Consent:      workerConsent,
		ParkDeadline: time.Duration(cfg.MCP.WorkerParkDeadlineMs) * time.Millisecond,
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
		EgressAuditor: mcpAuditor, // ADR-0043: nil ⇒ no egress auditing
		Budget:        mcpBudget,  // ADR-0043: nil ⇒ MCP calls unmetered
		Pricing:       mcpPricing, // ADR-0043: nil ⇒ MCP calls unmetered
		ContentStore:  storeHandle.ContentStore,
		// ADR-0145 / owner decision 2026-11-12: every content size is
		// execution.content (was a hardcoded 65536).
		InlineThreshold: cfg.Execution.Content.Inline(),
		PreviewChars:    cfg.Execution.Content.Preview(),
		PageBytes:       cfg.Execution.Content.Page(),
		Overlay:         domain.NewRunGrantOverlay(), // ADR-0046 D6: skill-conferred run-scoped grants
		// Promote files a confined tool writes into the DURABLE artifact system
		// (vault + metadata, retrievable via GetArtifact, scope-governed) AND
		// materialize them to data/outputs so a requested file lands on disk —
		// instead of living only as a GC-eligible content-store CID.
		ArtifactBytes: artifactVault,
		ArtifactMeta:  reg,
		ArtifactTags: func(ctx context.Context, agentID string) []string {
			// A promoted artifact is a WRITE, so its classification is derived by the
			// decision point exactly like a memory write — never chosen by the tool.
			tags, _ := authorizer.ClassifyWrite(ctx, domain.AgentPrincipal(agentID), nil)
			// ADR-0142 D12: a file a tool wrote in a person's conversation is
			// private to that person, like every other chat-turn write.
			return domain.StampPrivateOwner(ctx, tags)
		},
		ArtifactOutputDir: filepath.Join(cfg.Storage.DataDir, "outputs"),
		// ADR-0049 D1: mutations become action records. (The ADR-0048 D6 read-output
		// promotion and its 200-byte floor were deleted 2026-11-05 with the tier-2 judge.)
		ToolOutput: mem.Agent,
		// ADR-0139 D11: pre_tool after the policy decision (narrow only; a rewrite
		// is re-authorized), post_tool around the output.
		Hooks: hookChain,
		// The conversation's record of side-effecting calls: every approval the
		// executor raises names an earlier call it may duplicate
		// (T-20261107-approver-is-not-told-that-an-identical-side). nil when the
		// window is 0 (disabled).
		History: domain.NewActionHistory(time.Duration(cfg.Execution.Tools.DuplicateNoticeWindowMinutes)*time.Minute, nil),
	})
	if err != nil {
		return nil, fmt.Errorf("tool executor: %w", err)
	}
	cambrianServer.ToolExecutor = toolExecutor
	// ADR-0145 D1: the run ledger (execution.ledger, off by default): every tool
	// result and step output recorded, scope-enforced, read by reference. Threaded
	// to the executor, the server and the turn end here; nil sweeper when off.
	ledgerSweeper := wireLedger(ctx, cfg.Execution.Ledger, cfg.Execution.Content, vec.Pool(), artifactVault, reg, authorizer,
		cfg.Privacy.PrivateByDefaultOn(), cambrianServer, turnDelegations, eventBus)
	// Effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared): an
	// agent's effects are derived from the tools the executor would let it call
	// (grants × profile ceiling × live registry), so routing and enforcement read one
	// source. Wired here because the executor is built after the gatekeeper; until it
	// is set, a step that declares effects is refused, never routed unchecked.
	wireEffectCheck(cfg.Execution.Capability, cambrianServer.ToolExecutor, meta.Gatekeeper, aw.Planner)
	// ADR-0145 phase 2 / D14: checklist, plan validator, request-tied guards,
	// argument validation and idempotent writes (all default off), after the ledger above.
	var plannerGen domain.Generator
	if aw.Planner != nil {
		plannerGen = aw.Planner
	}
	wireEvidencePipeline(cfg.Execution, cambrianServer, plannerGen, actionRecords)
	// ADR-0145 D4: calculation tools (execution.tools.calc_tools, default on since 2026-11-15),
	// after the ledger so they read and record through it.
	wireCalcTools(cfg, cambrianServer, cambrianServer.Calendar)
	// ADR-0137 D9 (ENG-411 stage C): forget_memory, the agent-facing two-phase forget
	// over the same service as the PreviewForget/Forget RPCs. Offered on menus only with
	// execution.erasure.enabled AND agent_erase_enabled (both default off).
	if cambrianServer.ToolExecutor != nil {
		cambrianServer.ToolExecutor.KernelTools = append(cambrianServer.ToolExecutor.KernelTools,
			forget.NewTool(forgetSvc, cfg.Execution.Erasure.AgentEraseEnabled))
	}
	wireStagedInputs(cfg.Execution.Tools.StagedInputs, cambrianServer)
	// The chat front desk is told which work systems the planner's agents can reach,
	// from the planner's own live tool menu (the one wireEffectCheck just set), so a
	// work-system ask is delegated instead of answered "I have no tool for that"
	// (T-20261107-chat-agent-tells-the-user-it-has-no-tool-for).
	if chatTurns != nil && aw.Planner != nil {
		chatTurns.SetWorkSystems(aw.Planner.WorkSystems)
	}
	if chatTurns != nil {
		chatTurns.SetCalendar(calendar)
	}
	// ADR-0145 phase 3, the chat side (execution.chat grounding keys, default off):
	// after the ledger, which holds the turn state and the reply check's evidence.
	wireChatGrounding(cfg.Execution.Chat, chatTurns, cambrianServer, turnDelegations, plannerGen, runTracer)
	// ADR-0150 D3: the merged conversation agent in the single-agent lane.
	wireChatLane(cfg.Execution, chatTurns, cambrianServer)
	cambrianServer.ApprovalHub = toolApproval
	// Principal-owned recurring tasks: approval-gated through this same hub
	// (kind "schedule"), stored by the premium schedule store a plugin registered
	// at Build. nil when either is missing or execution.schedules.disabled.
	cambrianServer.ScheduledTasks = buildScheduledTasks(cfg.Execution.Schedules, principalSchedules.get(), toolApproval)
	toolApproval.SetBus(eventBus) // ADR-0047 0047-19 + ADR-0142 D11: raises AND resolutions reach the feed
	// ADR-0047 0047-17/0047-18: the executor publishes PlanStateChanged to the
	// operator feed, and live executions register into one shared control hub so
	// operator PauseSession/ResumeSession can steer them.
	cambrianServer.EventBus = eventBus
	operatorControlHub := operator.NewExecutionControlHub()
	cambrianServer.ControlHub = operatorControlHub
	// Agent stack v2: profile agents' grants are their allowlists' expansion over the
	// registry (native + MCP tools connected above); the MCP tool sink re-syncs them
	// when a server's tools change (below).
	profileGrants := newProfileGrantSync(toolGrants.Profile(), toolReg.All, profiles)
	profileGrants.Sync(ctx, true)
	if cfg.Execution.Tools.ToolsUnrestricted {
		slog.Warn("ADR-0039: tools_unrestricted=true — ALL agents may call ALL tools (grant system bypassed). Trusted deployments only.")
		if len(profiles) > 0 {
			slog.Info("agent profiles: tools_unrestricted does not lift profile tool allowlists; each profile agent stays inside its own",
				"profiles", len(profiles))
		}
	}
	if cfg.Execution.Tools.ToolsAutoApprove {
		slog.Warn("ADR-0039: tools_auto_approve=true — dangerous tools run WITHOUT operator approval. Trusted deployments only.")
	}

	// ADR-0044: index all discovered tools (native + MCP) for semantic retrieval,
	// then wire the retriever so ListTools(query) serves a task-relevant menu
	// instead of the whole registry. Reuses the kernel embedder + pgvector store;
	// the ToolExecutor depends only on the ToolRetriever port. Indexing is
	// best-effort — a failure degrades retrieval to the full menu, never blocks boot.
	//
	// AGENTS.md §2.2: tier 0, a kernel-authored artifact writer and reader. Tool
	// index docs are written by the kernel from the tool registry (native + MCP
	// manifests) at boot and carry no principal scope (ADR-0044 D3); ranking is
	// restricted to the caller's GRANTED tools by the in-query ID filter
	// (VectorToolRetriever.Rank), so no read-scope predicate applies to this index.
	// The same store is used by VectorToolRetriever just below; reconcileIndex lists
	// ids through the concrete adapter (vec).
	toolIndexStore := mem.KernelArtifactStore("ADR-0044 D3")
	toolIndexer := &domain.ToolIndexer{Store: toolIndexStore, Embedder: embedder}
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
			// A disabled server's tools are gone on purpose, not an outage:
			// prune their index docs like a removed server's.
			if s.IsEnabled() {
				configuredMCP[s.ID] = true
			}
		}
		reconcileIndex(ctx, vec, toolIndexer, domain.DocTypeTool, toolKeepFunc(currentTools, configuredMCP))
	}
	// Tier 0: see the AGENTS.md §2.2 note on toolIndexer above (ADR-0044 D3).
	cambrianServer.ToolExecutor.Retriever = domain.VectorToolRetriever{
		Store: toolIndexStore, Embedder: embedder, Floor: cfg.Execution.Tools.ToolRetrievalFloor,
	}

	// AGENTIC_RETRIEVAL_SPEC Phase 2a: wire the LLM query-planner (retrieval_agent)
	// as the QueryService's Planner — invoked directly via the agent transport (no
	// selection) with a managed LLM session, the same privileged-organ + session
	// privileged-organ pattern. Default off; fail-open to the single pass when the
	// agent is unreachable or the config flag is unset.
	if cfg.Execution.Retrieval.AgenticRetrievalEnabled {
		mem.QueryService.EnableAgenticRetrieval(&subnetwork.RetrievalDispatcher{
			Caller:  meta.AgentTransport,
			AgentID: "retrieval_agent",
			Gateway: llmGateway,
			// An explicit generator id overrides; "" (default) ⇒ the retrieval_planner
			// role, falling back to memory, read per call (ENG-329). A FAST model
			// matters on the hot path.
			Model:     cfg.Execution.Retrieval.AgenticPlannerModel,
			RoleModel: retrievalPlannerRole(llmProvider),
		}, cfg.Execution.Retrieval.AgenticMaxHops)
		mem.QueryService.SetDecomposeEnabled(cfg.Execution.Retrieval.AgenticDecomposeEnabled)
		mem.QueryService.SetSubQuestionConcurrency(cfg.Execution.Retrieval.AgenticSubQuestionConcurrency)
		mem.QueryService.SetAdaptiveRoutesEnabled(cfg.Execution.Retrieval.AgenticAdaptiveRoutesEnabled) // ADR-0135
		warnUndeclaredPlannerModel(cfg.Execution.Retrieval.AgenticPlannerModel, cfg.LLMProvider.Generators)
		slog.Info("AGENTIC_RETRIEVAL_SPEC: agentic retrieval query-planner ENABLED (retrieval_agent)", "decompose", cfg.Execution.Retrieval.AgenticDecomposeEnabled, "subquestion_concurrency", cfg.Execution.Retrieval.AgenticSubQuestionConcurrency, "adaptive_routes", cfg.Execution.Retrieval.AgenticAdaptiveRoutesEnabled)
	}

	// ADR-0053 D2 (revised): route write-time chunk-triplet extraction through the
	// deterministic, NO-LLM kg_extractor system agent (metadata + spacy_patterns),
	// invoked DIRECTLY via the agent transport (no selection) — the same privileged-organ
	// privileged-organ pattern. Off (default) ⇒ the batcher keeps its LLM extractor.
	// Injected before mem.Start so the swap is in place when the drain begins.
	if cfg.Execution.Ingestion.KgExtractorEnabled && mem.ChunkTripletsBatcher != nil {
		ing := cfg.Execution.Ingestion
		mem.ChunkTripletsBatcher.UseExtractor(kgExtractorDispatcherFrom(ing, meta.AgentTransport))
		slog.Info("ADR-0053 D2: kg_extractor ENABLED (deterministic metadata + spacy_patterns organ, no LLM)",
			"call_attempts", ing.KgExtractorCallAttempts, "retry_backoff_ms", ing.KgExtractorRetryBackoffMs,
			"max_requeues", ing.KgTripletsMaxRequeues)
	}

	// ADR-0046: discover authored system skills from skills/<name>/SKILL.md and
	// index them as DocTypeSkill for semantic retrieval (the analog of tool
	// discovery + indexing above). Agent skills are SDK-local and never indexed
	// here. Best-effort — a failure leaves the system-skill menu empty, never
	// blocks boot. The SkillRetriever and ListSkills are wired just below.
	//
	// "skills" is resolved against the process working directory, like "tools"
	// above: running from cambrian-core finds cambrian-core/skills (the shipped
	// system skills, ADR-0139 D13). skills/README.md says how other deployments
	// point at it. A missing directory means zero system skills, not an error.
	skillReg := domain.NewInMemorySkillRegistry()
	discoveredSkills, skerr := skilldiscovery.LoadRegistry("skills", skillReg)
	if skerr != nil {
		slog.Warn("ADR-0046: skill discovery failed; system skills disabled", "err", skerr)
	}
	// AGENTS.md §2.2: tier 0, a kernel-authored artifact writer. The skill index
	// docs are written by the kernel from operator-authored SKILL.md files at boot,
	// serve no principal, and carry the skill's scope tags as their classification
	// (ADR-0046 D2/D4/D9, the DocTypeSkill sibling of the ADR-0044 tool index, which
	// toolIndexer above writes the same way). Reads of this index go through the
	// fail-closed EnforcingVectorStore below (VectorSkillRetriever), so scope is
	// enforced where principals are served.
	skillIndexer := &domain.SkillIndexer{Store: mem.KernelArtifactStore("ADR-0046 D2"), Embedder: embedder}
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
	// retriever reads through a fail-closed authz.EnforcingVectorStore (ADR-0085) so an agent only
	// retrieves skills its effective scope permits (the same read path as memory).
	cambrianServer.SkillRegistry = skillReg
	cambrianServer.SkillRetriever = domain.VectorSkillRetriever{
		Store:    mem.Reader(), // the stack's tier-1 read chokepoint (AGENTS.md §2.1)
		Embedder: embedder,
		Floor:    cfg.Execution.Tools.ToolRetrievalFloor,
	}
	// ADR-0139 D13: the skill lifecycle overlays the catalog just built — shipped
	// skills get lifecycle records, approved agent skills are served from the
	// Postgres store into this same registry and index (app/skill_lifecycle_wiring.go).
	//
	// Per-agent skill access (decisions/skill-access-grants.md): a profile agent's
	// skills are its profile's list plus its grants; use_skill outside that asks the
	// operator on the shared approval hub (app/skill_access_wiring.go). An approved
	// proposal grants the skill to its proposer.
	skillAccess := wireSkillAccess(ctx, cfg.Execution.Skills, vec.Pool(), profiles, skillReg, toolApproval)
	var proposerGrants skilllifecycle.ProposalGranter
	if skillAccess != nil && skillAccess.HasStore() {
		proposerGrants = skillAccess
	}
	if skillAccess != nil {
		cambrianServer.SkillAccess = skillAccess // typed-nil guard: only a built service
	}
	// ADR-0145 D12: plan steps on workers the kernel composes (execution.workers.composed,
	// default off), after the ledger, the effect vocabulary and skill access are wired.
	wireStepRecovery(cfg.Execution, cambrianServer, meta.Dispatcher)
	wireComposedWorkers(cfg.Execution, cambrianServer, meta.Dispatcher, reg, execLeases.composed)
	// ADR-0150 D2: run_subtasks for the single-agent lane's agent (after the ledger
	// and the composed runtime it runs subtask workers on).
	wireSubtasks(cfg.Execution, cambrianServer, execLeases.subtasks)
	skillLifecycle, skillCurator := wireSkillLifecycle(ctx, cfg.Execution.Skills, vec.Pool(), skillReg, skillIndexer, discoveredSkills, proposerGrants)
	if skillLifecycle != nil {
		cambrianServer.SkillLifecycle = skillLifecycle // typed-nil guard: only a built service
	}

	// ADR-0043 D8 / ADR-0044 re-sync: the sink keeps the registry + the retrieval
	// index in step as servers drop and reconnect; the connector's Watch loop
	// (started as a background service) drives it. Seeded with the boot-time MCP
	// tools so a later drop knows exactly what to remove.
	var mcpSink mcp.ToolSink
	if mcpConnector != nil {
		sink := newMCPToolSink(toolReg, toolIndexer)
		sink.Seed(toolReg.All())
		sink.onChange = func(ctx context.Context) { profileGrants.Sync(ctx, false) }
		mcpSink = sink
	}

	// ADR-0047 0047-16/0047-25: real CommandEffects bound to kernel surfaces.
	// TagMemory follows Amendment A1.2: operators may widen AND narrow tags, from
	// the controlled vocabulary, written through the kernel store (not a raw DB
	// write), audited by the command path.
	// The controlled vocabulary lives with the decision point. With no policy
	// plugin installed there is no vocabulary to violate, so any tag is accepted —
	// consistent with the rest of the OSS posture.
	operatorEffects := operator.CommandEffectsFuncs{
		TagMemoryFn: func(ctx context.Context, docID, tag string, add bool) error {
			// ADR-0146 D5: owner tags are kernel-stamped. Adding one would forge
			// "written for <id>" (which the own-rows rule trusts); removing one would
			// lift a person's owner fence. RetagDocument already preserves them on the
			// document path; the memory-row path below would not, so refuse here.
			if domain.IsOwnerTag(tag) {
				return fmt.Errorf("tag_memory: %q is an owner tag; owner tags are stamped by the kernel and cannot be added or removed", tag)
			}
			if opts.PolicyAdmin != nil && !opts.PolicyAdmin.ValidateTag(tag) {
				return fmt.Errorf("tag %q is not in the controlled vocabulary", tag)
			}
			// ADR-0093: try the DOCUMENT ENTITY first. `documents.tags` is the
			// authoritative classification, and the per-chunk copies are a derived
			// cache — so tagging a document has to go through RetagDocument, which
			// moves the row and every one of its chunks in one transaction.
			//
			// Writing the chunk directly (which is all this used to do) left the
			// document row saying one thing and its chunks another: precisely the
			// half-classified state the split was built to make impossible. It also
			// could not tag a document at ALL, because GetByID searches the chunk and
			// descriptor tables and `documents` is deliberately not among them.
			if current, found, derr := vec.DocumentTags(ctx, docID); derr != nil {
				return fmt.Errorf("tag_memory: %w", derr)
			} else if found {
				return docLifecycle.RetagDocument(ctx, docID, applyTag(current, tag, add))
			}

			// Retrieval returns CHUNKS, so an operator labelling a search result holds a
			// chunk id. Label its parent document instead: tagging the chunk alone would
			// leave the document's other chunks unlabelled, which is the half-classified
			// state this design exists to prevent — produced by the feature meant to fix
			// classification.
			if parent, ok, perr := vec.ParentDocumentOf(ctx, docID); perr != nil {
				return fmt.Errorf("tag_memory: %w", perr)
			} else if ok {
				current, _, terr := vec.DocumentTags(ctx, parent)
				if terr != nil {
					return fmt.Errorf("tag_memory: %w", terr)
				}
				return docLifecycle.RetagDocument(ctx, parent, applyTag(current, tag, add))
			}

			// Not a document and not part of one — an agent-written memory or a tool
			// descriptor. Those have no parent to be authoritative, so the row itself
			// is it.
			// Those have no parent to be authoritative, so the row itself is it.
			doc, err := vec.GetByID(ctx, docID)
			if err != nil || doc == nil {
				return fmt.Errorf("tag_memory: %s is neither a document nor a memory: %w", docID, err)
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
			if opts.PolicyAdmin == nil {
				return fmt.Errorf("set_scope requires the access-policy plugin; this build has none")
			}
			return opts.PolicyAdmin.SetAgentScope(ctx, agentID, required, anyOf, forbidden)
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
			// A server registered at runtime by a tool call is unknown code:
			// the default tier (third-party unless configured otherwise).
			cfg := mcp.ServerConfig{ID: name, Transport: "stdio", Endpoint: command,
				Sandbox: sbPolicy.Resolve(name, "", "", false, sandbox.ServerSpec{})}
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

	authenticator, err := buildAuthenticator(ctx, cfg, idStores, opts.NewAuthenticator)
	if err != nil {
		return nil, err
	}

	// ADR-0142 D12: the explicit publish. The raw adapter (tier 0) for the retag —
	// a kernel-authored write after the owner, policy and approval checks in
	// memorySharer.plan and operator.ShareToTeam, the TagMemory lane.
	sharer := &memorySharer{docs: retagThroughLifecycle{vec, docLifecycle}, authz: authorizer}
	toolApproval.HandleKind(domain.ApprovalKindPublish, publishHandler(sharer))

	return &Kernel{
		agentSockets:    agentSockets,
		Links:           linkStore,
		linkReview:      queryPlane,
		OperatorEffects: operatorEffects,
		MemorySharer:    sharer,
		OperatorAudit:   operatorAudit,
		Documents:       vec,
		DocReader:       docReader,
		// Contract 0072 (Wave 1). storeHandle is the session/checkpoint store for
		// both backends; the operator wiring type-asserts for the two methods it
		// needs, so a backend lacking them degrades to Unimplemented rather than
		// to an empty list.
		CheckpointSource:    storeHandle,
		TokenSeries:         tokenSeries,
		Progress:            progressSink,
		StepOutputFeed:      stepOutFeed,
		Logs:                util.DefaultLogRing(),
		LLMProvider:         llmProvider,
		VectorCounter:       vectorCounterFor(vec),
		Config:              cfg,
		Registry:            reg,
		WatchDeadLetters:    reg,                      // REACT-01 / ADR-0061
		WatchMetrics:        watchMetricsReader,       // REACT-05 / ADR-0071
		WatchBacktester:     watchBacktester,          // REACT-05 / ADR-0071
		ExtraServices:       opts.ExtraServices,       // ADR-0073
		AgentGRPCServices:   opts.AgentGRPCServices,   // ADR-0118 D3
		OperatorMethodRoles: opts.OperatorMethodRoles, // premium-declared operator method roles
		LoginObserver:       opts.LoginObserver,       // ADR-0130 1f (nil in OSS)
		Lifecycles:          lifecycles,               // ADR-0074 (empty in OSS)
		PluginCapabilities:  composed.capabilities,    // ADR-0082 D2 (empty in OSS)
		Conversations:       conversations,            // ADR-0084 D1 (nil until migrated)
		ChatTurns:           chatTurns,                // ADR-0084 D4 (nil when disabled)
		PrincipalDirectory:  principalDirectoryOrRegistry(principalDirectory.get(), idStores.registry),
		PrincipalSchedules:  principalSchedules.get(),
		StreamRegistry:      streamRegistry,
		Principals:          idStores.registry,
		Authenticator:       authenticator,
		PluginStatuses:      composed.statuses, // ADR-0082 D9 (empty in OSS)
		Health:              healthChecker,     // PLAT-03 / ADR-0065
		Store:               storeHandle,
		Memory:              mem,
		Awareness:           aw,
		Metabolism:          meta,
		Supervision:         sup,
		Server:              cambrianServer,
		Listener:            lis,
		SessionMgr:          sessionMgr,
		CircadianRhythm:     circadianRhythm,
		SkillLifecycle:      skillLifecycle,
		SkillCurator:        skillCurator,
		LedgerSweeper:       ledgerSweeper,
		SkillAccess:         skillAccess,
		ArtifactVault:       artifactVault,
		EventBus:            eventBus,
		Authorizer:          authorizer,
		PolicyAdmin:         opts.PolicyAdmin,
		ToolGrants:          toolGrants,
		MCPConnector:        mcpConnector,
		MCPSink:             mcpSink,
		MCPServers:          mcpServers,
		MCPRuntime:          mcpRT,
		credentialSeams:     credSeams,
		MCPEndpoint:         mcpEndpoint,
		mcpTasks:            mcpTaskLane,
		MCPCoreBackends:     mcpCoreBackends,
		GeneratorRuntime:    newGeneratorRuntime(cfg.LLMProvider),
		LLMGateway:          llmGateway,
	}, nil
}

// truncateRunes returns s bounded to at most n runes, appending an ellipsis marker
// when it had to cut. Rune-safe so a multi-byte prompt is never split mid-character.
// Used for the ADR-0079 exchange lane, where prompts can be many KB.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…[truncated]"
}

// llmExchangeEvent builds the ADR-0079 exchange event: prompt and completion
// truncated to maxChars RUNES, and PromptChars/ResponseChars the untruncated
// lengths in the SAME unit, so "count > maxChars" is exactly "this text was
// truncated". They were byte counts (len), which for non-ASCII text (TR/EN market)
// overstated the length and made untruncated text look truncated
// (T-20261113 ENG-1317).
func llmExchangeEvent(sessionID, agentID, modelID string, stepIndex int, prompt, completion string, maxChars int) domain.AgentLLMExchangeEvent {
	return domain.AgentLLMExchangeEvent{
		SessionID:     sessionID,
		AgentID:       agentID,
		StepIndex:     stepIndex,
		Purpose:       "agent_llm",
		ModelID:       modelID,
		Prompt:        truncateRunes(prompt, maxChars),
		Completion:    truncateRunes(completion, maxChars),
		PromptChars:   utf8.RuneCountInString(prompt),
		ResponseChars: utf8.RuneCountInString(completion),
	}
}

func startKernelServices(g *errgroup.Group, ctx context.Context, k *Kernel) {
	// A. Domain stack background workers
	g.Go(func() error { return k.Memory.Start(ctx) })
	g.Go(func() error { return k.Metabolism.Start(ctx) })
	g.Go(func() error { return k.Supervision.Start(ctx) })
	// Opt-in pprof listener and memory log line (runtime block; off by default).
	startRuntimeDiagnostics(g, ctx, k.Config)

	// ADR-0139 D4: the agent card listener (server.a2a) and remote A2A agent
	// registration (a2a.endpoints); both off by default. app/agentcard_endpoint.go.
	startAgentCards(g, ctx, k)

	// ADR-0139 D13: the deterministic skill curator (execution.skills.curator.enabled;
	// off by default). Non-blocking; drained by Shutdown.
	if k.SkillCurator != nil {
		k.SkillCurator.Start(ctx)
	}

	// ADR-0145 D1: the run ledger's retention sweeper (execution.ledger.enabled).
	if k.LedgerSweeper != nil {
		k.LedgerSweeper.Start(ctx)
	}

	// ADR-0074: start plugin lifecycles (e.g. the reactive engine's worker pools +
	// REACT-06 scheduler). Start is non-blocking (launches goroutines and returns) — the
	// reactive engine replays its REACT-01 journal and arms schedule watches here. Empty
	// in OSS (no plugins).
	for _, lc := range k.Lifecycles {
		if lc.Start != nil {
			lc.Start(ctx)
			slog.Info("ADR-0074: plugin lifecycle started", "name", lc.Name)
		}
	}

	// B. Synaptic Bridge background workers (ADR-0012)
	g.Go(func() error {
		return nil
	})
	g.Go(func() error {
		k.CircadianRhythm.Start(ctx)
		return nil
	})

	// ADR-0043 D8 / ADR-0044: MCP server health + reconnect loop. On a drop it
	// removes the server's tools from the menu + index; on reconnect it re-discovers
	// and re-indexes (re-sync). No-op when no MCP servers are configured.
	// Started even with ZERO configured servers (contract 0097): Watch blocks on
	// ctx and hosts the lifecycle that runtime-saved servers attach their
	// health loops under — gating it on the boot list would leave the first
	// console-added server of an MCP-less kernel unwatched.
	if k.MCPConnector != nil && k.MCPSink != nil {
		g.Go(func() error {
			k.MCPConnector.Watch(ctx, k.MCPServers, k.MCPSink, 30*time.Second)
			return nil
		})
	}

	// B. Async Backfill (Brain Integrity)
	g.Go(func() error {
		slog.Info("🧬 Kernel: Starting Brain Integrity verification (profile backfill)...")
		backfillCfg := backfill.BackfillConfig{TimeoutMs: 60000}
		if err := backfill.RunProfileBackfill(ctx, k.Registry, k.Memory.ProfileStore, k.Metabolism.ProfileSeeder, k.Memory.Embedder, backfillCfg); err != nil {
			return fmt.Errorf("brain integrity failure: %w", err)
		}
		slog.Info("✅ Kernel: Brain Integrity verified.")
		return nil
	})

	// ADR-0047: Operator Transport Plane auth (D13). The interceptors are
	// method-scoped to /cambrian.OperatorConsole/* (plus the Orchestrator
	// approval RPCs, ADR-0039 D10 — authz.IsOperatorGatedAgentServiceMethod).
	// ADR-0142 D1: the bearer is resolved through the kernel Authenticator (local
	// accounts seeded with the env bootstrap operator, plus one optional OIDC
	// issuer, or a plugin's replacement), built in bootstrapKernel.
	//
	// ONE identity for the process: the ingestion server's /v1/admin routes
	// (below) validate the same bearer through the same instance, so a token
	// minted by the gRPC Login is the token the HTTP admin surface accepts.
	operatorIDP := k.Authenticator
	// ADR-0142 migration step 6: worker-machine owners become registry persons.
	// Here, not in bootstrapKernel, because the owner bindings are named secrets
	// and the secret store is attached to the process by Run.
	importMachineOwners(ctx, k.Principals, k.Config.Server.MCP.Clients, resolveNamedSecret)

	// ADR-0130 D1/D2: agent-plane client authentication. Every Orchestrator RPC
	// (and every premium service declared agent-facing) requires the per-agent
	// named secret `agent:<id>:token` as a bearer, with x-agent-id as the
	// assertion it must match — UNLESS server.insecure_localhost=true, which
	// waives it along with the transport refusal it already waives (the
	// benchmark harness runs under that flag). Constructor-injected; nothing
	// attaches it later (AGENTS.md §2.3).
	agentAuth := authz.NewAgentCredentialVerifier(resolveNamedSecret, k.Config.Server.InsecureLocalhost)
	switch {
	case agentAuth.Waived():
		slog.Warn("ADR-0130: agent-plane authentication DISABLED by server.insecure_localhost=true",
			"effect", "x-agent-id is trusted as the principal; any process reaching the port can act as any agent")
	case k.ConfigStore == nil:
		slog.Warn("ADR-0130: agent plane requires per-agent credentials but the config store is off — "+
			"no agent can authenticate", "fix",
			"enable the ADR-0101 store (CAMBRIAN_CONFIG_STORE) so `setup`/boot can mint agent:<id>:token, "+
				"or set server.insecure_localhost=true on a trusted host")
	default:
		slog.Info("ADR-0130: agent plane requires per-agent credentials",
			"secret", "agent:<id>:token", "header", "authorization: Bearer + x-agent-id")
	}

	// Per-principal token bucket on both gRPC planes
	// (T-20261101-rate-limit-the-operator-and-agent-grpc-planes). Built here and
	// passed to the interceptor constructors below — never attached later
	// (AGENTS.md §2.3). Its interceptors run LAST in each chain, after agent and
	// operator authentication, so a bucket is keyed by the principal the
	// credential established; unauthenticated calls are keyed by peer host.
	planeLimiter := ratelimit.New(rateLimitConfig(k.Config.Server.RateLimit), nil)

	// C. gRPC Server
	g.Go(func() error {
		// SEC-03: recomputed from the same cfg rather than threaded through the
		// signature. transportCredentials is pure, so the two calls cannot disagree,
		// and Run has already failed the boot if this configuration is refusable.
		tlsOpts, _, terr := transportCredentials(k.Config)
		if terr != nil {
			// FAIL, never degrade. Run already refused this configuration before
			// binding, so reaching here means it changed underneath us — and the
			// tempting fallback (serve with no credentials) is precisely the
			// fail-open direction this file exists to close.
			return fmt.Errorf("SEC-03: refusing to start the gRPC server: %w", terr)
		}
		// ADR-0118 D3: the declared agent-plane premium services route auth —
		// exempt from the operator bearer, SurfaceAgent, x-agent-id principal.
		// Everything else under /cambrian.premium. now REQUIRES the bearer
		// (closing the pre-0118 pass-through).
		agentPlaneServices := make(map[string]bool, len(k.AgentGRPCServices))
		for name := range k.AgentGRPCServices {
			agentPlaneServices[name] = true
		}
		k.GRPC = grpc.NewServer(
			// TLS when configured. Empty when the plane is on loopback or an operator
			// explicitly opted into plaintext.
			append(tlsOpts,
				// ADR-0085 D7: stamp the surface FIRST, from the transport, so every
				// downstream handler knows where the request arrived from. It runs
				// unconditionally — the kernel always establishes the entry point; whether
				// that constrains anything is the decision point's business. Then the
				// credential interceptors: the agent plane's (ADR-0130 D1), which
				// establishes the agent principal, and the operator plane's (ADR-0047
				// D13), which gates the console and premium operator planes.
				grpc.ChainUnaryInterceptor(
					authz.UnarySurfaceInterceptor(agentPlaneServices),
					authz.UnaryAgentAuthInterceptor(agentAuth, agentPlaneServices),
					operator.UnaryAuthInterceptor(operatorIDP, agentPlaneServices, operator.MethodRoles(k.OperatorMethodRoles)),
					ratelimit.UnaryInterceptor(planeLimiter)),
				grpc.ChainStreamInterceptor(
					authz.StreamSurfaceInterceptor(agentPlaneServices),
					authz.StreamAgentAuthInterceptor(agentAuth, agentPlaneServices),
					operator.StreamAuthInterceptor(operatorIDP, agentPlaneServices, operator.MethodRoles(k.OperatorMethodRoles)),
					ratelimit.StreamInterceptor(planeLimiter)),
				// The operator binary-ingest lane (IngestMemory) carries raw file bytes;
				// gRPC's 4 MiB default rejects any PDF above it server-side before docling
				// ever runs, so the documented upload path is unusable for real documents.
				// Raise both directions to maxGRPCMessageBytes (512 MiB), which matches the
				// UI client's tonic limit (ui/src-tauri/src/transport.rs MAX_MESSAGE_BYTES).
				grpc.MaxRecvMsgSize(maxGRPCMessageBytes),
				grpc.MaxSendMsgSize(maxGRPCMessageBytes),
			)...)
		pb.RegisterOrchestratorServer(k.GRPC, k.Server)
		// PLAT-03 / ADR-0065: standard grpc.health.v1 on the main listener. Starts the
		// readiness probe loop (DB ping) and, if configured, the /healthz HTTP shim.
		if k.Health != nil {
			healthpb.RegisterHealthServer(k.GRPC, k.Health.Server())
			k.Health.Start(ctx, time.Duration(k.Config.Server.HealthCheckIntervalSeconds)*time.Second)
			if hp := k.Config.Server.HealthzPort; hp > 0 {
				// ADR-0130 D3: the shim is served under the one listener policy —
				// server.bind_address (loopback by default, NOT every interface as
				// the old ":port" server was), TLS when the kernel has it,
				// timeouts, body cap. A probe on another host needs the
				// deployment's bind_address, like every other listener.
				g.Go(func() error {
					srv, _, err := serveHTTP(k.Config, strconv.Itoa(hp), k.Health.HealthzHandler(), "healthz shim")
					if err != nil {
						// Fail the boot, as the old ListenAndServe error did.
						return fmt.Errorf("ADR-0065 healthz shim: %w", err)
					}
					<-ctx.Done()
					shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					return srv.Shutdown(shutCtx)
				})
			}
		}
		// The sequenced operator feed for the Cambrian UI. The spool decouples the
		// synchronous EventBus from network clients; SubscribeBridge fans the
		// existing domain events into it; the projection folds plan state.
		operatorFeed := operator.NewSpool(operator.SpoolConfig{})
		operator.SubscribeBridge(k.EventBus, operatorFeed)
		// Latch + heartbeat the agent roster so a feed consumer that connects AFTER boot
		// (e.g. the benchmark harness in --no-supervise) reliably receives the current
		// agent→capabilities roster instead of racing spool eviction of the one-time boot
		// AgentReadyOp events — the empty-capabilities routing-measurement bug.
		rosterLatch := operator.NewRosterLatch(operatorFeed)
		k.EventBus.Subscribe(domain.EventTypeAgentReady, rosterLatch.Observe)
		rosterLatch.Seed(ctx, k.Registry) // manifest baseline — present before the first AgentReadyEvent
		rosterLatch.Start(ctx, 0)
		operatorProjection := operator.NewProjection()
		operator.SubscribeProjection(k.EventBus, operatorProjection)
		// The live step-output stream reaches the feed's live-only lane as
		// TokenChunkEvent (ADR-0139 amendment 2026-11-06; formerly ADR-0047 0047-23's
		// managed-proxy TokenSink): gateway deltas from both model lanes and
		// agent-pushed EmitStepOutput chunks.
		if k.StepOutputFeed != nil {
			k.StepOutputFeed.attach(operatorFeed.EmitEphemeral)
		}
		// ADR-0079: fork the full prompt+completion of every managed-proxy agent turn
		// onto the feed's live-only exchange lane so a benchmark can review every agent
		// output and reconstruct the internal ReAct loop. Gated (default off) because
		// prompts are large/sensitive; the sink truncates to a bounded size and records
		// the untruncated lengths so truncation is visible downstream.
		if k.Config.Execution.LLM.CaptureLLMExchanges {
			const maxExchangeChars = 8192
			k.Server.LLMExchangeSink = func(sessionID, agentID, modelID string, stepIndex int, prompt, completion string) {
				operatorFeed.EmitEphemeral(llmExchangeEvent(sessionID, agentID, modelID, stepIndex,
					prompt, completion, maxExchangeChars))
			}
		}
		operatorSvc := operator.NewService(operatorFeed)
		operatorSvc.SetSnapshotSources(operatorProjection, k.SessionMgr)
		operatorSvc.SetIdentity(operatorIDP)
		// Contract 0102 WhoAmI: the caller's display name from the registry
		// (nil-safe: without a registry WhoAmI answers with an empty name).
		if k.Principals != nil {
			operatorSvc.SetPrincipalNames(k.Principals)
		}
		// ADR-0130 1f: failed logins become receipts when a plugin observes them.
		// Wired beside SetIdentity, before Serve; it is observation, not a gate —
		// the per-source limiter is built inside NewService and runs regardless.
		operatorSvc.SetLoginObserver(k.LoginObserver)
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
		operatorSvc.SetMemorySharer(k.MemorySharer) // ADR-0142 D12 ShareToTeam
		// ADR-0047 Amendment A2 (CORE-OPS-1): operator-plane paged reads. The tool
		// catalog (whole registry, not a per-agent menu), the system-skill registry,
		// and ScopeSystem memory recall (operator sees all data, D13).
		operatorSvc.SetReadSources(k.Server.ToolExecutor, k.Server.SkillRegistry, k.Memory.QueryService)
		// Document enumeration: the classification counterpart to memory recall.
		// Search cannot answer "which of my documents have no labels?" — that question
		// has no query text — and an unlabelled document is invisible to the policy
		// model rather than denied, so without this the console could not find the one
		// set of documents that most needs attention.
		//
		// Reads the store directly, like the operator's other reads: the operator plane
		// is ScopeSystem (D13) and sees all data. This is a listing of ids and labels,
		// never document bodies, so it discloses nothing a tag listing would not.
		operatorSvc.SetDocumentLister(k.Documents)
		// The KEYED read (contract 0086). Distinct from the lister above, which
		// deliberately returns no bodies: this one returns a document's text by id,
		// which is what makes a citation followable and what lets a watch resolve a
		// references-only signal to content it can actually read.
		// The SAME reader the plugins use, not a second implementation. It delegates
		// to the enforcing store, so the console and a watch resolve a reference
		// through one code path with one scope model — the alternative is two
		// answers to "can this principal read this document", differing by caller.
		operatorSvc.SetDocumentGetter(k.DocReader)
		// ADR-0081: the answer lane is only meaningful when the agentic retrieval
		// loop is wired (it synthesizes over multi-hop evidence). Gate on the same
		// flag; the "memory-answer" capability is advertised iff this is set.
		if k.Config.Execution.Retrieval.AgenticRetrievalEnabled {
			operatorSvc.SetMemoryAnswerer(k.Memory.QueryService)
		}
		// ADR-0047 A2.2: operator-triggered tool execution at ScopeSystem (audited,
		// idempotent). Reuses the kernel tool reference monitor with the System bypass.
		operatorSvc.SetToolExec(k.Server.ToolExecutor)
		// ADR-0047 A2.4: operator memory ingest requests a KERNEL document ingest —
		// the same chunking-pipeline / write-back path the agent plane and benchmarks
		// use — never a raw store write. The operator principal is stamped as Author;
		// the kernel derives classification (tags are a narrow-only hint).
		// ADR-0134 Q4: the operator plane clamps future event times with the same
		// configured tolerance the agent plane reads from ExecCfg.
		operatorSvc.SetEventTimeFutureTolerance(
			time.Duration(k.Config.Execution.Ingestion.EventTimeFutureToleranceMs) * time.Millisecond)
		// ADR-0136 A3.5 (contract 0104): the same descriptive-field caps the agent plane reads.
		mc := k.Config.Execution.Ingestion.SourceMetaCaps()
		operatorSvc.SetSourceMetaCaps(operator.SourceMetaCaps{Kind: mc.Kind, Title: mc.Title, Author: mc.Author, Speaker: mc.Speaker})
		operatorSvc.SetMemoryIngestor(operator.MemoryIngestorFunc(func(ctx context.Context, req operator.IngestRequest) (string, error) {
			if k.Server.IngestionProcessor != nil {
				doc := operatorIngestDoc(req)
				return k.Server.IngestionProcessor.ProcessSync(ctx, doc)
			}
			// No raw-store-write fallback, matching the agent plane. A direct
			// MemoryWriter.Remember here produced a structurally different row —
			// un-chunked, no source-document entity, invisible to ListDocuments — so
			// the SAME operator action wrote two different shapes of memory depending
			// on a queue-size setting. Failing is the honest outcome.
			return "", fmt.Errorf("ingestion pipeline not configured: every memory ingest " +
				"must go through the chunker, and there is no raw-store-write path")
		}))
		// ADR-0138 Amendment B1.3 (contract 0100): the console's "Replace <name>?" lookup,
		// answered by the ingestion manager's own identity resolution over the same
		// operatorIngestDoc the ingest builds, so the prompt and the write cannot
		// disagree. Wired only when the manager keeps document identity v2 with a
		// lifecycle store — under document_identity=legacy uploads are not keyed by
		// name, and advertising the prompt would promise a replace that never happens.
		if um, ok := k.Server.IngestionProcessor.(interface {
			UploadReplaceEnabled() bool
			MatchUpload(context.Context, domain.ExternalDocument) (domain.DocumentRecord, error)
		}); ok && um.UploadReplaceEnabled() {
			operatorSvc.SetUploadMatcher(operator.UploadMatcherFunc(func(ctx context.Context, req operator.IngestRequest) (domain.DocumentRecord, error) {
				return um.MatchUpload(ctx, operatorIngestDoc(req))
			}))
		}
		// ADR-0047 A2.6: watch CRUD is premium capability-gated. k.Server.WatchHandler
		// is nil in an OSS build (⇒ Unimplemented, WatchTriggered never publishes) and
		// the premium binary injects a real handler via Options.NewSignalReceiver.
		operatorSvc.SetWatchHandler(k.Server.WatchHandler)
		// Contract 0087: the reactive-pipeline read surface. Wired through an
		// indirection ALWAYS, rather than only when a plugin has already
		// registered, so the console does not depend on whether plugin Build ran
		// before or after this line — an ordering that is easy to change and
		// impossible to notice breaking.
		operatorSvc.SetPipelineLister(deferredPipelineLister{holder: &pipelineListers})
		// Contract 0088: the shadow-run surface, wired through the same
		// always-on indirection and for the same reason.
		operatorSvc.SetPipelineDryRunner(deferredPipelineDryRunner{holder: &pipelineDryRunners})
		// Contract 0089: the canvas read surface, same always-on indirection.
		operatorSvc.SetPipelineAuthor(deferredPipelineAuthor{holder: &pipelineAuthors})
		// Contract 0090: the draft-save surface. Separate from the author for the
		// same reason the ports are separate.
		operatorSvc.SetPipelineWriter(deferredPipelineWriter{holder: &pipelineWriters})
		operatorSvc.SetPipelineLifecycle(deferredPipelineLifecycle{holder: &pipelineLifecycles})
		// REACT-01 / ADR-0061: the reactive dead-letter read surface reads the OSS
		// bbolt journal. Wired only when the premium reactive engine is active — the
		// same signal as watch CRUD (k.Server.WatchHandler) — since that engine is what
		// writes dead-letters; an OSS build leaves it nil → Unimplemented.
		if k.Server.WatchHandler != nil && k.WatchDeadLetters != nil {
			operatorSvc.SetDeadLetterReader(k.WatchDeadLetters)
		}
		// REACT-05 / ADR-0071: watch observability (metrics + backtest), from the premium
		// ReactiveEngine.
		if k.WatchMetrics != nil {
			operatorSvc.SetWatchObservability(k.WatchMetrics, k.WatchBacktester)
		}
		// ROUTE-07 / ADR-0077: gatekeeper route-preview (deterministic merit scoring over
		// inline candidates, under the active scorer arm). Always available in core.
		if k.Metabolism != nil && k.Metabolism.Gatekeeper != nil {
			// ADR-0085: the access-policy administration surface (ExplainAccess,
			// ListClassificationTags). nil in OSS ⇒ those RPCs answer Unimplemented,
			// because an unscoped deployment has no policy to explain.
			operatorSvc.SetPolicyAdmin(k.PolicyAdmin)
			operatorSvc.SetRoutePreviewer(routePreviewAdapter{
				cfg:    k.Config.Execution,
				scorer: k.Metabolism.Gatekeeper.RouteScorer,
			})
		}
		// ── Contract 0072 (Wave 1) read sources ──────────────────────────────
		//
		// Each is wired only when its backing surface actually exists, because the
		// capability strings below are derived from which of these are non-nil. A
		// source wired without a real backing would advertise a surface that then
		// answers emptily — the "0 MCP servers on a kernel with no MCP" confusion
		// these RPCs were added to remove.
		var (
			cpLister   operator.CheckpointLister
			mcpSource  operator.MCPServerLister
			embedSrc   operator.EmbeddingReporter
			classifier operator.InputClassifier
		)
		if src, ok := k.CheckpointSource.(checkpointSource); ok && src != nil {
			cpLister = checkpointLister{src: src}
		}
		if k.MCPRuntime != nil {
			mcpSource = mcpLister{
				runtime:   k.MCPRuntime,
				connector: k.MCPConnector,
				toolsFor:  k.mcpToolCounter(),
				secrets:   k.ConfigStore,
				org:       k.credentialSeams.orgWriter(),
			}
		}
		embedSrc = embeddingReporter{cfg: k.Config.Embedder, counter: k.VectorCounter}
		if k.Server != nil && k.Server.Router != nil {
			classifier = inputClassifier{router: k.Server.Router}
		}
		operatorSvc.SetWave1Reads(cpLister, mcpSource, embedSrc, classifier)

		if k.TokenSeries != nil {
			operatorSvc.SetTokenSeriesReader(k.TokenSeries)
		}

		// Contract 0076: the blast-radius preview. Wired whenever agents can be
		// listed; the estimator itself reports INCOMPLETE for anything it cannot
		// inspect rather than returning a reassuringly empty radius.
		if k.Registry != nil {
			operatorSvc.SetBlastRadiusEstimator(blastRadiusEstimator{
				agents: func(ctx context.Context) []domain.AgentDefinition {
					all, err := k.Registry.GetAllAgents(ctx)
					if err != nil {
						return nil
					}
					return all
				},
				scopeOf: k.agentScopeRenderer(),
				// In-flight plan tracking is not projected on this build, so the
				// preview marks itself incomplete and names why. That is the honest
				// state: a partial radius shown as total is the understatement this
				// RPC exists to prevent.
				inFlight: nil,
			})
		}

		// Contract 0075: propose WITHOUT committing. It reaches the planner
		// directly rather than through Server.Execute, because Execute is the
		// committing path — it binds a session, persists a run and dispatches
		// agents. Suppressing those afterwards would make the "nothing committed"
		// promise depend on every future edit to Execute remembering this caller.
		if k.Awareness != nil && k.Awareness.Planner != nil {
			var clarifier *awareness.Clarifier
			if k.Awareness.LLM != nil {
				clarifier = &awareness.Clarifier{
					LLM:        k.Awareness.LLM,
					Vocabulary: k.Config.Execution.Router.ClassificationVocabulary,
					Documents:  documentTagCounter(k.Documents),
				}
			}
			operatorSvc.SetPlanProposer(planProposer{
				planner:   k.Awareness.Planner.GetExecutionPlan,
				clarifier: clarifier,
				tags: func(context.Context) []string {
					return k.Config.Execution.Router.ClassificationVocabulary
				},
			})
		}

		if k.LLMProvider != nil {
			operatorSvc.SetGeneratorRegistry(generatorRegistry{
				cfg:      k.Config.LLMProvider,
				provider: k.LLMProvider,
				runtime:  k.GeneratorRuntime,
				secrets:  k.ConfigStore,
			})
		}

		// ADR-0101 D7: the read half of the runtime-config surface. Wired only when
		// provenance exists, because value_source is the field that earns this RPC
		// — without it the form can report a value but not what pins it, which is
		// the disclaiming state it was added to replace.
		if k.ConfigProvenance != nil {
			var live func(string) (float64, bool)
			if k.Memory != nil && k.Memory.QueryService != nil {
				live = liveBlendWeights(k.Memory.QueryService)
			}
			var overrides func() []config.StoreOverride
			if k.ConfigStore != nil {
				store, path := k.ConfigStore, k.ConfigPath
				// Read at call time: the console asks after its own writes.
				overrides = func() []config.StoreOverride {
					m, err := store.Overrides()
					if err != nil {
						slog.Warn("config schema: store overrides unreadable; overrides_file left empty", "err", err)
						return nil
					}
					return config.StoreOverrideReport(path, m)
				}
			}
			operatorSvc.SetConfigSchemaReporter(configSchemaSource{
				cfg:       k.Config,
				prov:      k.ConfigProvenance,
				live:      live,
				overrides: overrides,
			})

			// ADR-0101 D3: the durable WRITE path. Wired only alongside a real
			// store, because a Save button that cannot persist is worse than none —
			// it reports success for a change that vanishes on restart.
			if k.ConfigStore != nil {
				writer := configWriteSource{
					store:      k.ConfigStore,
					org:        k.credentialSeams.orgWriter(),
					configPath: k.ConfigPath,
					prov:       k.ConfigProvenance,
					hotApply:   hotApplyFor(k.OperatorEffects),
					// LIVE list + default (boot + every save since), so credential/
					// role validation and the remove/default guards judge current
					// state — a just-saved generator is immediately a valid target.
					generatorList:      k.GeneratorRuntime.list,
					defaultGeneratorID: k.GeneratorRuntime.defaultGenerator,
				}
				// Contract 0096: role writes hot-apply onto the live provider —
				// resolution reads the role map per call, so the next call the
				// organ makes goes to the new generator, no restart. Left nil
				// (restart_required) when no provider is configured.
				if k.LLMProvider != nil {
					writer.liveRoles = k.LLMProvider.Roles
					writer.applyRole = func(role, id string) bool {
						k.LLMProvider.SetRole(role, id)
						return true
					}
					// Live generator registration (owner directive 2026-08-12):
					// SaveGenerator/RemoveGenerator swap the broker's table, then
					// update the sibling tables that would otherwise drift — the
					// runtime list (console reads + guards), the gateway's default
					// rung (it serves through the Provider, so it needs no client
					// table of its own since ADR-0139 wave 1c), and the auction's
					// TraitModel agents (step allocation); the reconcile evicts the
					// agents of removed ids.
					liveGateway := k.LLMGateway
					agentRepo, _ := k.Registry.(*kernel.AgentRepoDecorator)
					kernelCtxGen := ctx
					writer.applyGenerators = func(gens []config.GeneratorConfig, defaultID string) bool {
						if err := k.LLMProvider.ReloadGenerators(gens, defaultID); err != nil {
							slog.Warn("live generator reload failed; stored for next boot", "err", err)
							return false
						}
						if k.GeneratorRuntime != nil {
							k.GeneratorRuntime.set(gens, defaultID)
						}
						if liveGateway != nil {
							// The gateway serves through the Provider reloaded above;
							// only its last-rung default needs moving.
							liveGateway.SetDefaultModelID("llm:" + defaultID)
						}
						if agentRepo != nil {
							registerModelAgents(agentRepo, gens)
							reconcileModelAgents(kernelCtxGen, agentRepo, gens)
						}
						return true
					}
				}
				operatorSvc.SetConfigWriter(writer)
				operatorSvc.SetSecretWriter(writer)
				// Contract 0083: the write half of the generator surface. Same
				// store, same shadowing rules — the console's Save button now
				// has somewhere to land other than a gitignored config file.
				operatorSvc.SetGeneratorWriter(writer)

				// Contract 0097: the MCP write half. The apply closures capture
				// the KERNEL's ctx, never the RPC's — a health loop parented on
				// a request context would die when the save returned.
				kernelCtx := ctx
				writer.mcpServerList = func() []config.MCPServerConfig {
					return k.Config.MCP.Servers
				}
				if k.MCPConnector != nil && k.MCPRuntime != nil {
					writer.applyMCPServer = func(s config.MCPServerConfig, source string) bool {
						k.MCPRuntime.upsertConfig(s, source)
						if !s.IsEnabled() {
							// Disabled: stop the process, withdraw the tools, keep it
							// listed as "stopped".
							k.MCPConnector.StopServer(kernelCtx, s.ID, k.MCPSink)
							return true
						}
						k.MCPConnector.AddServer(kernelCtx, mcpServerFromConfig(s, k.MCPRuntime.sandbox), k.MCPSink, 30*time.Second)
						return true
					}
					writer.detachMCPServer = func(id string) bool {
						k.MCPRuntime.remove(id)
						k.MCPConnector.RemoveServer(kernelCtx, id, k.MCPSink)
						return true
					}
					writer.bounceMCPServer = func(id string) bool {
						sc, ok := k.MCPRuntime.get(id)
						if ok && !k.MCPRuntime.enabled(id) {
							// Disabled: nothing runs; the credential is used when
							// it is enabled. Live in the sense that nothing stale
							// keeps running with the old one.
							return true
						}
						if !ok {
							// Configured in the store but not armed on this kernel —
							// nothing live to bounce; the next boot picks the token up.
							return false
						}
						k.MCPConnector.AddServer(kernelCtx, sc, k.MCPSink, 30*time.Second)
						return true
					}
					// ADR-0141: an org account rotated or revoked from the
					// accounts plane reconnects the shared servers bound to it.
					if k.credentialSeams != nil {
						k.credentialSeams.setBounce(writer.bounceMCPServer)
					}
					writer.probeMCPServer = func(ctx context.Context, s config.MCPServerConfig) operator.MCPTestResult {
						r := k.MCPConnector.Probe(ctx, mcpServerFromConfig(s, k.MCPRuntime.sandbox))
						return operator.MCPTestResult{
							OK: r.OK, LatencyMs: r.LatencyMs, ToolNames: r.ToolNames, Error: r.Err,
						}
					}
				}
				operatorSvc.SetMCPWriter(writer)

				// Integrations (capability "integrations"): the product surface
				// over the same writer, runtime and connector — catalog installs,
				// enable/disable, secrets, health and test. Needs the live
				// connector: without it nothing could start or report.
				if k.MCPConnector != nil && k.MCPRuntime != nil {
					catalogPath := integrations.ResolvePath(k.Config.MCP.CatalogFile, k.ConfigPath)
					cat, catErr := integrations.Load(catalogPath)
					if catErr != nil {
						slog.Error("integration catalog: the deployment catalog is invalid; offering the built-in catalog only",
							"file", catalogPath, "err", catErr)
						cat, catErr = integrations.Load("")
					}
					if catErr != nil {
						slog.Error("integration catalog: the built-in catalog failed to load; integrations disabled", "err", catErr)
					} else {
						operatorSvc.SetIntegrationManager(&integrationManager{
							writer:    writer,
							catalog:   cat,
							runtime:   k.MCPRuntime,
							connector: k.MCPConnector,
						})
					}
				}
			}
		}

		// ADR-0141: the credential provider's (premium accounts) audit sink
		// and plugin-plane mutation contract — the same audit store + feed and
		// command_id dedupe as every operator write.
		if k.credentialSeams != nil {
			k.credentialSeams.bind(operatorSvc)
		}

		// Contract 0098: the identity plane's REVIEW lane (five-planes step 2,
		// FIVE-PLANES-BUILD.md). Wired outside the ConfigStore block above
		// deliberately — confirming a link persists to the links table, not to
		// the ADR-0101 config store, so a kernel with no durable config store
		// still has a review surface as long as it has a database.
		//
		// Producers below the trust ceiling (derived, scored, correlation) can
		// only ever write `candidate`. Without this binding those proposals were
		// write-only: the queue filled and nothing in the product could empty it.
		if k.Links != nil {
			operatorSvc.SetLinkWriter(linkWriteSource{links: k.Links, review: k.linkReview})
		}

		// ADR-0047 D14: capability + version handshake. The UI hides surfaces this
		// build does not advertise and warns on contract-version skew.
		// ADR-0047 Amendment A2: contract bumped 0047→0048 for the CORE-OPS-1 read/
		// exec/approval surface. watches-* are advertised ONLY when the premium watch
		// handler is wired (D14) — an OSS kernel hides the Watches screen.
		operatorCaps := []string{
			"feed", "snapshot", "commands", "steering", "audit",
			// kernel-logs (contract 0082): QueryLogs/TailLogs read the in-process
			// retention window. OPERATOR-ONLY — a Viewer is refused, because a log
			// line bypasses the access-policy plane and is not filtered per
			// principal. A console gates the whole surface on this string.
			"kernel-logs",
			"tools-read", "tools-manage", "skills-read",
			"memory-read", "memory-ingest", "tool-exec", "tool-approvals",
			// addressed-approvals (ADR-0142 D11): WatchMyApprovals / ListApprovals /
			// AnswerApproval / ListStandingApprovals / RevokeStandingApproval, the
			// ApprovalOp rule/answerable/state fields and the feed's HITLResolvedOp.
			// share-to-team (ADR-0142 D12): ShareToTeam.
			"addressed-approvals", "share-to-team",
			// memory-ingest-binary: IngestMemoryOpRequest carries `content`/`filename`
			// (raw bytes -> docling_agent structure parse) + `context`, and MemoryOp
			// carries `section_path`/`text` for citations. A UI gates its file-upload
			// affordance on this — an older kernel silently text-only-ingests.
			"memory-ingest-binary",
			// routing-trace: SelectionEventOp (contract 0102; was AuctionEventOp)
			// carries the Gatekeeper L1/L2/L3 candidate funnel + winner margin +
			// candidate requirements (backlog ROUTE-02).
			"routing-trace",
			// selection-cost (ADR-0100 P2): SelectionEventOp carries
			// selection_latency_ms + selection_boots, so a client can measure what a
			// routing decision cost.
			"selection-cost",
			// route-preview: PreviewRoute deterministic gatekeeper merit scoring (ROUTE-07).
			"route-preview",
			// document-read (contract 0086): GetDocument fetches one document by id,
			// body included. The keyed read the memory lane never had — ListDocuments
			// is keyed but bodyless, QueryMemory has bodies but is ranked. Advertised
			// unconditionally: every build can carry the RPC, and whether a store
			// backs it is reported by Unimplemented rather than by a missing string.
			"document-read",
			// retention-feed (contract 0084, ADR-0102 A1): RetentionRunOp on the feed,
			// reporting what a compaction pass deleted, whether it was capped, and
			// whether it failed.
			//
			// Advertised UNCONDITIONALLY, unlike the source-backed strings below. The
			// capability is the kernel's ability to CARRY the event, which is true of
			// every build; whether any source emits one is a deployment fact. Gating it
			// on a registered plugin would make a console hide the retention view on an
			// OSS kernel that is perfectly able to display a journal-GC pass (GOV-02).
			"retention-feed",
			// native-tool-calling: the agent-plane GenerateWithTools RPC exists on this
			// build (ADR-0097 Phase B). Declared unconditionally because the RPC is
			// always served; whether a given STEP can use it depends on the model
			// allocated to it, which the kernel answers per call with
			// FailedPrecondition. A client uses this to decide whether the RPC is worth
			// attempting at all, rather than probing and getting Unimplemented.
			"native-tool-calling",
			// agent-steps: per-memory_query AgentStepOp on the feed — agent-loop
			// observability (query-thrash + retrieval-provenance poisoning signals).
			"agent-steps",
			// logout (contract 0099, ADR-0130 D5): the Logout RPC revokes the
			// caller's own bearer token. Unconditional: every build serves it.
			"logout",
			// memory-event-time (contract 0099, ADR-0134): the event-time fields on
			// ingest, query and hits. Unconditional because every build carries and
			// validates them; an event WINDOW this kernel cannot apply yet is refused
			// with Unimplemented, so a console never shows a filter that did nothing.
			"memory-event-time",
		}
		// integrations: the catalog + installed-server surface (List/Install/
		// Update/Set|ClearSecret/SetEnabled/Test/RemoveIntegration and the
		// integration_health feed event). Advertised only when wired: it needs
		// the config store and the live MCP connector.
		if operatorSvc.HasIntegrationManager() {
			operatorCaps = append(operatorCaps, "integrations")
		}
		// llm-exchange: full prompt+completion of every agent reasoning turn on the feed
		// (ADR-0079), advertised only when execution.capture_llm_exchanges is on so a
		// benchmark knows the exchange lane will actually emit for this kernel.
		if k.Config.Execution.LLM.CaptureLLMExchanges {
			operatorCaps = append(operatorCaps, "llm-exchange")
		}
		// memory-answer: AnswerMemory (ADR-0081) grounded, [n]-cited answer lane,
		// advertised only when the agentic retrieval path backs it.
		if operatorSvc.HasMemoryAnswerer() {
			operatorCaps = append(operatorCaps, "memory-answer")
		}
		// document-listing: ListDocuments enumerates documents by row, including the
		// unlabelled-only filter. Advertised only when a store backs it, so a console
		// never renders a document browser against a kernel that answers Unimplemented.
		if operatorSvc.HasDocumentLister() {
			operatorCaps = append(operatorCaps, "document-listing")
		}
		// upload-replace (contract 0100, ADR-0138 Amendment B1): uploads are keyed by
		// name (replace / exact-duplicate no-op) and ListDocuments answers the
		// upload_name lookup. Advertised only when the lookup is wired (identity v2 +
		// a lifecycle store), so a console never asks "Replace?" of a kernel that would
		// keep both anyway.
		if operatorSvc.HasUploadMatcher() {
			operatorCaps = append(operatorCaps, "upload-replace")
		}
		// Contract 0072 capabilities. Each is advertised only when a source backs
		// it, so a console never renders a surface against a kernel that answers
		// Unimplemented — and, just as importantly, can tell "this deployment has
		// none of these" apart from "this kernel cannot report them".
		operatorCaps = append(operatorCaps, operatorSvc.Wave1Capabilities()...)
		// reactive-pipelines (contract 0087): advertised only when a plugin
		// actually authors pipelines, so a console can tell "this build has no
		// pipeline runtime" from "this deployment has authored none".
		if pipelineListers.any() {
			operatorCaps = append(operatorCaps, "reactive-pipelines")
		}
		// pipeline-field-schema (contract 0091): GetPipeline / ValidatePipeline
		// carry the per-node field projection. Advertised with the authors,
		// because the projection is computed by them — a console on an older
		// kernel sees the capability absent and keeps its picker unavailable
		// with the reason, rather than reading an empty schema as "no fields".
		if len(pipelineAuthors.all()) > 0 {
			operatorCaps = append(operatorCaps, "pipeline-field-schema")
		}
		// pipeline-lifecycle (contract 0093): TransitionPipeline is wired, so
		// the console may render Publish/Arm/Pause as real buttons rather than
		// the disabled placeholders older kernels required.
		if len(pipelineLifecycles.all()) > 0 {
			operatorCaps = append(operatorCaps, "pipeline-lifecycle")
		}
		// config-schema: GetConfigSchema reports live tunable values AND the layer
		// supplying each one. Advertised separately because it is wired from the
		// config pipeline rather than from a kernel subsystem.
		if operatorSvc.HasConfigSchema() {
			operatorCaps = append(operatorCaps, "config-schema")
		}
		// config-write: SetConfig / DeleteConfig / SetGeneratorKey persist DURABLY
		// (ADR-0101). Advertised separately from config-schema because a kernel can
		// report its configuration without being able to persist a change to it —
		// and a console must render a read-only form in that case rather than a
		// Save button that silently does nothing.
		if operatorSvc.HasConfigWriter() {
			operatorCaps = append(operatorCaps, "config-write")
		}
		// authored-plans: SubmitPlan accepts an operator-written DAG. Without it a
		// console must hide Run/Dry-run rather than offer buttons with nowhere to go.
		if operatorSvc.HasPlanSubmitter() {
			operatorCaps = append(operatorCaps, "authored-plans")
		}
		// token-series: GetTokenSeries projects hourly usage. Without it a console
		// must say "no history is projected" rather than draw a flat line, which
		// reads as zero spend.
		if operatorSvc.HasTokenSeries() {
			operatorCaps = append(operatorCaps, "token-series")
		}
		if operatorSvc.HasBlastRadiusEstimator() {
			operatorCaps = append(operatorCaps, "blast-radius")
		}
		// propose-plan: ProposePlan shapes work without committing to it. A console
		// gates its "nothing committed yet" header on this — without the RPC the
		// only way to see a plan is to start one.
		if operatorSvc.HasPlanProposer() {
			operatorCaps = append(operatorCaps, "propose-plan")
		}
		// links (contract 0098): the identity plane's review lane —
		// ListLinkCandidates + ConfirmLink/RetractLink/RetractLinksByProducer.
		// Advertised separately from the substrate reads because a kernel can
		// ANSWER over links without being able to review them, and a console must
		// hide the queue in that case rather than draw one whose buttons return
		// Unimplemented.
		if operatorSvc.HasLinkWriter() {
			operatorCaps = append(operatorCaps, "links")
		}
		// ADR-0139 D14 (contract 0101): the operator half of principal memory notes.
		// Advertised only when the service is built, so a console never renders a
		// notes screen whose RPCs answer Unimplemented.
		if k.Server != nil && k.Server.MemoryNotes != nil {
			operatorSvc.SetMemoryNotesAdmin(k.Server.MemoryNotes)
		}
		if operatorSvc.HasMemoryNotes() {
			operatorCaps = append(operatorCaps, "memory-notes")
		}
		// ADR-0139 D13 (contract 0101): the skill lifecycle — proposal queue, locks,
		// ledger, rollback and the SkillOp lifecycle fields. Advertised only when
		// the Postgres lifecycle store is wired (wireSkillLifecycle).
		if k.SkillLifecycle != nil {
			operatorSvc.SetSkillLifecycle(k.SkillLifecycle)
		}
		if operatorSvc.HasSkillLifecycle() {
			operatorCaps = append(operatorCaps, "skills-lifecycle")
		}
		// Per-agent skill access grants (decisions/skill-access-grants.md):
		// ListSkillGrants / GrantSkill / RevokeSkillGrant. Advertised only when the
		// 0031 grant store is wired.
		if k.SkillAccess != nil && k.SkillAccess.HasStore() {
			operatorSvc.SetSkillGrants(k.SkillAccess)
		}
		if operatorSvc.HasSkillGrants() {
			operatorCaps = append(operatorCaps, "skill-grants")
		}
		// ADR-0082 D2: plugin-contributed capabilities. Each plugin declares the operator
		// surfaces it implements in its own manifest; the kernel collects them here and
		// advertises them WITHOUT interpreting any of them — which is what keeps downstream
		// (premium) vocabulary out of the open-source core. An absent or unentitled plugin
		// declares nothing, so its capabilities never appear and the UI hides those surfaces
		// exactly as it does on an OSS build.
		// chat: the ADR-0084 D9 conversation lane (OpenConversation/SendTurn/
		// CloseConversation/ListConversationMessages). Advertised only when the chat
		// worker pool is running (execution.chat_pool_size > 0) and a conversation
		// store exists; an OSS build with chat off returns Unimplemented and hides it.
		if k.ChatTurns != nil && k.Conversations != nil {
			operatorCaps = append(operatorCaps, "chat")
		}
		// Act-for conversations (on_behalf_of): only with a principal directory to
		// refuse unknown principals against (fail closed otherwise).
		if k.PrincipalDirectory != nil {
			operatorSvc.SetPrincipalDirectory(k.PrincipalDirectory)
		}
		if operatorSvc.HasPrincipalDirectory() && k.ChatTurns != nil && k.Conversations != nil {
			operatorCaps = append(operatorCaps, "conversations-act-for")
		}
		// Principal-owned recurring tasks: the agent-plane gate is built (store +
		// approval hub + not disabled), and the operator sees every principal's.
		if k.Server != nil && k.Server.ScheduledTasks != nil && k.PrincipalSchedules != nil {
			operatorSvc.SetPrincipalSchedules(k.PrincipalSchedules)
			operatorCaps = append(operatorCaps, "scheduled-tasks")
		}
		// ENG-472: the reactive journal that serves ListWatchDeadLetters also deletes
		// an entry an operator resolved (DeleteWatchDeadLetter); same condition as
		// the reader's wiring.
		if k.Server != nil && k.Server.WatchHandler != nil && k.WatchDeadLetters != nil {
			if _, ok := k.WatchDeadLetters.(domain.WatchDeadLetterDeleter); ok {
				operatorCaps = append(operatorCaps, "watch-deadletter-delete")
			}
		}
		// Contract 0074 watch liveness (T-20261101-contract-0074-watch-liveness-is-never-wired):
		// ListWatches reads each watch's source-stream state through the registry a
		// plugin registered. Fire history and the plane budget have no provider yet.
		if k.StreamRegistry != nil {
			operatorSvc.SetWatchLiveness(k.StreamRegistry, nil, nil)
		}
		// Contract 0103 (T-20261103-operator-plane-cannot-inspect-facts-no-rpc): the
		// read-only fact inspection RPCs, over the fail-closed inspector NewMemoryStack
		// composed. Advertised only when the fact substrate exists.
		if k.Memory != nil && k.Memory.FactInspector != nil {
			operatorSvc.SetFactInspector(k.Memory.FactInspector)
			operatorCaps = append(operatorCaps, "facts-inspect")
		}
		// ENG-431 (additive, capability only, no contract bump): the fact extractor's
		// counters on the Snapshot, so drops, failures, refusals and partial coverage
		// are visible outside the kernel log. Advertised only when extraction runs.
		if k.Memory != nil && k.Memory.FactExtractor != nil {
			operatorSvc.SetFactStats(k.Memory.FactExtractor)
			operatorCaps = append(operatorCaps, "facts-stats")
		}
		operatorCaps = append(operatorCaps, "session-lifecycle")
		// Contract 0105 (ADR-0137 S6, ENG-411 stage C): the compliance erase —
		// PreviewForgetMemory / ForgetMemory / ListErasures / GetErasure — over the same
		// forget service as the agent plane. Advertised only when erasure is enabled
		// (execution.erasure.enabled), so a console never offers a door that answers
		// Unimplemented.
		var forgetSvc *forget.Service
		if k.Server != nil {
			forgetSvc = k.Server.Forgetter
		}
		operatorSvc.SetForgetService(forgetSvc)
		if forgetSvc.Enabled() {
			operatorCaps = append(operatorCaps, "memory-erasure")
		}
		// step-output-stream (ADR-0139 amendment 2026-11-06, additive within 0101):
		// TokenChunkOp carries the live step-output stream — kind, lease, chunk_seq,
		// source — for every producer, the native tool lane included, and the agent
		// plane serves EmitStepOutput. Advertised when the stream is wired (always,
		// in this build), so a console renders live output only against a kernel
		// that produces it.
		if k.Server != nil && k.Server.StepOutput != nil {
			operatorCaps = append(operatorCaps, "step-output-stream")
		}
		operatorCaps = append(operatorCaps, k.PluginCapabilities...)
		// Contract 0070: adds the OSS ListDocuments RPC (ListDocumentsOpRequest /
		// ListDocumentsOpResponse / DocumentSummaryOp) and the "document-listing"
		// capability. Documents are an OSS concept — the store owns them with or
		// without the policy plugin — so this is NOT on the premium authz plane.
		//
		// It exists because access policy acts on labels and never on a document by
		// name, which makes an unlabelled document invisible to the policy model
		// rather than denied. Search cannot find one: "which of my documents have no
		// labels?" has no query text. Enumeration is the only instrument that answers
		// it, and the operator console had none.
		//
		// Contract 0069 (ADR-0097 D8): GenerateWithToolsRequest carries `messages`
		// (ModelMessageProto: role/content/tool_calls/tool_call_id) and deprecates the
		// single `prompt`. Native tool-calling is a CONVERSATION — the model must be
		// sent its own assistant turn and the tool result under the provider's call id —
		// and a lone prompt string could not express either. 0068 shipped that defect;
		// this revision is what makes the RPC usable.
		//
		// Contract 0068 (ADR-0097 Phase B): adds the agent-plane GenerateWithTools RPC
		// (native tool-calling) with ToolDefinitionProto / ModelToolCallProto. The
		// operator surface is unchanged, but the version is bumped anyway: the
		// handshake is the ONLY way a client can tell which agent-plane RPCs exist,
		// and the SetRuntimeConfig incident is the standing reminder that an
		// un-bumped proto change is invisible and undebuggable.
		//
		// Contract 0067 (ADR-0089): adds SnapshotResponse.plugins — every declared
		// plugin with its own version line, so a console can detect plugin-level skew
		// the way it already detects contract skew. 0066 (ADR-0085) added
		// ExplainAccess + ListClassificationTags, the AccessDecisionOp shape, and
		// QueryMemoryResponse.policy_note. The "access-policy" capability (declared by
		// the policy plugin's manifest) lets a console decide whether to render the
		// policy surfaces at all, instead of probing and getting Unimplemented.
		// Contract 0073 (ADR-0101 D3/D5): the durable WRITE path — SetConfig,
		// DeleteConfig, SetGeneratorKey, ClearGeneratorKey, with per-key
		// ConfigWriteOutcomeOp. The outcome shape is the point: a write that a
		// higher layer shadows is reported AT WRITE TIME, naming the variable,
		// rather than leaving the operator to infer it from a later read.
		// Capabilities "config-write" (+ "config-schema" for the read half).
		//
		// Contract 0072 (ADR-0101 + the operator UX refactor's Wave 1): adds
		// ListSessionCheckpoints, ListMCPServers, GetEmbeddingConfig, ClassifyInput,
		// ListGenerators / ListRoleAssignments / TestGenerator, RetryWatchDeadLetter;
		// PlanStepOp.required_capabilities; PluginInfoOp.last_check_unix_ms and the
		// "licence_unverifiable" state.
		//
		// Bundled as ONE bump rather than eight because each bump costs a re-vendor
		// across ui/proto, ui/src-tauri/pb.rs and cli/proto.
		//
		// Contract 0071 (ADR-0100 P2): AuctionEventOp gains selection_latency_ms +
		// selection_boots — the cost of the SELECTION decision, emitted identically by
		// the auction and dispatch arms so the orchestration suite can compare them.
		// Contract 0079: ADR-0098 progress onto the operator feed. The ephemeral
		// lane, because a status line is not history — replaying "working on it"
		// for a turn that finished an hour ago is worse than showing nothing.
		if k.Progress != nil {
			k.Progress.setFeed(operatorFeed.EmitEphemeral)
		}

		// Contract 0082: the kernel's own logs, operator-only.
		if k.Logs != nil {
			operatorSvc.SetLogRing(k.Logs)
		}
		// Contract 0096: SetRoleAssignment — the WRITE half of ListRoleAssignments.
		// Lands on the ADR-0101 store (per-role keys, so file-configured roles
		// compose) and hot-applies onto the live provider, so the effect is `live`.
		// RemoveGenerator now refuses to remove the global default or a generator a
		// role points at — the states that would refuse to boot, or silently
		// reroute an organ, on the next restart.
		// Contract 0097: the MCP server write half — SaveMCPServer/RemoveMCPServer
		// on the store with LIVE attach/detach through the connector,
		// SetMCPServerToken/ClearMCPServerToken (write-only, bounce-on-set), and
		// TestMCPServer (ephemeral probe of a possibly-unsaved spec). MCPServerOp
		// gains the declared half + token facts.
		// Contract 0098: the identity plane's review surface — ConfirmLink
		// (appends a human-mechanism row, never overwrites the proposal),
		// RetractLink (stamps state + retracted_at and nothing else),
		// RetractLinksByProducer (batch revocation, returning the count) and the
		// ListLinkCandidates read, plus `entity` / `why` / expand_aliases on the
		// substrate query plane.
		// Contract 0099: the bundled bump (owner decision 2026-11-01, ADR-0134 Q1).
		// BREAKING on the agent plane: the retired Orchestrator watch-CRUD RPCs and
		// their messages are deleted. Additive on the operator plane: Logout
		// (ADR-0130 D5), ADR-0134 event time (occurred_at / parts / time_precision on
		// IngestMemory; reference_time + event_after / event_before on QueryMemory and
		// AnswerMemory; event_time / event_time_end / time_source / time_precision on
		// MemoryOp and MemoryCitation, and on the agent plane's MemoryResult), and the
		// ADR-0101 D8 file facts on ConfigWriteOutcomeOp. Also: the /v1/admin HTTP
		// routes with an OperatorConsole equivalent are gone (write-tags remains).
		// Capabilities "logout" and "memory-event-time".
		// Contract 0100 (ADR-0138 Amendment B1, additive): IngestMemoryOpResponse gains
		// duplicate_of + version, ListDocumentsOpRequest gains the upload_name /
		// upload_tags replace-by-name lookup, DocumentSummaryOp gains updated_at_unix_ms
		// + version. Capability "upload-replace". (ADR-0137's erasure surface, which
		// planned 0100, takes the next free number after 0101.)
		// Contract 0101 (ADR-0139 phase 2/3, additive, one proto pass for D12/D13/D14):
		// agent plane SpawnSubagent, GetMemoryNotes, ProposeMemoryNote, ProposeSkill;
		// operator plane List/Get/Set/DeleteMemoryNotes + SetMemoryNotesBudget
		// (capability "memory-notes") and the D13 skill-lifecycle RPCs and SkillOp /
		// ListSkillsOpRequest fields (capability "skills-lifecycle"). Each capability
		// is advertised only when its store is wired, so one bump covers all three.
		// Still 0101 after the ADR-0139 amendment of 2026-11-06 (live step-output
		// stream): its changes are purely additive — TokenChunkOp fields 4-10 and the
		// agent-plane EmitStepOutput RPC — and are announced by the
		// "step-output-stream" capability, as the other additive passes were.
		// Still 0101 after ADR-0142 D11/D12 (2026-11-06): ApprovalOp fields 7-21,
		// OperatorEvent.hitl_resolved (34), WatchMyApprovals / ListApprovals /
		// AnswerApproval / ListStandingApprovals / RevokeStandingApproval /
		// ShareToTeam — additive, announced by "addressed-approvals" and
		// "share-to-team"; the legacy approval streams keep their pending-only shape.
		// 0102 (2026-11-14, owner decision third round): WhoAmI and CancelRun
		// (T-20261106-operator-plane-has-no-whoami-an-oidc-signed-in,
		// T-20261106-operator-closesession-only-pauses-a-live-plan); CancelRun is
		// also announced by "run-cancel". Same bump, owner decision fourth round:
		// the auction-named feed payload AuctionEventOp (OperatorEvent field 10,
		// now reserved) / BidEntryOp / InterviewResultOp are replaced by
		// SelectionEventOp (field 35) / SelectionCandidateOp / SemanticGateOp
		// (T-20261113-operator-feed-still-names-dispatch-decisions), and
		// AuthoredStepOp.checkpoint_query (7) is reserved (ENG-1313). Breaking for
		// a 0101 client reading the dispatch decision; the console never folded it.
		// 0103 (2026-11-14, owner decision on
		// T-20261103-operator-plane-cannot-inspect-facts-no-rpc): read-only fact
		// inspection — ListFacts, GetFactChain, GetFactCoverage — additive, also
		// announced by the "facts-inspect" capability.
		// 0104 (2026-11-15, additive): ConfigSchemaOp.overrides_file /
		// overrides_file_value (ADR-0101 D8, ENG-257), MemoryOp fact_matches /
		// fact_match / fact_current (ENG-461), and on the agent plane
		// ExecuteToolResponse.annotations (ADR-0139 D11 hook context, ENG-629).
		// 0105 (2026-11-16, additive; ADR-0137 S6, ENG-411 stage C): memory erasure —
		// operator plane PreviewForgetMemory / ForgetMemory / ListErasures / GetErasure
		// (+ ErasureSelectorOp / ErasureCandidateOp / ErasureReceiptOp), agent plane
		// PreviewForget / Forget / ListMyErasures; announced by "memory-erasure" when
		// execution.erasure.enabled.
		// 0106 (2026-11-17, additive; ADR-0137 D2, ENG-1948): agent-plane
		// PreviewForgetRequest.query (the query form, resolved by the caller's own
		// scoped recall) and ErasureCandidate.snippet (agent query previews only).
		// The operator plane is unchanged: its previews stay ids/counts only.
		// 0107 (2026-11-17, additive; ENG-1953): agent-plane
		// PreviewForgetResponse.truncated — a query preview cut by
		// execution.erasure.preview_max_candidates says so. Operator plane unchanged.
		operatorSvc.SetHandshake(KernelVersion, "0107", operatorCaps)
		// ADR-0089: plugin identity rides the same handshake. Reported for EVERY
		// declared plugin, registered or not — the console needs to distinguish "this
		// deployment has no such plugin" from "it declined to register".
		operatorSvc.SetPlugins(pluginHandshake(k.PluginStatuses))
		// ADR-0047 0047-10: chat & steer. CreateSession is wired to the
		// SessionManager; SendMessage/Inject dispatch through the Execute path is
		// the pending executor-producer side (nil hooks ⇒ Unimplemented).
		operatorSvc.SetSessionOps(operator.SessionOpsFuncs{
			CreateFn: func(ctx context.Context, goal, parentID string) (string, error) {
				// BRAIN-01: persist the caller's scope at session start, so later turns
				// can re-derive effective = caller_scope ∩ agent_scope (ADR-0034 D13).
				ses, err := k.SessionMgr.CreateScopedSession(ctx, goal,
					domain.SessionID(parentID), callerScopeFor(ctx, k.Server.Authz))
				if err != nil {
					return "", err
				}
				return string(ses.ID), nil
			},
			// ADR-0047 0047-21: dispatch the message through the kernel Execute path
			// (session threaded via x-session-id, the loadOrCreateSession key) and
			// return immediately — the operator watches progress on the feed.
			// Phase 2: the steering commands persist the lifecycle status through here,
			// so a paused session actually reads as paused and can be resumed.
			SetStatusFn: func(ctx context.Context, sessionID string, st domain.SessionStatus, reason string) error {
				return k.SessionMgr.TransitionStatusReason(ctx, domain.SessionID(sessionID), st, reason)
			},
			SendFn: func(ctx context.Context, sessionID, text string) error {
				// ENG-1378 (owner 2026-11-14): the run carries the operator's request
				// values, detached from its cancellation (operatorRunContext).
				runCtx := operatorRunContext(ctx, sessionID)
				go func() {
					if _, err := k.Server.Execute(runCtx, &pb.Handoff{Payload: &pb.Object{Data: []byte(text)}}); err != nil {
						slog.Warn("operator SendMessage execution failed", "session", sessionID, "err", err)
						// Tell the CONSOLE, not just the log. This returns immediately and
						// the operator watches the feed, so a turn that fails here used to
						// deliver nothing at all: no reply, no error, and a progress line
						// still reading "working on it". ADR-0098 D3 built the terminal
						// update for exactly this — `Note` is documented as "the closing
						// line a FINAL update leaves on screen instead of clearing — used
						// to say what went wrong" — and this path simply never emitted it.
						//
						// It rides the progress channel rather than becoming a message on
						// purpose (ADR-0098): a failure notice belongs in front of the user
						// but NOT in the transcript, or "something went wrong" feeds back
						// into the model's context next turn and a failed turn stops
						// storing nothing.
						if k.Progress != nil {
							k.Progress.Progress(context.Background(), domain.ProgressUpdate{
								ConversationID: sessionID,
								Final:          true,
								Note:           chatFailureNote(err),
								UpdatedAt:      time.Now().UTC(),
							})
						}
					}
				}()
				return nil
			},
		})
		// Contract 0074: operator-authored plans. The submitter reuses the SAME
		// execution path a planner-produced plan takes — it hands the plan in via
		// network.AuthoredPlanMetadataKey rather than opening a second entrypoint,
		// so session binding, run persistence, checkpointing and the feed are all
		// the existing code.
		if k.SessionMgr != nil && k.Server != nil {
			operatorSvc.SetPlanSubmitter(planSubmitter{
				sessions: func(ctx context.Context, goal string) (string, error) {
					ses, err := k.SessionMgr.CreateScopedSession(ctx, goal, "",
						callerScopeFor(ctx, k.Server.Authz))
					if err != nil {
						return "", err
					}
					return string(ses.ID), nil
				},
				execute: func(ctx context.Context, sessionID, planJSON string) {
					runCtx := operatorRunContext(ctx, sessionID) // ENG-1378, as SendFn
					go func() {
						_, err := k.Server.Execute(runCtx, &pb.Handoff{
							Payload: &pb.Object{Data: []byte("")},
							Metadata: map[string]string{
								subnetwork.AuthoredPlanMetadataKey: planJSON,
							},
						})
						if err != nil {
							slog.Warn("operator SubmitPlan execution failed", "session", sessionID, "err", err)
						}
					}()
				},
				known: func(id string) bool {
					if k.Registry == nil {
						// Cannot verify ⇒ do not warn. A false "no such agent" on
						// every pin would train an operator to ignore the warning,
						// and then the real one goes unread too.
						return true
					}
					a, err := k.Registry.GetAgentByName(context.Background(), id)
					return err == nil && a != nil
				},
			})
		}

		// ADR-0084 D9: the OSS chat lane on the operator plane. Wired only when the chat
		// TurnService exists (execution.chat_pool_size > 0 + a conversation store), so an
		// OSS build with chat off honestly reports SendTurn/OpenConversation as Unimplemented
		// and the "chat" capability is not advertised.
		if k.ChatTurns != nil && k.Conversations != nil {
			operatorSvc.SetConversationOps(&conversationOps{store: k.Conversations, turns: k.ChatTurns})
		}
		pb.RegisterOperatorConsoleServer(k.GRPC, operatorSvc)
		// ADR-0073: let a downstream (premium) binary mount ADDITIONAL gRPC services it
		// defines in its own proto (e.g. the reactive control plane the benchmark harness
		// drives). Registered after the core services and before Serve; inherits the
		// server-level operator auth interceptors. nil in OSS ⇒ no extra services.
		if k.ExtraServices != nil {
			k.ExtraServices(k.GRPC)
		}
		// ADR-0118 D3: agent-plane premium services, mounted on the same server;
		// the interceptors already treat their declared names as agent plane.
		for _, mount := range k.AgentGRPCServices {
			mount(k.GRPC)
		}
		slog.Info("🧬 Cambrian Substrate Active", "port", k.Config.Server.Port)
		// ADR-0157 S2: every service is registered; the kernel-only agents'
		// unix sockets (opened as their instances spawned) serve from here on.
		if k.agentSockets != nil {
			k.agentSockets.start(k.GRPC)
		}
		return k.GRPC.Serve(k.Listener)
	})

	// D. Ingestion HTTP server (ADR-0028) — opt-in via IngestionHTTPPort > 0.
	//
	// ADR-0130 D3: served by the one listener policy (serveHTTP): bound on
	// server.bind_address rather than ":port" (every interface), refused in
	// plaintext on a routable bind, TLS when the kernel has it, header/idle
	// timeouts, every body capped. /v1/ingest keeps its own X-Ingest-Token
	// (constant-time, ADR-0028).
	//
	// Contract 0099 removed the /v1/admin routes that have an OperatorConsole
	// equivalent, so each mutation has ONE door, and it is audited:
	//   /v1/admin/consolidate              -> OperatorConsole/TriggerConsolidation
	//   /v1/admin/agents/{id}/scope        -> OperatorConsole/SetScope
	//   /v1/admin/agents/{id}/tool-grants  -> OperatorConsole/SetToolGrant + SetToolPolicy
	// One route remains: /v1/admin/agents/{id}/write-tags (ADR-0035 C2,
	// PolicyAdmin.SetAgentWriteTags) has NO operator-plane RPC yet, and the access
	// policy is inert without a write ceiling (premium cmd/cambrian-apply and the
	// access_policy benchmark's out-of-band setup depend on it). It requires the
	// OPERATOR bearer, validated through the same OperatorIdentity the gRPC
	// interceptor uses, so a Logout or an expired token closes it too. Moving it
	// onto OperatorConsole is T-20261101-write-tags-admin-route-has-no-operatorconsole.
	if port := k.Config.Execution.Ingestion.IngestionHTTPPort; port > 0 {
		mux := http.NewServeMux()
		// ADR-0142 (K-3): /v1/ingest is never anonymous. With an ingest token the
		// pusher presents X-Ingest-Token; without one it presents the operator
		// bearer (resolved through the same Authenticator as the gRPC plane), where
		// an empty token used to switch authentication off entirely.
		if tok := k.Config.Execution.Ingestion.IngestToken; tok != "" {
			mux.Handle("/v1/ingest", memory.NewWebhookReceiver(tok, k.Memory.IngestionManager.Enqueue))
		} else {
			slog.Warn("ADR-0142: execution.ingestion.ingest_token is empty; /v1/ingest requires the operator bearer",
				"fix", "set execution.ingestion.ingest_token for token-based pushers")
			mux.Handle("/v1/ingest", requireOperatorBearer(operatorIDP,
				memory.NewPreauthenticatedWebhookReceiver(k.Memory.IngestionManager.Enqueue)))
		}
		mux.Handle("/v1/admin/agents/", requireOperatorBearer(operatorIDP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Path: /v1/admin/agents/{id}/write-tags
			rest := strings.TrimPrefix(r.URL.Path, "/v1/admin/agents/")
			agentID, suffix, ok := strings.Cut(rest, "/")
			if !ok || agentID == "" || suffix != "write-tags" {
				http.Error(w, "expected /v1/admin/agents/{id}/write-tags (scope and tool grants moved to "+
					"OperatorConsole SetScope / SetToolGrant / SetToolPolicy in contract 0099)", http.StatusNotFound)
				return
			}
			// Write-tags are POLICY administration, so they exist only when a policy
			// plugin is installed. An OSS build answers 501 rather than pretending to
			// have stored a boundary it will never enforce.
			if k.PolicyAdmin == nil {
				http.Error(w, "access-policy plugin not installed; scope administration unavailable", http.StatusNotImplemented)
				return
			}
			// ADR-0035 C2: set the agent's DefaultWriteTags (write classification).
			// Body: {"default_write_tags":["company_wide","analytics"]}
			var body struct {
				DefaultWriteTags []string `json:"default_write_tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid write-tags body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := k.PolicyAdmin.SetAgentWriteTags(r.Context(), agentID, body.DefaultWriteTags); err != nil {
				http.Error(w, "write-tags rejected: "+err.Error(), http.StatusBadRequest)
				return
			}
			slog.Info("ADR-0035: agent DefaultWriteTags updated", "agent_id", agentID, "tags", body.DefaultWriteTags)
			w.WriteHeader(http.StatusOK)
		})))
		srv, addr, err := serveHTTP(k.Config, strconv.Itoa(port), mux, "ingestion http server")
		if err != nil {
			// FAIL the boot, never degrade: the only degradation available is the
			// ":port"-on-every-interface server this replaced.
			g.Go(func() error { return fmt.Errorf("ADR-0130: %w", err) })
			return
		}
		slog.Info("🌐 Ingestion HTTP server started", "addr", addr,
			"queue_size", k.Config.Execution.Ingestion.IngestionQueueSize,
			"workers", k.Config.Execution.Ingestion.IngestionWorkers,
			"admin_auth", "operator bearer (ADR-0130 D3)")
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
	// onChange runs after a server's tool set changed (registered or dropped),
	// outside the sink's lock. It re-expands profile agents' tool grants (agent
	// stack v2), since MCP tools register after boot. nil ⇒ nothing to do.
	onChange func(ctx context.Context)
}

func (s *mcpToolSink) changed(ctx context.Context) {
	if s.onChange != nil {
		s.onChange(ctx)
	}
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
	defer s.changed(ctx) // deferred first ⇒ runs after the unlock below
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
	defer s.changed(ctx) // deferred first ⇒ runs after the unlock below
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.byServer[serverID] {
		s.reg.Remove(name)
		_ = s.indexer.Remove(ctx, name)
	}
	delete(s.byServer, serverID)
}

var _ mcp.ToolSink = (*mcpToolSink)(nil)

// registerAgentSources discovers and upserts agent definitions from plugin AgentSources
// (ADR-0075). Parallel to registerModelAgents: each definition goes through the same
// reg.SetAgent path as any other agent, so it participates in the scope/merit/dispatch
// machinery normally. A System=true definition (from Registry.AddSystemAgent) is logged
// as an explicit privilege grant — the grant is visible, not inferred.
//
// It returns every agent it registered, so the composition root can bind what a
// declarative profile declares (tool grants and ceiling, model preference) and
// prune profile agents that were not discovered this boot.
func registerAgentSources(ctx context.Context, reg *kernel.AgentRepoDecorator, sources []AgentSource) []DiscoveredAgent {
	var registered []DiscoveredAgent
	for _, src := range sources {
		discovered, err := src.DiscoverAgents(ctx)
		if err != nil {
			slog.Warn("registerAgentSources: source discovery failed", "source", src.Name(), "err", err)
			continue
		}
		for _, da := range discovered {
			def := da.Definition
			if def.System {
				slog.Warn("ADR-0075: registering PRIVILEGED system agent (explicit grant)",
					"source", src.Name(), "id", def.ID)
			}
			// SetAgentWithManifest carries the manifest EXTRAS (PythonDeps/MemoryLimitMB/
			// schemas); a nil manifest degrades to a record-only write.
			if err := reg.SetAgentWithManifest(def, da.Manifest); err != nil {
				slog.Warn("registerAgentSources: failed to register agent", "source", src.Name(), "id", def.ID, "err", err)
				continue
			}
			slog.Info("ADR-0075: registered agent from source", "source", src.Name(), "id", def.ID, "system", def.System, "manifest", da.Manifest != nil)
			registered = append(registered, da)
		}
	}
	return registered
}

// routePreviewAdapter implements operator.RoutePreviewer (ROUTE-07 / ADR-0077) by scoring
// the inline candidates as ONE slate through gatekeeper.ScoreSlate under the ACTIVE arm,
// then ranking them with gatekeeper.MeritRanksBefore — the same two calls the live
// Gatekeeper makes, so the preview applies the same slate-mean neutral fills
// (T-20261104-previewroute-scores-missing-latency-as-0-while) and the same ADR-0139 D6
// merit mode. It is the gatekeeper-benchmark's deterministic routing entry point — no
// planner, auction, or agents.
//
// Arms: "hand_weights" / "learned_scorer" (merit_mode legacy), "posterior_lcb", and
// "thompson_mean": under thompson the live path ranks by one random posterior draw per
// dispatch, which a deterministic preview cannot reproduce, so it reports and ranks by
// the posterior MEAN (the expected draw) and names the arm accordingly.
type routePreviewAdapter struct {
	cfg    config.ExecutionConfig
	scorer gatekeeper.RouteScorer
}

func (r routePreviewAdapter) PreviewRoute(requiredCaps []string, cands []operator.RouteCandidate) ([]domain.MeritResult, string) {
	arm := "hand_weights"
	switch r.cfg.Gatekeeper.ActiveMeritMode() {
	case config.MeritModePosteriorLCB:
		arm = config.MeritModePosteriorLCB
	case config.MeritModeThompson:
		arm = "thompson_mean"
	default:
		if r.cfg.Routing.LearnedScorer && r.scorer != nil {
			arm = "learned_scorer"
		}
	}
	entries := make([]gatekeeper.SlateEntry, len(cands))
	for i, c := range cands {
		p := previewProfileWithEvidence(c.Profile)
		entries[i] = gatekeeper.SlateEntry{Profile: &p, Trait: c.Trait}
	}
	mbs := gatekeeper.ScoreSlate(entries, requiredCaps, r.cfg, r.scorer, nil)
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := mbs[order[i]], mbs[order[j]]
		return gatekeeper.MeritRanksBefore(r.cfg, a.Score, a, b.Score, b)
	})
	out := make([]domain.MeritResult, len(cands))
	for k, i := range order {
		mb := mbs[i]
		out[k] = domain.MeritResult{
			AgentID: cands[i].Profile.AgentID, Score: mb.Score, SuccessRate: mb.SuccessRate,
			TrustScore: mb.TrustScore, LatencyTerm: mb.LatencyTerm, CostTerm: mb.CostTerm,
			Provisional: mb.Provisional, Mode: mb.Mode, Alpha: mb.Alpha, Beta: mb.Beta,
		}
	}
	return out, arm
}

// previewProfileWithEvidence gives a PreviewRoute candidate's capability stats the
// ADR-0139 D6 outcome evidence the wire cannot carry yet: RouteCandidateOp has
// success_rate and sample_count per capability but no evidence fields, so a stat
// with samples and no evidence is read as SuccessRate·n verified successes and
// (1−SuccessRate)·n failures. The legacy arm never reads the evidence, so its
// scores are unchanged. Wire fields for the evidence are a proto follow-up
// (T-20261105-previewroute-wire-carries-no-outcome-evidence).
func previewProfileWithEvidence(p domain.AgentProfile) domain.AgentProfile {
	if len(p.CapabilityStats) == 0 {
		return p
	}
	stats := make(map[string]domain.CapabilityStat, len(p.CapabilityStats))
	for k, st := range p.CapabilityStats {
		if st.SampleCount > 0 && st.Outcomes.IsZero() {
			n := float64(st.SampleCount)
			st.Outcomes = domain.OutcomeEvidence{VerifiedSuccess: st.SuccessRate * n, VerifiedFailure: (1 - st.SuccessRate) * n}
		}
		stats[k] = st
	}
	p.CapabilityStats = stats
	return p
}

// registerModelAgents persists each generator as a TraitModel AgentDefinition so it is
// an addressable agent in the registry (dispatch, scope, merit). ADR-0042: sourced from
// llm_provider.generators, agent ID = "llm:<id>" (id-keyed end to end; the server strips
// "llm:" to the generator id when Acquiring an agent-step model).
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
		// ExecPath is stored BOTH ways: the filesystem scanner records a path
		// relative to Dir, while a plugin-contributed definition carries an
		// absolute one — it has to, because the spawner runs it with
		// cmd.Dir = Dir and no other anchor.
		//
		// Joining an already-absolute path onto Dir produced a doubled path that
		// can never exist, so every plugin-contributed agent was deleted at the
		// same boot that registered it, and every later spawn failed with
		// "agent not found in database". That is what kept a second Telegram bot
		// from ever starting.
		// An empty Dir is skipped for the same reason: there is nothing to
		// resolve against, so joining can only rewrite separators — enough, on
		// Windows, to turn a path that exists into one that does not.
		fullPath := a.ExecPath
		if a.Dir != "" && !filepath.IsAbs(fullPath) {
			fullPath = filepath.Join(a.Dir, a.ExecPath)
		}
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

// pluginHandshake maps composed plugin statuses into the operator plane's own
// shape (ADR-0089). The mapping lives here, in the composition root, because the
// operator package must not learn how plugins are composed — the same separation
// that keeps the kernel from interpreting capability strings (ADR-0082 D2).
func pluginHandshake(statuses []PluginStatus) []operator.PluginInfo {
	out := make([]operator.PluginInfo, 0, len(statuses))
	for _, st := range statuses {
		info := operator.PluginInfo{
			ID:           st.Manifest.ID,
			DisplayName:  st.Manifest.DisplayName,
			Version:      st.Manifest.Version,
			State:        st.State,
			Capabilities: st.Manifest.Capabilities,
			Reason:       st.Reason,
			Missing:      st.Missing,
		}
		if st.ExpiresAt != nil {
			info.ExpiresAt = st.ExpiresAt.Format(time.RFC3339)
		}
		for _, pan := range st.Manifest.Panels {
			info.Panels = append(info.Panels, operator.PluginPanel{
				ID: pan.ID, Title: pan.Title, Capability: pan.Capability,
			})
		}
		out = append(out, info)
	}
	return out
}

// pipelineListers collects the reactive-pipeline read surfaces plugins register.
//
// A SLICE, not a slot. Ingresses come from more than one place — the Ingress
// Studio authors graphs for the ones it creates, and the reactive plane
// describes the data flow of the ones registered under ADR-0090 — and every one
// of them is an ingress. A single slot made the last registrant silently erase
// the others, which is the kind of bug that looks like "those pipelines don't
// exist" rather than like a bug.
var pipelineListers listerSet

type listerSet struct {
	mu   sync.Mutex
	all  []domain.PipelineLister
	seen bool
}

func (s *listerSet) add(l domain.PipelineLister) {
	if l == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = append(s.all, l)
	s.seen = true
}

func (s *listerSet) list() []domain.PipelineLister {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.PipelineLister(nil), s.all...)
}

// any reports whether any plugin contributes pipelines, for the capability.
func (s *listerSet) any() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// deferredPipelineLister resolves the registered listers at CALL time and
// concatenates what they return.
type deferredPipelineLister struct {
	holder *listerSet
}

func (d deferredPipelineLister) ListPipelines(ctx context.Context, armedOnly bool, ingressID string) ([]domain.PipelineSummary, error) {
	var out []domain.PipelineSummary
	for _, l := range d.holder.list() {
		got, err := l.ListPipelines(ctx, armedOnly, ingressID)
		if err != nil {
			// One contributor failing must not blank the panel: the others
			// describe real ingresses, and an operator losing sight of them
			// because an unrelated source is broken is the worse outcome.
			slog.Warn("operator: a pipeline source failed to list", "err", err)
			continue
		}
		out = append(out, got...)
	}
	return preferAuthored(out), nil
}

// ListNodeItems asks every contributor and returns the first non-empty answer.
//
// Concatenating would be wrong here, unlike ListPipelines: a node belongs to
// exactly one pipeline, so two sources answering means one of them is describing
// a pipeline it does not own. Taking the first that HAS the node keeps a derived
// source from padding an authored source's item list with nothing.
func (d deferredPipelineLister) ListNodeItems(ctx context.Context, pipelineID, nodeID string, limit int) ([]domain.NodeItem, error) {
	for _, l := range d.holder.list() {
		got, err := l.ListNodeItems(ctx, pipelineID, nodeID, limit)
		if err != nil {
			slog.Warn("operator: a pipeline source failed to list node items", "err", err)
			continue
		}
		if len(got) > 0 {
			return got, nil
		}
	}
	return nil, nil
}

// preferAuthored collapses rows that describe the same pipeline.
//
// Two kinds of source contribute. One AUTHORS pipelines and reports the revision
// it stored (1 or higher). The other DERIVES a description of an entry point that
// has no authored graph, and reports revision 0 — "nothing was authored".
//
// Both legitimately describe the same ingress during a rollout, and showing both
// is how one ingress appears twice in the console with different node counts. The
// authored row wins, because it is the one that actually runs; the derived row is
// a fallback for an ingress nobody has authored anything for yet.
func preferAuthored(rows []domain.PipelineSummary) []domain.PipelineSummary {
	best := make(map[string]domain.PipelineSummary, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		// One ENTRY POINT, one row.
		//
		// The same ingress can be described twice under two ids: a watch that
		// was migrated keeps its original id, and the chat pipeline generated
		// for the same ingress is `ingress:<id>`. They are the same entry organ
		// — one Telegram bot — and listing both makes a deployment with two bots
		// look like it has five pipelines, which is exactly how it read.
		key := entryKeyOf(r)
		prev, seen := best[key]
		if !seen {
			best[key] = r
			order = append(order, key)
			continue
		}
		if betterRow(r, prev) {
			best[key] = r
		}
	}
	out := make([]domain.PipelineSummary, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out
}

// entryKeyOf identifies the ENTRY POINT a row describes, not the row's own id.
//
// A chat pipeline is generated as `ingress:<agent>` while the watch that was
// migrated for the same agent keeps the bare `<agent>`. They are one Telegram
// bot, and keying on the pipeline id treated them as two — which is how a
// deployment with two bots listed five pipelines.
//
// Deliberately narrow: it strips only that one prefix. Keying on the trigger
// reference instead would look tidier and would be wrong, because two different
// watches on one stream are two pipelines and must both be shown.
func entryKeyOf(r domain.PipelineSummary) string {
	return strings.TrimPrefix(r.PipelineID, "ingress:")
}

// betterRow picks which of two descriptions of one entry point to show.
//
// An AUTHORED revision beats a derived one (revision 0 means nothing was
// authored). Between two authored revisions the later wins. And an `ingress`
// trigger beats a `stream` one for the same entry organ, because the chat
// pipeline is the thing that actually handles the turn — the migrated watch is
// the shape it used to have.
func betterRow(a, b domain.PipelineSummary) bool {
	if (a.Revision > 0) != (b.Revision > 0) {
		return a.Revision > 0
	}
	if a.TriggerType != b.TriggerType {
		return a.TriggerType == "ingress"
	}
	return a.Revision > b.Revision
}

// ingressPipelineRetirers removes the pipeline armed for a removed entry organ.
//
// A function rather than an interface: it is one verb, and the plugin that owns
// the pipelines is not otherwise a port.
var ingressPipelineRetirers atomic.Pointer[func(ctx context.Context, agentID string) error]

// ingressDeregistrars holds the write half of the ADR-0090 registry.
//
// Separate from ingressListers because reading who may enter and WITHDRAWING an
// entry are different powers, and a plugin that only needs to list should not
// acquire the second by asking for the first.
var ingressDeregistrars atomic.Pointer[domain.IngressDeregistrar]

// ingressSchemaDeclarers holds the schema half of the ADR-0090 registry
// (ADR-0117): a plugin that owns an entry point declares what its items carry.
var ingressSchemaDeclarers atomic.Pointer[domain.IngressSchemaDeclarer]

// ingressRegistrars holds the CREATE half of the ADR-0090 registry. Its three
// siblings were wired long before it, which is how a deployment ended up able to
// withdraw an entry point it had no way to create.
var ingressRegistrars atomic.Pointer[domain.IngressRegistrar]

// ingressListers holds the ADR-0090 ingress registry a plugin registers.
//
// One slot, unlike the pipeline listers: there is a single registry of record
// for who may enter, and two of them would be a security question, not a
// display one.
var ingressListers atomic.Pointer[domain.IngressLister]

// KernelIngressLister exposes the registered ingresses to a plugin that needs to
// describe them, resolving at CALL time so registration order does not matter.
type KernelIngressLister struct{}

func (KernelIngressLister) ListIngresses(ctx context.Context) ([]domain.IngressRegistration, error) {
	p := ingressListers.Load()
	if p == nil || *p == nil {
		return nil, nil
	}
	return (*p).ListIngresses(ctx)
}

// The per-pipeline surfaces plugins register (contracts 0088/0089/0090).
//
// SETS, not single values — the correction to a design mistake worth recording.
// These were singular on the reasoning that "a read names ONE pipeline, so a
// second contributor could only answer about a pipeline it does not own." That
// is backwards. Naming one pipeline is exactly why every source has to be asked:
// only one of them holds it, and which one is not knowable here.
//
// Two sources contribute today. The reactive engine holds migrated watches and
// generated chat graphs; the Ingress Studio holds what it generates from
// mappings. A console that could read only one of them would show a list of
// pipelines where half refuse to open.
var (
	pipelineDryRunners pipelineSourceSet[domain.PipelineDryRunner]
	pipelineAuthors    pipelineSourceSet[domain.PipelineAuthor]
	pipelineWriters    pipelineSourceSet[domain.PipelineWriter]
	pipelineLifecycles pipelineSourceSet[domain.PipelineLifecycle]
)

// pipelineSourceSet collects the sources registered for one per-pipeline surface.
type pipelineSourceSet[T any] struct {
	mu   sync.Mutex
	list []T
}

func (s *pipelineSourceSet[T]) add(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, v)
}

func (s *pipelineSourceSet[T]) all() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]T(nil), s.list...)
}

// deferredPipelineAuthor offers a read to each source until one recognises it.
type deferredPipelineAuthor struct {
	holder *pipelineSourceSet[domain.PipelineAuthor]
}

func (d deferredPipelineAuthor) GetPipeline(ctx context.Context, pipelineID string, revision int) (domain.PipelineGraph, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineGraph{
			Refused: "this build has no pipeline runtime, so there is no graph to read",
		}, nil
	}
	for _, a := range sources {
		got, err := a.GetPipeline(ctx, pipelineID, revision)
		if errors.Is(err, domain.ErrPipelineNotFound) {
			continue
		}
		if err != nil {
			// A source that is broken must not hide a source that would have
			// answered, so this is logged and the rest are still asked.
			slog.Warn("operator: a pipeline source failed to read", "err", err)
			continue
		}
		return got, nil
	}
	return domain.PipelineGraph{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

func (d deferredPipelineAuthor) ValidatePipeline(ctx context.Context, graphJSON string) (domain.PipelineValidation, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineValidation{
			Refused: "this build has no pipeline compiler, so it cannot check a graph",
		}, nil
	}
	// Validation is the COMPILER, which every source shares — the VERDICT is
	// identical from all of them. The field projection is not: only the author
	// holding the trigger's capture profile can say which fields arrive, and
	// which author that is depends on plugin registration order. So each source
	// is asked until one answers with the trigger schema RESOLVED, and the
	// first answer stands when none does.
	//
	// The preference keys on FieldsResolved, never on FieldsJSON being
	// non-empty — that check shipped a live defect TWICE: a schema-less author
	// still returns a full projection (every availability a named unknown),
	// which is non-empty JSON, so the editor's picker showed "no schema source"
	// for an ingress pipeline whenever the reactive plugin registered first.
	var fallback *domain.PipelineValidation
	for _, a := range sources {
		got, err := a.ValidatePipeline(ctx, graphJSON)
		if err != nil {
			slog.Warn("operator: a pipeline source failed to validate", "err", err)
			continue
		}
		if got.FieldsResolved {
			return got, nil
		}
		if fallback == nil {
			fallback = &got
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return domain.PipelineValidation{
		Refused: "no pipeline compiler could check this graph",
	}, nil
}

// deferredPipelineDryRunner offers a shadow run to each source in turn.
type deferredPipelineDryRunner struct {
	holder *pipelineSourceSet[domain.PipelineDryRunner]
}

func (d deferredPipelineDryRunner) DryRunPipeline(ctx context.Context, pipelineID string, revision, sampleLimit int) (domain.PipelineDryRun, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineDryRun{
			Refused: "this build has no pipeline runtime, so it cannot dry-run one",
		}, nil
	}
	for _, r := range sources {
		got, err := r.DryRunPipeline(ctx, pipelineID, revision, sampleLimit)
		if errors.Is(err, domain.ErrPipelineNotFound) {
			continue
		}
		if err != nil {
			slog.Warn("operator: a pipeline source failed to dry-run", "err", err)
			continue
		}
		return got, nil
	}
	return domain.PipelineDryRun{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

// deferredPipelineWriter routes a save to the source that already holds the id.
type deferredPipelineWriter struct {
	holder *pipelineSourceSet[domain.PipelineWriter]
}

func (d deferredPipelineWriter) SavePipeline(ctx context.Context, graphJSON string) (domain.PipelineSaved, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineSaved{
			Refused: "this build cannot author pipelines, so there is nowhere to save one",
		}, nil
	}
	// Route by ownership BEFORE writing. A read can be offered to each source
	// until one recognises the id; a save cannot, because writing to the wrong
	// store is not something the next attempt can undo — the two stores would
	// then disagree about what that pipeline is.
	if id := pipelineIDIn(graphJSON); id != "" {
		for _, w := range sources {
			h, ok := w.(domain.PipelineHolder)
			if !ok {
				continue
			}
			held, err := h.HoldsPipeline(ctx, id)
			if err != nil {
				slog.Warn("operator: a pipeline writer failed an ownership check", "err", err)
				continue
			}
			if held {
				return w.SavePipeline(ctx, graphJSON)
			}
		}
	}
	// Nobody holds it, so it is new. The first registered writer takes it.
	return sources[0].SavePipeline(ctx, graphJSON)
}

// deferredPipelineLifecycle routes a transition to the source holding the id.
type deferredPipelineLifecycle struct {
	holder *pipelineSourceSet[domain.PipelineLifecycle]
}

func (d deferredPipelineLifecycle) TransitionPipeline(ctx context.Context, pipelineID string, revision int, toState string) (domain.PipelineTransitioned, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineTransitioned{
			Refused: "this build has no pipeline registry, so nothing can transition",
		}, nil
	}
	// A transition is a WRITE: route by ownership first, exactly as saves are.
	// Arming in the wrong registry would not fail — it would create a second
	// authority over which revision is live, which is the split-brain D11
	// exists to forbid.
	for _, l := range sources {
		h, ok := l.(domain.PipelineHolder)
		if !ok {
			continue
		}
		held, err := h.HoldsPipeline(ctx, pipelineID)
		if err != nil {
			slog.Warn("operator: a pipeline lifecycle failed an ownership check", "err", err)
			continue
		}
		if held {
			return l.TransitionPipeline(ctx, pipelineID, revision, toState)
		}
	}
	return domain.PipelineTransitioned{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

// pipelineIDIn reads just the id out of a graph document.
//
// Deliberately not a full unmarshal into a pipeline type: that type lives in the
// premium module, and the composition root routes by identity without needing to
// understand the graph.
func pipelineIDIn(graphJSON string) string {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(graphJSON), &head); err != nil {
		return ""
	}
	return head.ID
}

// turnRouters holds the seam a plugin registers for shaping admitted turns.
var turnRouters atomic.Pointer[domain.TurnRouter]

// deferredTurnRouter resolves the registered router at CALL time.
//
// Always wired, rather than only when a plugin has already registered: whether
// plugin Build runs before or after the inbound service is a boot-order detail,
// and a nil read at wiring time would leave chat permanently unrouted with
// nothing to show for it.
type deferredTurnRouter struct {
	holder *atomic.Pointer[domain.TurnRouter]
}

func (d deferredTurnRouter) RouteTurn(
	ctx context.Context,
	ingressAgentID, conversationID string,
	msg domain.TurnMessage,
	run domain.TurnFunc,
) (bool, error) {
	p := d.holder.Load()
	if p == nil || *p == nil {
		// No router in this build: the turn runs directly, exactly as before.
		return false, nil
	}
	return (*p).RouteTurn(ctx, ingressAgentID, conversationID, msg, run)
}

// kgExtractorDispatcherFrom builds the kg_extractor triplet extractor from the
// execution.ingestion block. A function of its own so a test can prove the
// retry settings are read (AGENTS.md §3.4;
// T-20261102-kg-extractor-cold-start-drops-the-first-kg: a failed
// boot/dial/call is retried, bounded, before the batch fails visibly).
func kgExtractorDispatcherFrom(ing config.IngestionConfig, caller domain.AgentCaller) *subnetwork.KgExtractorDispatcher {
	return &subnetwork.KgExtractorDispatcher{
		Caller:   caller,
		AgentID:  "kg_extractor_agent",
		Attempts: ing.KgExtractorCallAttempts,
		Backoff:  time.Duration(ing.KgExtractorRetryBackoffMs) * time.Millisecond,
	}
}

// acquireKernelLLMToken mints a gateway lease for a plugin or the chat lane and, when
// agentID is set, binds it to that agent before returning it, so there is no window in
// which the lease is live but presentable by no one or anyone (ADR-0139 D8). A lease the
// gateway will not bind is completed rather than handed out. A nil gateway is a no-op,
// as before: no token, no error.
func acquireKernelLLMToken(ctx context.Context, gw *subnetwork.SubstrateLLMGateway, agentID string, tokenLimit int, ttl time.Duration) (string, func(), error) {
	if gw == nil {
		return "", func() {}, nil
	}
	tok, err := gw.Acquire(ctx, domain.StepAllocation{}, tokenLimit, ttl)
	if err != nil {
		return "", func() {}, err
	}
	// The release runs after the turn, often once ctx is cancelled, so it must outlive
	// it — WithoutCancel keeps ctx's values (scope, session, trace) where a bare
	// Background would drop them (AGENTS.md §2.7). The error is best-effort: an
	// uncompleted lease expires on its TTL.
	release := func() { _, _ = gw.Complete(context.WithoutCancel(ctx), tok) }
	if agentID != "" && !gw.BindLeaseAgent(tok, agentID) {
		release()
		return "", func() {}, fmt.Errorf("acquire agent LLM token: lease could not be bound to agent %q", agentID)
	}
	// chat.TokenAcquirer speaks the wire type; the lease is cast at this seam.
	return string(tok), release, nil
}

// secondsOr converts a seconds config value to a duration, or returns def when the
// value is not positive (the config's "0 uses the default").
func secondsOr(secs int, def time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
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
