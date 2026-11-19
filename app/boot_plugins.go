package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
	"github.com/cambrian-sh/core/internal/ontology"
	ossreactive "github.com/cambrian-sh/core/internal/reactive"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// buildPluginSeams builds what the plugin bundle hands out before the server
// exists: the default signal receiver, the late-bound holders (progress,
// schedules, streams, principals, owned turns, credentials), the ingestor and
// document reader, the evidence foundation, the consent view, and the knowledge
// and ontology draft bridges.
func (b *kernelBuild) buildPluginSeams() error {
	cfg, lifecycles := b.cfg, b.lifecycles
	vec, storeHandle, authorizer, mem, substrate := b.vec, b.storeHandle, b.authorizer, b.mem, b.substrate
	watcher, toolApproval, pyPackages, knowledgeStore, onto := b.watcher, b.toolApproval, b.pyPackages, b.knowledgeStore, b.onto
	// 9. ArtifactVault — built above, before the erasure worker (ADR-0157 D5.6).

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
			return fmt.Errorf("evidence capture (ADR-0105): %w", everr)
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
	if pyPackages != nil {
		// Stop waits for layer installs an approval started in the background.
		lifecycles = append(lifecycles, Lifecycle{Name: "python-packages", Start: func(context.Context) {}, Stop: pyPackages.Stop})
	}

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
		return fmt.Errorf("knowledge commit bridge: %w", kbErr)
	}
	// ADR-0149 D2: agents draft ontology changes; a person approves. The drafts
	// bridge registers the ontology_change handler here, at boot (an item approved
	// after a restart still applies — as its approver), audits every transition
	// through the operator audit seam, and records denials and expiries on a
	// reconcile loop. Served on the agent plane as Orchestrator/ProposeOntologyChange
	// (cambrianServer.OntologyDrafts below).
	ontologyDrafts, odErr := ontology.NewDrafts(vec.Pool(), onto, toolApproval, credSeams.recordAudit, slog.Default())
	if odErr != nil {
		return fmt.Errorf("ontology drafts: %w", odErr)
	}
	lifecycles = append(lifecycles, Lifecycle{Name: "ontology-drafts", Start: ontologyDrafts.Start, Stop: ontologyDrafts.Stop})

	b.lifecycles = lifecycles
	b.signalRcv, b.progressSink, b.progressOut = signalRcv, progressSink, progressOut
	b.principalSchedules, b.streamRegistry, b.principalDirectory = principalSchedules, streamRegistry, principalDirectory
	b.ownedTurns, b.credSeams, b.memIngestor, b.docReader = ownedTurns, credSeams, memIngestor, docReader
	b.evIngestor, b.evStore, b.eventStore, b.entityStore = evIngestor, evStore, eventStore, entityStore
	b.linkStore, b.identityScans, b.queryPlane, b.workerConsent = linkStore, identityScans, queryPlane, workerConsent
	b.knowledgeBridge, b.ontologyDrafts = knowledgeBridge, ontologyDrafts
	return nil
}

// buildKernelServices assembles the capability bundle handed to plugins. Built
// once so the ADR-0082 D12 Build phase and the ADR-0057 NewSignalReceiver hook
// see exactly the same services.
func (b *kernelBuild) buildKernelServices() {
	cfg, opts := b.cfg, b.opts
	vec, reg, authorizer, idStores, ingressTraffic := b.vec, b.reg, b.authorizer, b.idStores, b.ingressTraffic
	knowledgeStore, onto, ontologies, substrate := b.knowledgeStore, b.onto, b.ontologies, b.substrate
	meta, aw, llmProvider, llmGateway, eventBus := b.meta, b.aw, b.llmProvider, b.llmGateway, b.eventBus
	sessionMgr, toolApproval, planExec := b.sessionMgr, b.toolApproval, b.planExec
	progressSink, progressOut, workerConsent, memIngestor, docReader := b.progressSink, b.progressOut, b.workerConsent, b.memIngestor, b.docReader
	principalSchedules, streamRegistry, principalDirectory, ownedTurns := b.principalSchedules, b.streamRegistry, b.principalDirectory, b.ownedTurns
	eventStore, entityStore, linkStore, identityScans, queryPlane := b.eventStore, b.entityStore, b.linkStore, b.identityScans, b.queryPlane
	ontologyDrafts, evStore, evIngestor, credSeams := b.ontologyDrafts, b.evStore, b.evIngestor, b.credSeams
	kernelSvc := KernelServices{
		Principals: idStores.registry,
		// ENG-2135: closed right before GRPC.Serve; boot-time daemon spawns wait
		// on it through AwaitSpawnReady.
		Serving: b.serving.C(),
		// ENG-2137: and, after it, the sandbox Python env's readiness, so the
		// same gate holds a Python daemon spawn until the env can launch it.
		PythonEnv: pythonSpawnReadinessOf(b.pyEnv),
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
		TracePipelinePayloads:     cfg.Execution.LLM.TracePipelinePayloads,
		PipelineDrainerEnabled:    cfg.Execution.Pipelines.DrainerEnabled,
		PipelineDrainerSliceTasks: cfg.Execution.Pipelines.DrainerSliceTasks,
		PipelineDrainerWorkers:    cfg.Execution.Pipelines.DrainerWorkers,
		Events:                    eventStore,
		Entities:                  entityStore,
		Links:                     linkStore,
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

	b.kernelSvc = kernelSvc
}

// runPluginBuild runs the plugins' Build phase over the bundle and collects the
// capabilities they could only decide once built.
func (b *kernelBuild) runPluginBuild() error {
	composed, kernelSvc := &b.composed, b.kernelSvc
	// ADR-0082 D12: the Build phase — plugins construct their runtime objects now that the
	// stacks exist and before anything is served. Runs in dependency order; a plugin that
	// implements no Builder is skipped.
	if err := buildPlugins(composed.built, kernelSvc); err != nil {
		return err
	}
	// REC-02: capabilities a plugin could only decide once it saw the deployment.
	// Collected AFTER every Build, so a plugin reports what it actually got rather
	// than what its build could in principle contribute — the record lane used to
	// advertise itself on a kernel with no Postgres, which made "no record lane
	// here" indistinguishable from "this kernel is broken".
	composed.capabilities = append(composed.capabilities, liveCapabilities(composed.built)...)
	return nil
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
