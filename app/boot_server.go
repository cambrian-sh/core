package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/telemetry"
)

// buildChatLane builds the OSS chat lane (ADR-0084 D4) — the pool, the turn
// service, ingress delivery — and the conversation consent bridge (ADR-0127
// CL-2). Both stay nil when the lane is off or unmigrated.
func (b *kernelBuild) buildChatLane() {
	cfg, opts, lifecycles := b.cfg, b.opts, b.lifecycles
	reg, meta, conversations, kernelSvc, idStores, llmGateway := b.reg, b.meta, b.conversations, b.kernelSvc, b.idStores, b.llmGateway
	ownedTurns, principalDirectory, progressSink, progressOut, workerConsent := b.ownedTurns, b.principalDirectory, b.progressSink, b.progressOut, b.workerConsent
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
			// ENG-1348: a worker whose turn was abandoned is held until heartbeat
			// liveness replaced it or it reports idle (0 when liveness is off).
			RecoverTimeout: poolRecoverTimeout(cfg.Execution.Agents),
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
		// A tool call blocked on a human approval reads as "now" through
		// turnDelegations too (the hub reports the wait), so the stall watchdog pauses
		// while the person decides, bounded by the approval's own window.
		chatTurns.SetLivenessProbe(corechat.LatestActivity(progressSink.LastActivity, llmGateway.ConversationActivity, turnDelegations.ConversationActivity))
		// And the turn deadline counts only time not spent waiting on a person: an
		// approval window (schedules: 300 s) longer than chat_turn_timeout_seconds
		// must be usable from chat (T-20261118-a-chat-turn-waiting-on-a-human-approval-is).
		chatTurns.SetPersonWaitProbe(turnDelegations.PersonWait)
		// ENG-2124: a failed turn tells the person the provider failure the gateway
		// recorded on the turn's lease (credentials refused, rate-limited, unreachable)
		// in plain words, whatever the worker reported; the raw cause goes to the log.
		chatTurns.SetLeaseFaultProbe(llmGateway.LeaseFault)
		chatTurns.PersonWaitAllowance = personWaitAllowance(cfg)
		// Both bounds are operator config (execution.chat.chat_turn_timeout_seconds /
		// chat_stall_timeout_seconds); 0 keeps the package default
		// (T-20261107-chat-turn-timeout-is-hardcoded-at-240-s).
		chatTurns.TurnTimeout = secondsOr(cfg.Execution.Chat.ChatTurnTimeoutSeconds, corechat.DefaultTurnTimeout)
		chatTurns.StallTimeout = secondsOr(cfg.Execution.Chat.ChatStallTimeoutSeconds, corechat.DefaultStallTimeout)
		// ENG-2146: which turns the last stop cut off are also told over the
		// conversation's ingress (execution.chat.restart_notice_window_seconds; 0 =
		// the default, -1 = transcript only).
		chatTurns.RestartNoticeWindow = restartNoticeWindow(cfg.Execution.Chat.RestartNoticeWindowSeconds)
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
		// ENG-2148, ENG-2105: the pool starts behind the same spawn gate as the
		// plugins' boot-time daemons — once the kernel serves gRPC (lifecycles
		// start before Serve, so workers spawned at once dialled an unserved
		// backlog) and the sandbox Python env is ready (a fresh install builds
		// it for ~2 min) — and the health checker reports the chat lane not
		// ready, with the reason, until the pool is up.
		chatGate := spawnGate{Serving: b.serving.C(), PythonEnv: pythonSpawnReadinessOf(b.pyEnv)}
		chatStart := newChatPoolStarter(pool, chatGate, pythonReadinessOf(b.pyEnv), b.agentLayout)
		if b.healthChecker != nil {
			b.healthChecker.AddComponent(chatLaneComponent, chatStart.Ready)
		}
		// ENG-2124: turns the last stop cut off (a user message still unanswered, from
		// before this boot) get an interruption marker, so the person sees on reopening
		// that the turn did not complete and why. bootAt is taken before the operator
		// plane serves, so no turn of this process can be among them.
		bootAt := time.Now()
		lifecycles = append(lifecycles, Lifecycle{
			Name: "chat-interrupted-turns",
			Start: func(ctx context.Context) {
				n, err := chatTurns.MarkInterruptedTurns(ctx, bootAt)
				switch {
				case err != nil:
					slog.Warn("chat: could not mark the turns the last stop interrupted", "err", err)
				case n > 0:
					slog.Info("chat: marked turns the last stop interrupted", "count", n)
				}
			},
			Stop: func() {},
		})
		lifecycles = append(lifecycles, Lifecycle{
			Name:  "chat-pool",
			Start: chatStart.Start,
			Stop:  chatStart.Stop,
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

	b.lifecycles = lifecycles
	b.chatTurns, b.turnDelegations, b.consentBridge = chatTurns, turnDelegations, consentBridge
}

// buildServer assembles the agent-plane server (kernel.ProvideServer) — the
// consumer, not a producer — over the signal receiver, then wires its forget,
// knowledge, step-output, ingress and ingestion lanes and the evidence consumer.
func (b *kernelBuild) buildServer() error {
	ctx, cfg, opts := b.ctx, b.cfg, b.opts
	vec, reg, storeHandle, mem, aw, meta := b.vec, b.reg, b.storeHandle, b.mem, b.aw, b.meta
	watcher, llmProvider, sessionMgr, llmGateway, observer, spendMeter := b.watcher, b.llmProvider, b.sessionMgr, b.llmGateway, b.observer, b.spendMeter
	kernelSvc, signalRcv, pgSessions, forgetSvc, turnDelegations, runTracer := b.kernelSvc, b.signalRcv, b.pgSessions, b.forgetSvc, b.turnDelegations, b.runTracer
	knowledgeBridge, ontologyDrafts, hookChain, planExec, progressSink := b.knowledgeBridge, b.ontologyDrafts, b.hookChain, b.planExec, b.progressSink
	chatTurns, conversations, idStores, consentBridge, authorizer := b.chatTurns, b.conversations, b.idStores, b.consentBridge, b.authorizer
	evIngestor, eventStore, entityStore, memIngestor, eventBus := b.evIngestor, b.eventStore, b.entityStore, b.memIngestor, b.eventBus
	substrate, embedder := b.substrate, b.embedder
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
	// ADR-0070 D3: SignalStream heartbeats feed the agent manager's liveness table.
	cambrianServer.Heartbeats = meta.Manager
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

	// REQ-SDK-007c: the artifact RPCs are served by the run ledger (wireLedger,
	// ADR-0157 D5) behind the decision point.
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
		cambrianServer.IngestStatus = mem.IngestionManager // ENG-2143
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
			// The query-side section boost is composed in NewMemoryStack (ENG-1320).
			slog.Info("ADR-0060: structure-aware ingestion ENABLED (docling parse -> structure graph)")
		}
		// ADR-0105: the knowledge substrate's evidence foundation. Opt-in. Every
		// ingested document's original bytes become durable, content-addressed
		// evidence (+ outbox work item) BEFORE any write derived from them. Extraction
		// (chunking, structure parse) runs first and writes nothing, so a document
		// refused for having no usable text leaves no evidence row (ENG-2110).
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
			return fmt.Errorf("evidence consumer (ADR-0108): %w", cerr)
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

	b.cambrianServer, b.stepOutFeed, b.tokenSeries = cambrianServer, stepOutFeed, tokenSeries
	b.watchMetricsReader, b.watchBacktester = watchMetricsReader, watchBacktester
	return nil
}

// secondsOr converts a seconds config value to a duration, or returns def when the
// value is not positive (the config's "0 uses the default").
// restartNoticeWindow maps execution.chat.restart_notice_window_seconds onto
// TurnService.RestartNoticeWindow: 0 = the package default, negative = off (0).
func restartNoticeWindow(secs int) time.Duration {
	if secs < 0 {
		return 0
	}
	return secondsOr(secs, corechat.DefaultRestartNoticeWindow)
}

func secondsOr(secs int, def time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

// personWaitAllowance is how long a chat turn may wait on a person beyond its turn
// timeout (corechat.TurnService.PersonWaitAllowance): the longest window the kernel
// gives one blocking approval a turn can raise — the approvals default (dangerous
// tools), a schedule proposal, a skill access request, a machine consent — so any
// one of them can run its full window from chat
// (T-20261118-a-chat-turn-waiting-on-a-human-approval-is). Derived from the
// settings that own those windows; it adds no tunable of its own.
func personWaitAllowance(cfg *config.Config) time.Duration {
	consent := time.Duration(cfg.MCP.WorkerConsentTimeoutMs) * time.Millisecond
	if consent <= 0 {
		consent = 120 * time.Second // the hub's default consent window (domain.defaultConsentGrace)
	}
	longest := consent
	for _, w := range []time.Duration{
		cfg.Approvals.DefaultTimeout(),
		cfg.Execution.Schedules.ApprovalTimeout(),
		time.Duration(cfg.Execution.Skills.AccessRequestTimeoutSeconds) * time.Second,
	} {
		if w > longest {
			longest = w
		}
	}
	return longest
}

// poolRecoverTimeout bounds how long the chat pool holds a worker whose turn was
// abandoned (agentpool.Config.RecoverTimeout, ENG-1348), derived from the settings that
// decide how long a replacement can take: the missed-beat budget, one more beat, the
// longest restart backoff and a boot. 0 (no hold) when heartbeat liveness is off —
// nothing would ever replace the worker or report it idle.
func poolRecoverTimeout(a config.AgentsConfig) time.Duration {
	if a.DaemonHeartbeatIntervalMs <= 0 {
		return 0
	}
	missed := a.DaemonHeartbeatMissedBeforeKill
	if missed <= 0 {
		missed = 3
	}
	boot := a.AgentBootTimeoutMs
	if boot <= 0 {
		boot = 10000
	}
	ms := a.DaemonHeartbeatIntervalMs*(missed+1) + a.DaemonRestartMaxBackoffMs + boot
	return time.Duration(ms) * time.Millisecond
}
