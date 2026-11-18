package app

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memory/vault"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// buildSubstrate validates the knowledge kind and link-verb registries, loads
// the live ontology over them (ADR-0149 D2), and composes the substrate's stores
// and query plane (app.NewSubstrate) and the kernel fact substrate. It runs
// before the memory stack because the kernel's own fact layer lives on the
// substrate (ADR-0136 A1.6).
func (b *kernelBuild) buildSubstrate() error {
	ctx, cfg, opts := b.ctx, b.cfg, b.opts
	vec, authorizer, llmProvider := b.vec, b.authorizer, b.llmProvider
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
		return fmt.Errorf("knowledge kind registry (ADR-0110): %w", kerr)
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
		return fmt.Errorf("link relation registry (five-planes step 2; deployment verbs are declared "+
			"in the kernel config's \"relations\" section, e.g. configs/config.json): %w", rerr)
	}
	// ADR-0149 D2: the two declarers above are the BOOT declarations (the base,
	// version 1 on a fresh database); the live ontology seeds them into
	// ontology_declarations and replays every namespace's applied changes over
	// them. From here on the stores ask it for a namespace's current registries
	// per write, so an operator's change takes effect without a restart.
	ontologies, oerr := LoadOntology(ctx, vec.Pool(), domain.OntologyDoc{Kinds: kindSpecs, Relations: relSpecs}, authorities, authorizer)
	if oerr != nil {
		return fmt.Errorf("ontology declarations (ADR-0149): %w", oerr)
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

	b.ontologies, b.onto, b.substrate, b.knowledgeStore = ontologies, onto, substrate, knowledgeStore
	b.docLifecycle, b.factsSubstrate = docLifecycle, factsSubstrate
	return nil
}

// buildMemoryStack composes the memory stack (kernel.NewMemoryStack, the one
// composition point, AGENTS.md §2.1) with its erasure store, the forget service
// every forget door shares, the artifact vault and the erasure worker.
func (b *kernelBuild) buildMemoryStack() {
	cfg, opts, lifecycles := b.cfg, b.opts, b.lifecycles
	vec, storeHandle, docLifecycle, authorizer := b.vec, b.storeHandle, b.docLifecycle, b.authorizer
	memoryGen, factsGen, embedder, factsSubstrate := b.memoryGen, b.factsGen, b.embedder, b.factsSubstrate
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
	// ADR-0137 D7: the async tail's steps beyond the CAS/vault deletes — the plugins'
	// (opts.ErasureTailSteps, ENG-1966) and core's own, through the same seam:
	// ENG-1967 evicts the step cache (no entry records its session or source records,
	// so the whole memo goes; it is TTL-bounded and recomputed on a miss).
	erasureTails := coreErasureTailSteps(storeHandle, opts.ErasureTailSteps)
	erasureStore := postgres.NewPgErasureStore(docLifecycle,
		postgres.WithErasureAsyncDeadline(cfg.Execution.Erasure.AsyncDeadline()),
		postgres.WithErasureParticipants(opts.ErasureParticipants...),
		postgres.WithErasureTailSteps(erasureTailStepNames(erasureTails)...),
		postgres.WithErasureObserver(opts.ErasureObserver))
	memBackend.Eraser = erasureStore
	// ENG-1320: every config-and-store recall lane is composed inside NewMemoryStack;
	// app.go hands over the plugin hooks and the embedder identity only.
	mem := kernel.NewMemoryStack(memBackend, memoryGen, embedder, cfg.Execution, authorizer, cfg.Chunker, factsGen, factsSubstrate,
		kernel.RetrievalDeps{
			DecisionObserver:    opts.DecisionObserver,
			SubstrateConsultant: opts.SubstrateConsultant,
			EmbedModel:          cfg.Embedder.Model,
		})
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
	// ADR-0139 D14: the principal memory-notes service (one for both planes), built
	// here because the forget service reaches it (ENG-2106): a forget of a person's
	// fact must reach the notes a chat agent saved it in.
	memoryNotes := buildMemoryNotes(b.ctx, cfg.Execution.MemoryNotes, vec.Pool(), authorizer)
	var forgetNotes forget.Notes
	if memoryNotes != nil {
		forgetNotes = memoryNotes
	}
	forgetSvc := forget.New(mem.Eraser(), forget.Config{
		PreviewTTL: cfg.Execution.Erasure.PreviewTTL(),
		PolicyFingerprint: fmt.Sprintf("agent_erase=%t;max_targets=%d",
			cfg.Execution.Erasure.AgentEraseEnabled, cfg.Execution.Erasure.MaxTargets()),
		Searcher:      mem.QueryService,
		SnippetChars:  cfg.Execution.Erasure.PreviewSnippet(),
		MinCosine:     cfg.Execution.Erasure.PreviewMinCos(),
		MaxCandidates: cfg.Execution.Erasure.PreviewMaxCands(),
		Notes:         forgetNotes,
	})
	if cfg.Execution.Erasure.Enabled && cfg.Execution.Erasure.AgentEraseEnabled {
		slog.Info("ENG-1953: forget query preview bounds",
			"min_cosine", cfg.Execution.Erasure.PreviewMinCos(),
			"max_candidates", cfg.Execution.Erasure.PreviewMaxCands())
	}
	// ADR-0137 S5: the one drainer of the erasure outbox (CAS deletes of erased evidence
	// and source-document bodies; request completion; tombstone retention). Started only
	// with erasure on — off, nothing writes the outbox.
	// The artifact vault (content-addressed bytes of run-ledger entries and
	// artifacts): the erasure worker deletes erased ledger entries' bodies from it
	// (ADR-0157 D5.6), and the ledger and the artifact plane store into it.
	vaultPath := filepath.Join(cfg.Storage.DataDir, "vault")
	artifactVault := vault.NewArtifactVault(vaultPath)
	if cfg.Execution.Erasure.Enabled {
		ew := erasureworker.New(erasureStore, storeHandle.ContentEraser, erasureworker.Config{
			Vault:              artifactVault,
			TailSteps:          erasureTails,
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

	b.lifecycles = lifecycles
	b.mem, b.forgetSvc, b.artifactVault, b.memoryNotes = mem, forgetSvc, artifactVault, memoryNotes
}

// logFactLayerBoot writes the fact layer's boot line (the expect_log anchor of
// the fact arms) with its EFFECTIVE switches, and warns about every switch that
// is on but cannot act (ADR-0136 D4/D5).
func logFactLayerBoot(cfg *config.Config, mem *kernel.MemoryStack) {
	rv2 := cfg.Execution.Retrieval
	fc := cfg.Execution.Facts
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
}

// composeLateRecall attaches the recall lanes that call system agents through
// the metabolism stack's agent transport (ENG-1320).
func composeLateRecall(cfg *config.Config, mem *kernel.MemoryStack, meta *kernel.MetabolismStack,
	llmGateway *subnetwork.SubstrateLLMGateway, llmProvider *llm.Provider) {
	// ENG-1320: the recall lanes that call system agents through the agent transport
	// (Stage-B rerank via the reranker_agent, the agentic query-planner via the
	// retrieval_agent). The transport belongs to the metabolism stack, which is built
	// from the memory stack, so they are attached here, once, by the kernel; every
	// other lane was composed inside NewMemoryStack. Each dispatcher is used only when
	// its flag is on (ComposeLateRetrieval reads execution.retrieval).
	kernel.ComposeLateRetrieval(mem, cfg.Execution, kernel.LateRetrievalDeps{
		Reranker: &subnetwork.RerankerDispatcher{Caller: meta.AgentTransport, AgentID: "reranker_agent"},
		Planner: &subnetwork.RetrievalDispatcher{
			Caller:  meta.AgentTransport,
			AgentID: "retrieval_agent",
			Gateway: llmGateway,
			// An explicit generator id overrides; "" (default) ⇒ the retrieval_planner
			// role, falling back to memory, read per call (ENG-329). A FAST model
			// matters on the hot path.
			Model:     cfg.Execution.Retrieval.AgenticPlannerModel,
			RoleModel: retrievalPlannerRole(llmProvider),
		},
	})
	if cfg.Execution.Retrieval.AgenticRetrievalEnabled {
		warnUndeclaredPlannerModel(cfg.Execution.Retrieval.AgenticPlannerModel, cfg.LLMProvider.Generators)
	}
}
