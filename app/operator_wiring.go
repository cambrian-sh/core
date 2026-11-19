package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory/forget"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// composeOperator assembles the operator plane (OperatorConsole) from the kernel's
// surfaces and builds it with operator.New (ENG-1305): every seam is a constructor
// argument, the required ones (operator.Deps) refuse a boot that lacks one, and the
// optional ones (operator.Options) left nil are logged once. Moved out of
// startKernelServices, where each seam used to arrive by a post-construction Set*
// call; the capability list is computed from the same seams at the same points.
func composeOperator(ctx context.Context, k *Kernel, operatorFeed *operator.Spool, operatorProjection *operator.Projection) (*operator.Service, error) {
	d := operator.Deps{Feed: operatorFeed}
	var o operator.Options
	d.Projection, d.Sessions = operatorProjection, k.SessionMgr
	d.Identity = k.Authenticator // the same instance the operator interceptors use (startKernelServices' operatorIDP)
	// Contract 0102 WhoAmI: the caller's display name from the registry
	// (nil-safe: without a registry WhoAmI answers with an empty name).
	if k.Principals != nil {
		o.PrincipalNames = k.Principals
	}
	// ADR-0130 1f: failed logins become receipts when a plugin observes them.
	// Wired beside Deps.Identity, before Serve; it is observation, not a gate —
	// the per-source limiter is built inside operator.New and runs regardless.
	o.LoginObserver = k.LoginObserver
	// ADR-0047 D15/0047-24: mutating commands (audit-stamped, idempotent) over
	// the durable Postgres operator_audit store (in-memory fallback).
	d.Audit, d.Grants = k.OperatorAudit, k.ToolGrants
	// ADR-0047 D11: steering. The control hub is the rendezvous a live
	// DAGExecutor registers into (registration is the executor-producer side);
	// HITL resolution reuses the kernel ApprovalHub.
	d.Controls, d.Approvals = k.Server.ControlHub, k.Server.ApprovalHub
	// ADR-0047 0047-07/0047-16: remaining mutations. SetScope/RegisterSkill/
	// RegisterMCP/TriggerConsolidation are bound to real kernel surfaces;
	// TagMemory is gated on the 0047-20 decision (Unimplemented for now).
	d.Effects = k.OperatorEffects
	o.Sharer = k.MemorySharer // ADR-0142 D12 ShareToTeam
	// ADR-0047 Amendment A2 (CORE-OPS-1): operator-plane paged reads. The tool
	// catalog (whole registry, not a per-agent menu), the system-skill registry,
	// and ScopeSystem memory recall (operator sees all data, D13).
	d.Tools, d.Skills, d.Memory = k.Server.ToolExecutor, k.Server.SkillRegistry, k.Memory.QueryService
	// Document enumeration: the classification counterpart to memory recall.
	// Search cannot answer "which of my documents have no labels?" — that question
	// has no query text — and an unlabelled document is invisible to the policy
	// model rather than denied, so without this the console could not find the one
	// set of documents that most needs attention.
	//
	// Reads the store directly, like the operator's other reads: the operator plane
	// is ScopeSystem (D13) and sees all data. This is a listing of ids and labels,
	// never document bodies, so it discloses nothing a tag listing would not.
	o.Documents = k.Documents
	// The KEYED read (contract 0086). Distinct from the lister above, which
	// deliberately returns no bodies: this one returns a document's text by id,
	// which is what makes a citation followable and what lets a watch resolve a
	// references-only signal to content it can actually read.
	// The SAME reader the plugins use, not a second implementation. It delegates
	// to the enforcing store, so the console and a watch resolve a reference
	// through one code path with one scope model — the alternative is two
	// answers to "can this principal read this document", differing by caller.
	o.DocumentGetter = k.DocReader
	// ENG-2143: the ingest progress read (GetIngestStatus, IngestMemory, GetDocument).
	if k.Server.IngestStatus != nil {
		o.IngestStatus = k.Server.IngestStatus
	}
	// ADR-0081: the answer lane is only meaningful when the agentic retrieval
	// loop is wired (it synthesizes over multi-hop evidence). Gate on the same
	// flag; the "memory-answer" capability is advertised iff this is set.
	if k.Config.Execution.Retrieval.AgenticRetrievalEnabled {
		o.MemoryAnswerer = k.Memory.QueryService
	}
	// ADR-0047 A2.2: operator-triggered tool execution at ScopeSystem (audited,
	// idempotent). Reuses the kernel tool reference monitor with the System bypass.
	d.ToolRunner = k.Server.ToolExecutor
	// ADR-0047 A2.4: operator memory ingest requests a KERNEL document ingest —
	// the same chunking-pipeline / write-back path the agent plane and benchmarks
	// use — never a raw store write. The operator principal is stamped as Author;
	// the kernel derives classification (tags are a narrow-only hint).
	// ADR-0134 Q4: the operator plane clamps future event times with the same
	// configured tolerance the agent plane reads from ExecCfg.
	eventTimeTol := time.Duration(k.Config.Execution.Ingestion.EventTimeFutureToleranceMs) * time.Millisecond
	o.EventTimeFutureTolerance = &eventTimeTol
	// ADR-0136 A3.5 (contract 0104): the same descriptive-field caps the agent plane reads.
	mc := k.Config.Execution.Ingestion.SourceMetaCaps()
	o.SourceMetaCaps = operator.SourceMetaCaps{Kind: mc.Kind, Title: mc.Title, Author: mc.Author, Speaker: mc.Speaker,
		LabelCount: mc.LabelCount, LabelChars: mc.LabelChars}
	d.Ingestor = operatorMemoryIngestor(k.Server.IngestionProcessor)
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
		o.UploadMatcher = operator.UploadMatcherFunc(func(ctx context.Context, req operator.IngestRequest) (domain.DocumentRecord, error) {
			return um.MatchUpload(ctx, operatorIngestDoc(req))
		})
	}
	// ADR-0047 A2.6: watch CRUD is premium capability-gated. k.Server.WatchHandler
	// is nil in an OSS build (⇒ Unimplemented, WatchTriggered never publishes) and
	// the premium binary injects a real handler via Options.NewSignalReceiver.
	o.WatchHandler = k.Server.WatchHandler
	// Contract 0087: the reactive-pipeline read surface. Wired through an
	// indirection ALWAYS, rather than only when a plugin has already
	// registered, so the console does not depend on whether plugin Build ran
	// before or after this line — an ordering that is easy to change and
	// impossible to notice breaking.
	d.PipelineLister = deferredPipelineLister{holder: &pipelineListers}
	// Contract 0088: the shadow-run surface, wired through the same
	// always-on indirection and for the same reason.
	d.PipelineDryRunner = deferredPipelineDryRunner{holder: &pipelineDryRunners}
	// Contract 0089: the canvas read surface, same always-on indirection.
	d.PipelineAuthor = deferredPipelineAuthor{holder: &pipelineAuthors}
	// Contract 0090: the draft-save surface. Separate from the author for the
	// same reason the ports are separate.
	d.PipelineWriter = deferredPipelineWriter{holder: &pipelineWriters}
	d.PipelineLifecycle = deferredPipelineLifecycle{holder: &pipelineLifecycles}
	// REACT-01 / ADR-0061: the reactive dead-letter read surface reads the OSS
	// bbolt journal. Wired only when the premium reactive engine is active — the
	// same signal as watch CRUD (k.Server.WatchHandler) — since that engine is what
	// writes dead-letters; an OSS build leaves it nil → Unimplemented.
	if k.Server.WatchHandler != nil && k.WatchDeadLetters != nil {
		o.DeadLetters = k.WatchDeadLetters
	}
	// REACT-05 / ADR-0071: watch observability (metrics + backtest), from the premium
	// ReactiveEngine.
	if k.WatchMetrics != nil {
		o.WatchMetrics, o.WatchBacktester = k.WatchMetrics, k.WatchBacktester
	}
	// ROUTE-07 / ADR-0077: gatekeeper route-preview (deterministic merit scoring over
	// inline candidates, under the active scorer arm). Always available in core.
	if k.Metabolism != nil && k.Metabolism.Gatekeeper != nil {
		// ADR-0085: the access-policy administration surface (ExplainAccess,
		// ListClassificationTags). nil in OSS ⇒ those RPCs answer Unimplemented,
		// because an unscoped deployment has no policy to explain.
		o.PolicyAdmin = k.PolicyAdmin
		o.RoutePreviewer = routePreviewAdapter{
			cfg:    k.Config.Execution,
			scorer: k.Metabolism.Gatekeeper.RouteScorer,
		}
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
	o.Checkpoints, o.MCPServers, o.Embedding, o.Classifier = cpLister, mcpSource, embedSrc, classifier

	if k.TokenSeries != nil {
		o.TokenSeries = k.TokenSeries
	}

	// Contract 0076: the blast-radius preview. Wired whenever agents can be
	// listed; the estimator itself reports INCOMPLETE for anything it cannot
	// inspect rather than returning a reassuringly empty radius.
	if k.Registry != nil {
		o.BlastRadius = blastRadiusEstimator{
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
		}
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
		o.PlanProposer = planProposer{
			planner:   k.Awareness.Planner.GetExecutionPlan,
			clarifier: clarifier,
			tags: func(context.Context) []string {
				return k.Config.Execution.Router.ClassificationVocabulary
			},
		}
	}

	if k.LLMProvider != nil {
		o.Generators = generatorRegistry{
			cfg:      k.Config.LLMProvider,
			provider: k.LLMProvider,
			runtime:  k.GeneratorRuntime,
			secrets:  k.ConfigStore,
		}
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
		o.ConfigSchema = configSchemaSource{
			cfg:       k.Config,
			prov:      k.ConfigProvenance,
			live:      live,
			overrides: overrides,
		}

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
			o.ConfigWriter = writer
			o.SecretWriter = writer
			// Contract 0083: the write half of the generator surface. Same
			// store, same shadowing rules — the console's Save button now
			// has somewhere to land other than a gitignored config file.
			o.GeneratorWriter = writer

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
			o.MCPWriter = writer

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
					o.IntegrationManager = &integrationManager{
						writer:    writer,
						catalog:   cat,
						runtime:   k.MCPRuntime,
						connector: k.MCPConnector,
					}
				}
			}
		}
	}

	// ADR-0141: the credential provider's (premium accounts) audit sink
	// and plugin-plane mutation contract — the same audit store + feed and
	// command_id dedupe as every operator write.

	// Contract 0098: the identity plane's REVIEW lane (five-planes step 2,
	// FIVE-PLANES-BUILD.md). Wired outside the ConfigStore block above
	// deliberately — confirming a link persists to the links table, not to
	// the ADR-0101 config store, so a kernel with no durable config store
	// still has a review surface as long as it has a database.
	//
	// Producers below the trust ceiling (derived, scored, correlation) can
	// only ever write `candidate`. Without this binding those proposals were
	// write-only: the queue filled and nothing in the product could empty it.
	if k.linkReviewLane != nil {
		o.LinkWriter = k.linkReviewLane
	}

	// Contract 0074 SubmitPlan. Decided BEFORE the handshake below: the
	// "authored-plans" capability is derived from this seam, and deciding it after
	// the capability list (as startKernelServices did until
	// T-20261118-operator-handshake-never-advertises-authored) left the capability
	// unadvertised on every kernel although SubmitPlan was served.
	knownAgent := func(ctx context.Context, id string) bool {
		if k.Registry == nil {
			// Cannot verify ⇒ do not warn. A false "no such agent" on
			// every pin would train an operator to ignore the warning,
			// and then the real one goes unread too.
			return true
		}
		a, err := k.Registry.GetAgentByName(ctx, id)
		return err == nil && a != nil
	}
	o.PlanSubmitter = planSubmitterFor(k, knownAgent)

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
	if o.HasIntegrationManager() {
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
	if o.HasMemoryAnswerer() {
		operatorCaps = append(operatorCaps, "memory-answer")
	}
	// document-listing: ListDocuments enumerates documents by row, including the
	// unlabelled-only filter. Advertised only when a store backs it, so a console
	// never renders a document browser against a kernel that answers Unimplemented.
	if o.HasDocumentLister() {
		operatorCaps = append(operatorCaps, "document-listing")
	}
	// ingest-status (contract 0111, ENG-2143): a large upload is accepted and embedded in
	// the background; GetIngestStatus (and the status on IngestMemory / GetDocument)
	// shows its progress. Advertised only when the reader is wired.
	if o.IngestStatus != nil {
		operatorCaps = append(operatorCaps, "ingest-status")
	}
	// upload-replace (contract 0100, ADR-0138 Amendment B1): uploads are keyed by
	// name (replace / exact-duplicate no-op) and ListDocuments answers the
	// upload_name lookup. Advertised only when the lookup is wired (identity v2 +
	// a lifecycle store), so a console never asks "Replace?" of a kernel that would
	// keep both anyway.
	if o.HasUploadMatcher() {
		operatorCaps = append(operatorCaps, "upload-replace")
	}
	// Contract 0072 capabilities. Each is advertised only when a source backs
	// it, so a console never renders a surface against a kernel that answers
	// Unimplemented — and, just as importantly, can tell "this deployment has
	// none of these" apart from "this kernel cannot report them".
	operatorCaps = append(operatorCaps, o.Wave1Capabilities(d.Controls)...)
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
	if o.HasConfigSchema() {
		operatorCaps = append(operatorCaps, "config-schema")
	}
	// config-write: SetConfig / DeleteConfig / SetGeneratorKey persist DURABLY
	// (ADR-0101). Advertised separately from config-schema because a kernel can
	// report its configuration without being able to persist a change to it —
	// and a console must render a read-only form in that case rather than a
	// Save button that silently does nothing.
	if o.HasConfigWriter() {
		operatorCaps = append(operatorCaps, "config-write")
	}
	// authored-plans: SubmitPlan accepts an operator-written DAG. Without it a
	// console must hide Run/Dry-run rather than offer buttons with nowhere to go.
	if o.HasPlanSubmitter() {
		operatorCaps = append(operatorCaps, "authored-plans")
	}
	// token-series: GetTokenSeries projects hourly usage. Without it a console
	// must say "no history is projected" rather than draw a flat line, which
	// reads as zero spend.
	if o.HasTokenSeries() {
		operatorCaps = append(operatorCaps, "token-series")
	}
	if o.HasBlastRadiusEstimator() {
		operatorCaps = append(operatorCaps, "blast-radius")
	}
	// propose-plan: ProposePlan shapes work without committing to it. A console
	// gates its "nothing committed yet" header on this — without the RPC the
	// only way to see a plan is to start one.
	if o.HasPlanProposer() {
		operatorCaps = append(operatorCaps, "propose-plan")
	}
	// links (contract 0098): the identity plane's review lane —
	// ListLinkCandidates + ConfirmLink/RetractLink/RetractLinksByProducer.
	// Advertised separately from the substrate reads because a kernel can
	// ANSWER over links without being able to review them, and a console must
	// hide the queue in that case rather than draw one whose buttons return
	// Unimplemented.
	if o.HasLinkWriter() {
		operatorCaps = append(operatorCaps, "links")
	}
	// ADR-0139 D14 (contract 0101): the operator half of principal memory notes.
	// Advertised only when the service is built, so a console never renders a
	// notes screen whose RPCs answer Unimplemented.
	if k.Server != nil && k.Server.MemoryNotes != nil {
		o.MemoryNotes = k.Server.MemoryNotes
	}
	if o.HasMemoryNotes() {
		operatorCaps = append(operatorCaps, "memory-notes")
	}
	// ADR-0139 D13 (contract 0101): the skill lifecycle — proposal queue, locks,
	// ledger, rollback and the SkillOp lifecycle fields. Advertised only when
	// the Postgres lifecycle store is wired (wireSkillLifecycle).
	if k.SkillLifecycle != nil {
		o.SkillLifecycle = k.SkillLifecycle
	}
	if o.HasSkillLifecycle() {
		operatorCaps = append(operatorCaps, "skills-lifecycle")
	}
	// Per-agent skill access grants (decisions/skill-access-grants.md):
	// ListSkillGrants / GrantSkill / RevokeSkillGrant. Advertised only when the
	// 0031 grant store is wired.
	if k.SkillAccess != nil && k.SkillAccess.HasStore() {
		o.SkillGrants = k.SkillAccess
	}
	if o.HasSkillGrants() {
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
		o.PrincipalDirectory = k.PrincipalDirectory
	}
	if o.HasPrincipalDirectory() && k.ChatTurns != nil && k.Conversations != nil {
		operatorCaps = append(operatorCaps, "conversations-act-for")
	}
	// Principal-owned recurring tasks: the agent-plane gate is built (store +
	// approval hub + not disabled), and the operator sees every principal's.
	if k.Server != nil && k.Server.ScheduledTasks != nil && k.PrincipalSchedules != nil {
		o.PrincipalSchedules = k.PrincipalSchedules
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
		o.Streams, o.WatchFires, o.PlaneBudget = k.StreamRegistry, nil, nil
	}
	// Contract 0103 (T-20261103-operator-plane-cannot-inspect-facts-no-rpc): the
	// read-only fact inspection RPCs, over the fail-closed inspector NewMemoryStack
	// composed. Advertised only when the fact substrate exists.
	if k.Memory != nil && k.Memory.FactInspector != nil {
		o.FactInspector = k.Memory.FactInspector
		operatorCaps = append(operatorCaps, "facts-inspect")
	}
	// ENG-431 (additive, capability only, no contract bump): the fact extractor's
	// counters on the Snapshot, so drops, failures, refusals and partial coverage
	// are visible outside the kernel log. Advertised only when extraction runs.
	if k.Memory != nil && k.Memory.FactExtractor != nil {
		o.FactStats = k.Memory.FactExtractor
		operatorCaps = append(operatorCaps, "facts-stats")
	}
	// Contract 0113 (ENG-2139): the derivation journal's backlog, counters and dead
	// jobs, and a requeue of a dead job. Advertised only when the journal runs
	// (execution.ingestion.derivation_journal_enabled).
	if k.Memory != nil && k.Memory.Derivations != nil {
		o.Derivations = k.Memory.Derivations
		operatorCaps = append(operatorCaps, "derivation-journal")
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
	o.Forget = forgetSvc
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

	// Contract 0082: the kernel's own logs, operator-only.
	if k.Logs != nil {
		o.LogRing = k.Logs
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
	// 0108 (2026-11-18, additive; ENG-2018): ErasureReceipt.not_erasable_external
	// (agent plane) and ErasureReceiptOp.not_erasable_external (operator plane) —
	// the ADR-0137 D7 async steps that could not erase from an external service.
	// 0109 (2026-11-18, additive; ENG-2030/ENG-2032): the review lane over CLAIMS -
	// ListLinkPairs (the inbox with each pair once, already_confirmed = "extend
	// the confirmation?"), ConfirmLinkPair (one confirmation per readable open
	// candidate row), RejectLinkPair (retract them + record the claim's
	// rejection) - and LinkOp.supporting_evidence_ids. Row-level RPCs unchanged.
	// 0110 (2026-11-19, additive; ENG-2147/ENG-2154): memory notes in the compliance
	// erase - ErasureSelectorOp.memory_note_ids (single lines) and memory_note_owners
	// (a person's whole personal notes, history included), ErasureCandidateOp.note_text
	// (the line a memory-notes candidate erases), ErasureReceiptOp.redacted_messages;
	// agent plane ErasureSelector.memory_note_ids and ErasureReceipt.redacted_messages.
	// 0111 (2026-11-19, additive; ENG-2143): background embedding of large ingests -
	// GetIngestStatus + IngestStatusOp, IngestMemoryOpResponse.status and
	// GetDocumentOpResponse.ingest ("ingest-status" capability); agent plane
	// GetIngestStatus + IngestStatus and IngestMemoryResponse.document_id / status.
	// 0112 (2026-11-19, additive; ENG-2170): ingest labels - IngestMemoryOpRequest.labels
	// and agent-plane IngestMemoryRequest.labels, the writer's non-classification
	// markers (a revision's lifecycle, rev<N>, corpus/document ids) carried beside the
	// tags; the write ceiling never filters them and access policy never reads them.
	// 0113 (2026-11-19, additive; ENG-2139): the derivation journal on the operator
	// plane - GetDerivationJournal (backlog per kind, counters, dead jobs with their
	// last error; identifiers and counts only) and RequeueDerivationJob (command_id +
	// reason; NotFound when no dead job of that version exists); capability
	// "derivation-journal".
	d.KernelVersion, d.ContractVersion, d.Capabilities = KernelVersion, "0113", operatorCaps
	// ADR-0089: plugin identity rides the same handshake. Reported for EVERY
	// declared plugin, registered or not — the console needs to distinguish "this
	// deployment has no such plugin" from "it declined to register".
	o.Plugins = pluginHandshake(k.PluginStatuses)
	// ADR-0047 0047-10: chat & steer. CreateSession is wired to the
	// SessionManager; SendMessage/Inject dispatch through the Execute path is
	// the pending executor-producer side (nil hooks ⇒ Unimplemented).
	d.SessionOps = operator.SessionOpsFuncs{
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
						k.Progress.Progress(runCtx, domain.ProgressUpdate{
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
	}
	// ADR-0084 D9: the OSS chat lane on the operator plane. Wired only when the chat
	// TurnService exists (execution.chat_pool_size > 0 + a conversation store), so an
	// OSS build with chat off honestly reports SendTurn/OpenConversation as Unimplemented
	// and the "chat" capability is not advertised.
	if k.ChatTurns != nil && k.Conversations != nil {
		o.ConversationOps = &conversationOps{store: k.Conversations, turns: k.ChatTurns}
	}
	operatorSvc, err := operator.New(d, o)
	if err != nil {
		return nil, fmt.Errorf("operator plane: %w", err)
	}
	if unwired := operatorSvc.Unwired(); len(unwired) > 0 {
		slog.Info("operator plane: optional seams not wired on this kernel; their RPCs answer Unimplemented",
			"seams", unwired)
	}
	return operatorSvc, nil
}

// planSubmitterFor builds the contract 0074 SubmitPlan seam: operator-authored
// plans. The submitter hands the plan to Orchestrator.Execute via
// network.AuthoredPlanMetadataKey rather than opening a second entrypoint. On
// the planner lane that is the planner-produced plan's path (DAG executor,
// checkpoints, step dispatch); on the single-agent lane, the default, the plan
// runs as ONE brief for one agent (ENG-1377) — pins, depends_on and
// fan_out_over are guidance there, which SubmitPlan states as an advisory
// (oneBrief) — and the lane persists the run and closes the session with its
// outcome (ENG-2246). It returns an untyped
// nil (never a typed-nil planSubmitter) when the kernel has no session manager
// or server, so Options.HasPlanSubmitter and the "authored-plans" capability
// read false exactly when SubmitPlan answers Unimplemented. known reports
// whether a pinned agent exists (the pin warning).
func planSubmitterFor(k *Kernel, known func(ctx context.Context, id string) bool) operator.PlanSubmitter {
	if k.SessionMgr == nil || k.Server == nil {
		return nil
	}
	return planSubmitter{
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
		known:    known,
		oneBrief: k.Server.ExecCfg.Lane.IsSingleAgent(),
	}
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

// operatorMemoryIngestor binds the operator plane's IngestMemory to the kernel ingest
// pipeline (ADR-0047 A2.4): the upload becomes an ExternalDocument (operatorIngestDoc)
// and goes through ProcessSync — the same path the agent plane uses.
func operatorMemoryIngestor(proc subnetwork.IngestionProcessor) operator.MemoryIngestor {
	return operator.MemoryIngestorFunc(func(ctx context.Context, req operator.IngestRequest) (string, error) {
		if proc != nil {
			return proc.ProcessSync(ctx, operatorIngestDoc(req))
		}
		// No raw-store-write fallback, matching the agent plane. A direct
		// MemoryWriter.Remember here produced a structurally different row —
		// un-chunked, no source-document entity, invisible to ListDocuments — so
		// the SAME operator action wrote two different shapes of memory depending
		// on a queue-size setting. Failing is the honest outcome.
		return "", fmt.Errorf("ingestion pipeline not configured: every memory ingest " +
			"must go through the chunker, and there is no raw-store-write path")
	})
}
