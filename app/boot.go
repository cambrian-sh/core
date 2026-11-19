package app

import (
	"context"
	"net"
	"net/http"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/memorynotes"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
	skillaccess "github.com/cambrian-sh/core/internal/skill/access"
	skilllifecycle "github.com/cambrian-sh/core/internal/skill/lifecycle"
	"github.com/cambrian-sh/core/internal/storage"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
	"github.com/cambrian-sh/core/internal/telemetry"
	"github.com/cambrian-sh/core/pkg/util"
)

// kernelBuild carries the state bootstrapKernel's phases share (ENG-1123: the
// former 2,250-line bootstrapKernel, split along its stack seams with no change
// in wiring order). Each phase is a method that reads the fields it needs into
// locals of the same name at its top, runs the original wiring verbatim, and
// stores what later phases use at its bottom — so a phase's inputs and outputs
// are the first and last lines of its body. The phases run in exactly the order
// the single function ran them; testdata/boot_graph pins the assembled graph.
type kernelBuild struct {
	ctx      context.Context
	cfg      *config.Config
	opts     Options
	cfgStore *storage.BoltConfigStore
	composed composedPlugins
	// lifecycles accumulates background components in registration order
	// (ADR-0074); several phases append to it.
	lifecycles []Lifecycle

	// infrastructure
	storeHandle    *kernel.StorageHandle
	reg            *kernel.AgentRepoDecorator
	vec            *postgres.PgVectorAdapter
	healthChecker  *health.Checker
	authorizer     domain.Authorizer
	operatorAudit  domain.AuditStore
	conversations  domain.ConversationStore
	ingressTraffic domain.IngressTrafficLister
	idStores       identityStores
	hookChain      *domain.HookChain
	llmProvider    *llm.Provider
	embedder       *llm.OllamaEmbedder
	observer       domain.TelemetryObserver
	runTracer      domain.RunTracer
	memoryGen      domain.Generator
	factsGen       domain.Generator
	awarenessGen   domain.Generator
	supervisionGen domain.Generator
	// agent registry
	profiles []profileAgent
	// substrate (ontology, stores, fact substrate)
	ontologies     Ontology
	onto           *ontology.Ontology
	substrate      *Substrate
	knowledgeStore *postgres.PgKnowledgeStore
	docLifecycle   *postgres.PgDocumentLifecycle
	factsSubstrate kernel.FactSubstrate
	// memory stack
	mem           *kernel.MemoryStack
	forgetSvc     *forget.Service
	memoryNotes   *memorynotes.Service
	artifactVault *vault.ArtifactVault
	// awareness, metabolism, supervision
	aw           *kernel.AwarenessStack
	calendar     *domain.Calendar
	spendMeter   *llm.SpendMeter
	llmGateway   *subnetwork.SubstrateLLMGateway
	meta         *kernel.MetabolismStack
	pyEnv        pythonEnvGate
	agentLayout  error // checkAgentLayout's finding at boot; nil when complete
	sup          *kernel.SupervisionStack
	eventBus     *domain.InMemoryEventBus
	agentSockets *agentSocketServer
	// serving is closed right before GRPC.Serve (ENG-2135); plugins see it as
	// KernelServices.Serving.
	serving *servingSignal
	// approvals, watcher, sessions
	toolApproval    *domain.AddressedHub
	pyPackages      *pypackages.Service
	planExec        *kernelPlanExecutor
	watcher         *supwatcher.Watcher
	pgSessions      *postgres.PgSessionStore
	sessionMgr      *session.SessionManager
	circadianRhythm *circadian.CircadianRhythm
	// plugin seams
	signalRcv          domain.SignalReceiver
	progressSink       *progressHolder
	progressOut        *progressDelivererHolder
	principalSchedules *principalSchedulesHolder
	streamRegistry     *streamRegistryHolder
	principalDirectory *principalDirectoryHolder
	ownedTurns         *ownedTurnRunner
	credSeams          *credentialSeams
	memIngestor        *kernelMemoryIngestor
	docReader          *kernelDocumentReader
	evIngestor         *evidence.Ingestor
	evStore            *postgres.PgEvidenceStore
	eventStore         *postgres.PgEventStore
	entityStore        *postgres.PgEntityStore
	linkStore          *postgres.PgLinkStore
	identityScans      *postgres.PgIdentityScanSource
	queryPlane         *postgres.PgQueryPlane
	workerConsent      domain.WorkerConsentHub
	knowledgeBridge    *knowledgecommit.Service
	ontologyDrafts     *ontology.Drafts
	kernelSvc          KernelServices
	// chat lane
	chatTurns       *corechat.TurnService
	turnDelegations *subnetwork.TurnDelegations
	consentBridge   *ingress.ConsentBridge
	// agent-plane server
	cambrianServer     *subnetwork.Server
	watchMetricsReader domain.WatchMetricsReader
	watchBacktester    domain.WatchBacktester
	stepOutFeed        *stepOutputFeed
	tokenSeries        *telemetry.TokenSeries
	// MCP endpoint and connector
	toolReg         *domain.InMemoryToolRegistry
	mcpCoreBackends *mcpserve.CoreBackends
	workerHub       *domain.WorkerHub
	mcpTaskLane     *mcpTaskLane
	mcpEndpoint     *http.Server
	mcpConnector    *mcp.Connector
	mcpRT           *mcpRuntime
	sbPolicy        *sandbox.Policy
	egressProxy     *egressproxy.Broker
	mcpServers      []mcp.ServerConfig
	mcpHandler      domain.ToolHandler
	mcpPricing      domain.ToolPricingSource
	mcpBudget       *domain.BudgetLedger
	mcpAuditor      domain.EgressAuditor
	// tool executor and catalogs
	toolGrants     *domain.LayeredGrantsStore
	ledgerSweeper  *ledger.Sweeper
	execLeases     executorLeases
	profileGrants  *profileGrantSync
	skillReg       *domain.InMemorySkillRegistry
	skillIndexer   *domain.SkillIndexer
	skillAccess    *skillaccess.Service
	skillLifecycle *skilllifecycle.Service
	skillCurator   *skilllifecycle.Curator
	mcpSink        mcp.ToolSink
	// operator surface
	operatorEffects operator.CommandEffectsFuncs
	authenticator   domain.Authenticator
	sharer          *memorySharer
}

// cfgStore is the ADR-0101 config store opened by Run (nil when it could not be
// opened, and in tests): the MCP connector and the outbound-credential provider
// are constructed over it here, before the boot connect.
//
// The phases below run in the order the wiring always ran; each lives in the
// file of its seam (boot_infrastructure.go, boot_memory.go, boot_metabolism.go,
// boot_plugins.go, boot_server.go, boot_tools.go, boot_operator.go).
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
	b := &kernelBuild{ctx: ctx, cfg: cfg, opts: opts, cfgStore: cfgStore, composed: composed, lifecycles: lifecycles, serving: newServingSignal()}

	// 1. Infrastructure — storage, vector DB, LLM; then the agent registry.
	if err := b.buildInfrastructure(); err != nil {
		return nil, err
	}
	b.registerAgents()
	// The substrate and the memory stack (ADR-0106/0110/0136/0149).
	if err := b.buildSubstrate(); err != nil {
		return nil, err
	}
	b.buildMemoryStack()
	// 2. Domain stacks — awareness, metabolism, supervision, and their wiring.
	if err := b.buildMetabolism(); err != nil {
		return nil, err
	}
	// 4. Watcher and 5. SessionManager.
	if err := b.buildApprovalsAndSessions(); err != nil {
		return nil, err
	}
	logFactLayerBoot(b.cfg, b.mem)
	composeLateRecall(b.cfg, b.mem, b.meta, b.llmGateway, b.llmProvider)
	b.wireSessionLifecycle()
	wirePlannerRouting(b.cfg, b.aw, b.sup, b.meta)
	// 10. Server assembly, preceded by the plugin seam (ADR-0082 D12 Build).
	if err := b.buildPluginSeams(); err != nil {
		return nil, err
	}
	b.buildKernelServices()
	if err := b.runPluginBuild(); err != nil {
		return nil, err
	}
	b.buildChatLane()
	if err := b.buildServer(); err != nil {
		return nil, err
	}
	// The tool plane: the inbound MCP endpoint, the MCP connector, the tool
	// executor, then the tool and skill catalogs.
	if err := b.buildMCPEndpoint(); err != nil {
		return nil, err
	}
	if err := b.buildMCPConnector(); err != nil {
		return nil, err
	}
	if err := b.buildToolExecutor(); err != nil {
		return nil, err
	}
	b.buildCatalogs()
	// The operator surface: command effects, the authenticator, the publish effect.
	if err := b.buildOperatorSurface(); err != nil {
		return nil, err
	}
	return b.kernel(lis), nil
}

// kernel assembles the Kernel from the finished phases.
func (b *kernelBuild) kernel(lis net.Listener) *Kernel {
	cfg, opts, composed, lifecycles := b.cfg, b.opts, b.composed, b.lifecycles
	agentSockets, linkStore, substrate, operatorEffects, sharer := b.agentSockets, b.linkStore, b.substrate, b.operatorEffects, b.sharer
	operatorAudit, vec, docReader, storeHandle, tokenSeries := b.operatorAudit, b.vec, b.docReader, b.storeHandle, b.tokenSeries
	progressSink, stepOutFeed, llmProvider, reg := b.progressSink, b.stepOutFeed, b.llmProvider, b.reg
	watchMetricsReader, watchBacktester, conversations, chatTurns := b.watchMetricsReader, b.watchBacktester, b.conversations, b.chatTurns
	principalDirectory, idStores, principalSchedules, streamRegistry := b.principalDirectory, b.idStores, b.principalSchedules, b.streamRegistry
	authenticator, healthChecker, mem, aw, meta, pyEnv, sup := b.authenticator, b.healthChecker, b.mem, b.aw, b.meta, b.pyEnv, b.sup
	cambrianServer, sessionMgr, circadianRhythm := b.cambrianServer, b.sessionMgr, b.circadianRhythm
	skillLifecycle, skillCurator, ledgerSweeper, skillAccess := b.skillLifecycle, b.skillCurator, b.ledgerSweeper, b.skillAccess
	artifactVault, eventBus, authorizer, toolGrants := b.artifactVault, b.eventBus, b.authorizer, b.toolGrants
	mcpConnector, mcpSink, mcpServers, mcpRT, credSeams := b.mcpConnector, b.mcpSink, b.mcpServers, b.mcpRT, b.credSeams
	egressProxy := b.egressProxy
	mcpEndpoint, mcpTaskLane, mcpCoreBackends, llmGateway := b.mcpEndpoint, b.mcpTaskLane, b.mcpCoreBackends, b.llmGateway
	return &Kernel{
		agentSockets:    agentSockets,
		serving:         b.serving,
		Links:           linkStore,
		linkReviewLane:  substrate.LinkReview(),
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
		PythonEnv:           pyEnv,
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
		EgressProxy:         egressProxy,
		MCPSink:             mcpSink,
		MCPServers:          mcpServers,
		MCPRuntime:          mcpRT,
		credentialSeams:     credSeams,
		MCPEndpoint:         mcpEndpoint,
		mcpTasks:            mcpTaskLane,
		MCPCoreBackends:     mcpCoreBackends,
		GeneratorRuntime:    newGeneratorRuntime(cfg.LLMProvider),
		LLMGateway:          llmGateway,
	}
}
