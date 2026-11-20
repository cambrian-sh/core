package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
	skilllifecycle "github.com/cambrian-sh/core/internal/skill/lifecycle"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/supervision"

	"google.golang.org/grpc/metadata"
)

// buildMCPEndpoint discovers the native tools, binds the inbound MCP endpoint's
// core-tool backends, builds the worker hub and the MCP task lane, wires the
// server's notes and principal resolution, and starts the endpoint (ADR-0126)
// — immediately before the MCP connector's ConnectAll, on purpose.
func (b *kernelBuild) buildMCPEndpoint() error {
	cfg, opts, composed := b.cfg, b.opts, b.composed
	vec, mem, docReader, sessionMgr, authorizer := b.vec, b.mem, b.docReader, b.sessionMgr, b.authorizer
	cambrianServer, conversations, idStores, calendar := b.cambrianServer, b.conversations, b.idStores, b.calendar
	// ADR-0039: the kernel-owned tool registry. It holds the MCP servers'
	// tools (registered after ConnectAll below); the executor authorizes every
	// call (grant + resource policy + scope + approval). Default: no grants ⇒ no
	// tools (fail-closed). The native tools/*tool.py discovery and its confined
	// Python process handler were deleted (ENG-1421): none ever shipped, and the
	// owner directive decisions/agent-stack-v2-mcp-tools-and-profiles.md allows
	// MCP tools only.
	toolReg := domain.NewInMemoryToolRegistry()

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
	cambrianServer.MemoryNotes = b.memoryNotes // built with the memory stack (the forget service reaches it)
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
		return err
	}

	b.toolReg, b.mcpCoreBackends, b.workerHub = toolReg, mcpCoreBackends, workerHub
	b.mcpTaskLane, b.mcpEndpoint = mcpTaskLane, mcpEndpoint
	return nil
}

// buildMCPConnector builds the outbound credential source and the MCP
// connector (ADR-0043, contract 0097), assembles the plugin and configured
// server lists and the live runtime list, connects the enabled servers into the
// tool registry, and builds the MCP handler, pricing, budget and auditor.
func (b *kernelBuild) buildMCPConnector() error {
	ctx, cfg, opts, cfgStore, composed := b.ctx, b.cfg, b.opts, b.cfgStore, b.composed
	credSeams, eventBus, toolReg := b.credSeams, b.eventBus, b.toolReg
	// The integration catalog is read first so a deployment catalog that does
	// not parse refuses boot before any MCP machinery is built (ENG-2121).
	sbCatalog, err := loadSandboxCatalog(cfg.MCP.CatalogFile)
	if err != nil {
		return err
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
	mcpConnector = mcp.NewConnector(mcpCredentialSources(cfg, cfgStore, credSource), mcpConnectorOptions(cfg, cfgStore)...)
	// Integrations: every health change of every server goes onto the operator
	// feed (OperatorEvent.integration_health). Publish is non-blocking.
	// A health transition is a kernel-owned observation of the server (the
	// connector's own handshake and restart loops), not part of any request, so
	// there is no caller ctx to carry (AGENTS.md §2.7).
	mcpConnector.SetHealthObserver(func(h mcp.ServerHealth) {
		_ = eventBus.Publish(context.Background(), domain.MCPServerHealthEvent{
			ServerID: h.ServerID, State: h.State, LastHandshake: h.LastHandshake,
			ToolCount: h.ToolCount, LastError: h.LastError, Restarts: h.Restarts, ChangedAt: h.ChangedAt,
		})
	})
	mcpPricingMap := domain.MapPricingSource{}
	// ADR-0139 D8: one trust-tier containment policy (mcp.sandbox + the
	// integration catalog's tiers), resolved onto every server built below.
	//
	// ENG-688: the egress allowlist proxy is built and started here and
	// injected into that policy; every launch that enforces declared hosts
	// opens its session on it. It lives as long as the kernel (WithoutCancel:
	// the boot ctx is not its lifetime), and Kernel.Shutdown stops it.
	egressProxy := egressproxy.New(egressproxy.Options{})
	if err := egressProxy.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("ENG-688: egress proxy: %w", err)
	}
	sbPolicy := sandboxPolicyFromConfig(cfg.MCP.Sandbox, sbCatalog, egressProxy)
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

	b.mcpConnector, b.mcpRT, b.sbPolicy, b.mcpServers = mcpConnector, mcpRT, sbPolicy, mcpServers
	b.egressProxy = egressProxy
	b.mcpHandler, b.mcpPricing, b.mcpBudget, b.mcpAuditor = mcpHandler, mcpPricing, mcpBudget, mcpAuditor
	return nil
}

// buildToolExecutor builds the tool grants and the tool executor — the
// reference monitor, its controls constructor-injected (AGENTS.md §2.3) — and
// threads the run ledger, effect checks, evidence pipeline, calculation and
// forget tools, chat grounding, approvals and profile grants through it.
func (b *kernelBuild) buildToolExecutor() error {
	ctx, cfg := b.ctx, b.cfg
	vec, reg, storeHandle, mem, aw, meta, authorizer := b.vec, b.reg, b.storeHandle, b.mem, b.aw, b.meta, b.authorizer
	toolApproval, cambrianServer, toolReg, profiles := b.toolApproval, b.cambrianServer, b.toolReg, b.profiles
	mcpHandler, workerHub, workerConsent := b.mcpHandler, b.workerHub, b.workerConsent
	mcpAuditor, mcpBudget, mcpPricing, hookChain, artifactVault := b.mcpAuditor, b.mcpBudget, b.mcpPricing, b.hookChain, b.artifactVault
	turnDelegations, eventBus, forgetSvc, chatTurns, calendar := b.turnDelegations, b.eventBus, b.forgetSvc, b.chatTurns, b.calendar
	runTracer, principalSchedules := b.runTracer, b.principalSchedules
	// ENG-1116: operator grants survive a restart (Postgres 0052 tool_grants;
	// in memory only without a database); profile allowlist grants are an
	// in-memory layer re-derived below on every boot.
	toolGrants, err := buildToolGrants(ctx, vec.Pool())
	if err != nil {
		return err
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
		return fmt.Errorf("tool executor: %w", err)
	}
	// ENG-117 (ADR-0043 D4): the MCP response injection scan, read from
	// execution.tools.output_scan (default on); a constructor control too.
	outputGuard, err := untrustedOutputGuard(cfg.Execution.Tools.OutputScan)
	if err != nil {
		return fmt.Errorf("tool executor: %w", err)
	}
	// ENG-531: the Rule-of-Two gate, read from execution.tools.rule_of_two
	// (default on) and handed to the constructor like every other control.
	ruleOfTwo, err := ruleOfTwoGate(cfg.Execution.Tools.RuleOfTwo)
	if err != nil {
		return fmt.Errorf("tool executor: %w", err)
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
		// ENG-117: every MCP response is scanned; a hit is flagged or withheld.
		UntrustedOutput: outputGuard,
		// ENG-531: tainted ∧ sensitive ∧ write/egress calls go to approval.
		RuleOfTwo: ruleOfTwo,
		// ENG-2232: free-query (raw SQL) tools need a grant naming them exactly,
		// unless the operator lifted the guard.
		FreeQueryOpen: !cfg.Execution.Tools.FreeQuery.RequireExplicitGrant,
		// ENG-1634: the record of the side-effecting calls each chat turn begins,
		// keyed turn-scoped in the durable action record; the chat lane reads it to
		// re-drive a killed turn only when it began none (SetTurnCalls below).
		TurnCalls: domain.NewTurnCalls(domain.NewDurableIdempotencyCache(idempotencyTTL, actionRecords)),
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
		// No default Handler: every registered tool is an MCP tool (ADR-0043) or
		// a contributed local:<machine>/<tool> step (ADR-0127). The native
		// tool.py process handler and its CAMBRIAN_WEB_* env passthrough are gone
		// (ENG-1421); any other identity is refused "no tool handler configured".
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
		// ADR-0049 D1: mutations become action records. (The ADR-0048 D6 read-output
		// promotion and its 200-byte floor were deleted 2026-11-05 with the tier-2 judge.)
		ToolOutput: mem.Agent,
		// ADR-0139 D11: pre_tool after the policy decision (narrow only; a rewrite
		// is re-authorized), post_tool around the output.
		Hooks: hookChain,
		// The conversation's record of side-effecting calls: every approval the
		// executor raises names an earlier call it may duplicate
		// (T-20261107-approver-is-not-told-that-an-identical-side). nil when the
		// window is 0 (disabled); persisted across restarts (ENG-784).
		History: buildActionHistory(ctx, cfg.Execution.Tools.DuplicateNoticeWindowMinutes, vec.Pool()),
	})
	if err != nil {
		return fmt.Errorf("tool executor: %w", err)
	}
	cambrianServer.ToolExecutor = toolExecutor
	// ENG-1634: the chat lane (built before the tool plane) tracks its turns'
	// leases in the executor's record and re-drives a killed turn from it.
	if b.chatTurns != nil {
		b.chatTurns.SetTurnCalls(toolExecutor.TurnCalls())
	}
	// ADR-0145 D1: the run ledger (execution.ledger, off by default): every tool
	// result and step output recorded, scope-enforced, read by reference. Threaded
	// to the executor, the server and the turn end here; nil sweeper when off.
	ledgerSweeper := wireLedger(ctx, cfg.Execution.Ledger, cfg.Execution.Content, vec.Pool(), artifactVault, reg, authorizer,
		cfg.Privacy.PrivateByDefaultOn(), cambrianServer, turnDelegations, eventBus, outputGuard)
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
	// ENG-2117: company_records, the read-only look-up of the company's typed records
	// (execution.tools.company_records, default on), over the substrate's query plane.
	wireCompanyRecords(cfg.Execution.Tools.CompanyRecords, cambrianServer, b.substrate)
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
	// T-20261118-a-sync-approval-orphaned-by-a-kernel-restart: an approval
	// approved after the kernel run that raised it ended is carried out by these
	// (once, under the person's principal and the rules in force then), or
	// refused with the reason. Registered before the transport opens.
	wireApprovalRecovery(toolApproval, toolExecutor, cambrianServer.ScheduledTasks, func() domain.PrincipalDirectory {
		return principalDirectoryOrRegistry(b.principalDirectory.get(), b.idStores.registry)
	})
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
	profileGrants := newProfileGrantSync(toolGrants.Profile(), toolReg.All, profiles,
		cfg.Execution.Tools.FreeQuery.RequireExplicitGrant) // ENG-2232: globs never grant free SQL
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

	b.toolGrants, b.ledgerSweeper, b.execLeases, b.profileGrants = toolGrants, ledgerSweeper, execLeases, profileGrants
	return nil
}

// buildCatalogs indexes the tools (ADR-0044) and the system skills (ADR-0046)
// for retrieval, swaps in the kg_extractor, wires skill access, composed
// workers, subtasks and the skill lifecycle, and builds the MCP tool sink.
func (b *kernelBuild) buildCatalogs() {
	ctx, cfg := b.ctx, b.cfg
	vec, reg, mem, meta, embedder, toolReg, profiles := b.vec, b.reg, b.mem, b.meta, b.embedder, b.toolReg, b.profiles
	cambrianServer, toolApproval, execLeases, mcpConnector, profileGrants := b.cambrianServer, b.toolApproval, b.execLeases, b.mcpConnector, b.profileGrants
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
	// "skills" is resolved against the process working directory: running from cambrian-core finds cambrian-core/skills (the shipped
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

	b.skillReg, b.skillIndexer, b.skillAccess = skillReg, skillIndexer, skillAccess
	b.skillLifecycle, b.skillCurator, b.mcpSink = skillLifecycle, skillCurator, mcpSink
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
