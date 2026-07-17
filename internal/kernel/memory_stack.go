package kernel

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
	"github.com/cambrian-sh/core/internal/scope"
)

// MemoryStack is the memory substrate of the system. It owns everything that
// persists agent knowledge: the vector database, cognitive fingerprints
// (ProfileStore), memory curation (Agent), procedural memory (Hippocampus),
// and cross-session workspace enrichment (WorkspaceStage).
//
// Biologically: this is the hippocampus + long-term memory cortex.
type MemoryStack struct {
	VecDB                domain.VectorStore
	WriteStore           domain.VectorStore // ADR-0034: ScopedStoreWriter over ScopedVectorStore (read+write gated)
	ProfileStore         memstore.ProfileStore
	Agent                *memory.Agent
	Hippocampus          *memory.Hippocampus
	QueryService         *memory.QueryService
	Embedder             domain.Embedder
	WorkspaceStage       domain.WorkspaceStage        // ADR-0016: may be nil
	GraphStore           domain.GraphStore            // ADR-0025: may be nil; used to construct PgSceneWriter
	IngestionManager     *memory.IngestionManager     // ADR-0028: may be nil when IngestionQueueSize=0
	EntityIndex          *memory.EntityIndex          // ADR-0052: in-memory entity→docs index; nil = surface-only recall
	EdgeWriter           *memory.EdgeWriter           // ADR-0052: per-doc LLM-driven edge populator (sync mode, kept for tests)
	EdgeBatcher          *memory.EdgeBatcher          // ADR-0052: batched LLM-driven edge populator; production path
	ChunkTripletsBatcher *memory.ChunkTripletsBatcher // ADR-0053 Phase 0: batched per-chunk (h,r,t) extractor
}

// NewPgSceneWriter constructs a per-request PgSceneWriter for DAGExecutor.
// Returns nil when the VecDB does not support graph operations.
// ADR-0025: one instance per plan execution — tracks lastSceneID for specifies edges.
func (s *MemoryStack) NewPgSceneWriter() *memory.PgSceneWriter {
	if s.VecDB == nil || s.Embedder == nil {
		return nil
	}
	return &memory.PgSceneWriter{
		Store:      s.VecDB,
		Embedder:   s.Embedder,
		GraphStore: s.GraphStore, // may be nil; nil disables specifies edges
	}
}

// NewMemoryStack constructs the memory layer from infrastructure primitives.
// It does not start background workers — call Start() for that.
func NewMemoryStack(vec domain.VectorStore, gen domain.Generator, embed domain.Embedder, execCfg config.ExecutionConfig) *MemoryStack {
	profileStore := memstore.NewProfileStore(vec)
	// ADR-0034 (D8/R3): route memory writes through the ScopedStoreWriter so every
	// write — including the LLM-driven ConsolidatorAgent — is validated against the
	// controlled vocabulary + writer ForbiddenTags and has provenance kernel-stamped.
	// Enforcement activates per-write when a WriterScope is seeded (scope.WithWriterScope);
	// kernel curation writes without a writer scope pass through unchanged.
	// ADR-0034 (D5): scopedRead is the fail-closed read chokepoint; writeStore layers
	// write-side validation on top. Agent reads (QueryService) carry the agent's
	// effective scope; all kernel-internal reads carry domain.ScopeSystem explicitly.
	// Raw vec is retained ONLY for the GraphStore assertions + spreading engine +
	// profile store (system components that need the concrete adapter).
	scopedRead := scope.NewScopedVectorStore(vec, slog.Default())
	writeStore := scope.NewScopedStoreWriter(scopedRead, scope.NewVocabulary(execCfg.ClassificationVocabulary), slog.Default())
	memoryManager := memory.NewMemoryManager(writeStore, embed)
	memoryAgent := memory.NewAgent(memoryManager, gen,
		execCfg.MemoryRelevanceThreshold, execCfg.MaxMemoryResults, execCfg.MaxNeighborExpansion,
		execCfg.Tier1ChannelCapacity, execCfg.Tier2BatchSize, execCfg.Tier2MaxIdleSeconds, execCfg.Tier2LLMTimeout)
	hippocampus := memory.NewHippocampus(scopedRead, embed,
		config.NewStaticPolicyProvider(execCfg.HippocampusPolicies, execCfg.HippocampusDefaultPolicy))
	queryService := memory.NewQueryService(embed, vec)
	// ADR-0048 #1: gate agent recall on a relevance floor so irrelevant promoted
	// facts (a prior task's web search, a stray shell error) are dropped instead of
	// padding the top-k — and an all-irrelevant query returns empty, which the agent
	// reads as "no relevant memory" rather than treating junk as grounding.
	queryService.SetRelevanceFloor(execCfg.RecallSimilarityFloor)
	// ADR-0054 retrieval tuning: widen the seed/ANN fetch + returned window. The
	// previous hardcoded 25/10 (+ HNSW ef_search=40) capped the candidate pool too
	// small for the gold chunk to surface. 0 ⇒ built-in defaults.
	queryService.SetRecallSizes(execCfg.RecallTopK, execCfg.RecallOverFetch)

	// ADR-0016: Cross-session workspace enrichment.
	ws := memory.NewWorkspaceStage(scopedRead, embed, gen,
		execCfg.WorkspacePlanningSlots, execCfg.WorkspaceExecutionSlots,
		execCfg.RetrievalFloor, execCfg.WorkspaceEnableDriftGuard, execCfg.WorkspaceDriftThreshold)

	// PLANNERREQ REQ1: wire MinFactCosine — raw cosine floor before Planner injection.
	ws.MinFactCosine = execCfg.WorkspaceMinFactCosine
	// ADR-0022: wire ActivationThreshold (distinct from RetrievalFloor).
	ws.ActivationThreshold = execCfg.ActivationThreshold
	// ADR-0022: wire LRU cache capacity from config so it isn't hardcoded to 100.
	ws.LRUCacheCapacity = execCfg.WorkspaceLRUCacheCapacity
	// ADR-0029: wire PolicyProvider so the episodic retrieval lane is active.
	ws.PolicyProvider = config.NewStaticPolicyProvider(execCfg.HippocampusPolicies, execCfg.HippocampusDefaultPolicy)
	// ADR-0022 Phase 2B: invalidate WorkspaceStage LRU cache on Tier-2 drain.
	memoryAgent.RegisterCacheInvalidator(ws)

	// ADR-0017: Spreading activation layer (optional — depends on GraphStore).
	var entityIdx *memory.EntityIndex
	var edgeWriter *memory.EdgeWriter
	var edgeBatcher *memory.EdgeBatcher
	var chunkTripletsBatcher *memory.ChunkTripletsBatcher
	if gs, ok := vec.(domain.GraphStore); ok {
		// ADR-0052: build the LLM-driven edge populator (batched) and the
		// in-memory entity reverse index BEFORE the spreader so
		// queryService.EnableEntityRouting has a populated index on the first
		// recall. The index is rebuilt from existing edges on boot; for a
		// fresh DB it starts empty and warms up as the batcher populates it.
		entityIdx = memory.NewEntityIndex()
		extractor := memory.NewEdgeExtractor(gen)
		edgeWriter = memory.NewEdgeWriter(extractor, gs, entityIdx, embed)
		edgeBatcher = memory.NewEdgeBatcher(extractor, edgeWriter, memory.EdgeBatcherConfig{
			QueueSize:  execCfg.EdgeExtractionQueueSize,
			BatchSize:  execCfg.EdgeExtractionBatchSize,
			MaxIdle:    time.Duration(execCfg.EdgeExtractionMaxIdleMs) * time.Millisecond,
			LLMTimeout: time.Duration(execCfg.EdgeExtractionLLMTimeoutMs) * time.Millisecond,
		})

		spEngine := memory.NewSpreadingEngine(gs, vec,
			execCfg.Graph.DecayFactor, execCfg.Graph.MaxDepth, execCfg.Graph.EnergyFloor)
		// ADR-0052: per-type weight map removed; edge.Weight is the LLM/Hebbian
		// confidence. The 4 per-type constants are still read from config for
		// audit visibility (see execCfg.Graph) but no longer feed the spreader.
		_ = execCfg.Graph.WeightContradicts
		_ = execCfg.Graph.WeightSpecifies
		_ = execCfg.Graph.WeightCloses
		_ = execCfg.Graph.WeightDiscussedIn
		spEngine.HebbianDecayPerDay = execCfg.HebbianDecayPerDay // ADR-0049 D10: decay-on-spread-read
		ws.SpreadingEngine = spEngine

		// ADR-0053 Phase 0: build the batched per-chunk (h, r, t) extractor.
		// The ChunkTripletsBatcher is the production path for back-filling
		// the chunk_triplets table; it uses the same LLM (deepseek, via the
		// same purposeGenerator wrapper) and the same streaming-or-Generate
		// routing as the EdgeBatcher. Re-uses the EdgeExtraction* config knobs
		// (the prompt is a similar size; 16-fact batches stream fine on
		// the hosted reasoning model). nil = no enrichment (legacy).
		if cts, ok := vec.(memory.ChunkTripletsStore); ok {
			chunkTripletsBatcher = memory.NewChunkTripletsBatcher(gen, cts, memory.ChunkTripletsBatcherConfig{
				QueueSize:  execCfg.EdgeExtractionQueueSize,
				BatchSize:  execCfg.EdgeExtractionBatchSize,
				MaxIdle:    time.Duration(execCfg.EdgeExtractionMaxIdleMs) * time.Millisecond,
				LLMTimeout: time.Duration(execCfg.EdgeExtractionLLMTimeoutMs) * time.Millisecond,
			})
		}
		// ADR-0048 D2: optionally enrich the agent's PULL recall with the same
		// associative spreading (flag-gated; default off for latency/cost control).
		if execCfg.RecallSpreadingEnabled {
			queryService.EnableSpreading(spEngine)
		}
		// ADR-0052: entity-aware routing. The T-Mem "first hop" — finds the
		// top-K entity keys by query-embedding cosine and seeds the BFS with
		// their doc associations. Flag-gated by the same RecallSpreadingEnabled
		// knob; off for latency/cost control.
		if execCfg.RecallSpreadingEnabled {
			queryService.EnableEntityRouting(entityIdx)
		}
		// ADR-0049 D10: Hebbian co-activation edge reinforcement on recall (flag-gated;
		// default off — the constants are HITL-tuned against real traces).
		if execCfg.HebbianEnabled {
			queryService.EnableHebbian(gs,
				execCfg.HebbianLearningRate, execCfg.HebbianMaxWeight, execCfg.HebbianCoActivationFloor,
				execCfg.HebbianDecayPerDay, execCfg.HebbianBaseWeight, execCfg.HebbianTopN)
		}
		memoryAgent.GraphStore = gs
		// ADR-0025: wire EdgeWriter for Tier-2 discussed_in edges.
		memoryAgent.EdgeWriter = &memory.GraphStoreEdgeWriter{GraphStore: gs}
	}

	// ADR-0025: GraphStore for scene writing (nil when vec doesn't implement it).
	var gs domain.GraphStore
	if g, ok := vec.(domain.GraphStore); ok {
		gs = g
	}

	// ADR-0028: ingestion pipeline — SceneGenerator + IngestionManager + DirectoryWatcher.
	sceneGen := memory.NewSceneGenerator(gen)
	ingestionCfg := memory.IngestionConfig{
		QueueSize: execCfg.IngestionQueueSize,
		BatchSize: execCfg.IngestionBatchSize,
		Workers:   execCfg.IngestionWorkers,
		BatchWait: time.Duration(execCfg.IngestionBatchWaitMs) * time.Millisecond,
	}
	ingestionMgr := memory.NewIngestionManager(sceneGen, embed, memoryAgent, ingestionCfg)
	// NOTE: the legacy single-directory DirectoryWatcher (ADR-0028/0031) was removed
	// from the boot path — it delivered file events to a NoOpSignalReceiver (dead
	// weight) and its fixed-dir fsnotify watch errored at startup when InboxDir was
	// absent. On-demand + reactive watch sources (ADR-0032/REACT-06) supersede it.

	return &MemoryStack{
		VecDB:                vec,
		WriteStore:           writeStore,
		ProfileStore:         profileStore,
		Agent:                memoryAgent,
		Hippocampus:          hippocampus,
		QueryService:         queryService,
		Embedder:             embed,
		WorkspaceStage:       ws,
		GraphStore:           gs,
		IngestionManager:     ingestionMgr,
		EntityIndex:          entityIdx,
		EdgeWriter:           edgeWriter,
		EdgeBatcher:          edgeBatcher,
		ChunkTripletsBatcher: chunkTripletsBatcher,
	}
}

// Start launches the memory Agent's background workers and the ingestion pipeline.
func (s *MemoryStack) Start(ctx context.Context) error {
	s.Agent.StartTier2Drain(ctx)
	// ADR-0052: start the entity+relation extraction batcher. The drain
	// goroutine runs until Stop() (called from Shutdown) flushes the tail.
	if s.EdgeBatcher != nil {
		s.EdgeBatcher.Start(ctx)
	}
	// ADR-0053 Phase 0: start the per-chunk (h, r, t) extraction batcher.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Start(ctx)
	}
	// ADR-0028: start the ingestion pipeline. (The legacy DirectoryWatcher that
	// used to start here was removed — see NewMemoryStack.)
	if s.IngestionManager != nil {
		s.IngestionManager.Start(ctx)
	}
	return s.Agent.StartMemoryWorker(ctx, false)
}

// Shutdown is a no-op for MemoryStack — its resources (VecDB) are closed by
// the Kernel which owns the infrastructure handle.
func (s *MemoryStack) Shutdown(_ context.Context) {
	slog.Info("🧠 MemoryStack: shutdown acknowledged")
	// ADR-0052: flush the EdgeBatcher's tail (Stop blocks until the
	// drain goroutine exits and the last batch is written). Best-effort:
	// a failed tail-flush is logged inside the batcher, not surfaced here.
	if s.EdgeBatcher != nil {
		s.EdgeBatcher.Stop()
	}
	// ADR-0053 Phase 0: flush the ChunkTripletsBatcher's tail.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Stop()
	}
}
