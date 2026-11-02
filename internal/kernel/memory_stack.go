package kernel

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
)

// MemoryStack is the memory substrate of the system. It owns everything that
// persists agent knowledge: the vector database, cognitive fingerprints
// (ProfileStore), memory curation (Agent), procedural memory (Hippocampus),
// and cross-session workspace enrichment (WorkspaceStage).
//
// Biologically: this is the hippocampus + long-term memory cortex.
type MemoryStack struct {
	VecDB domain.VectorStore
	// ReadStore is the fail-closed read chokepoint over VecDB. Every principal-facing
	// read goes through it; the raw VecDB is reserved for kernel-internal components
	// that need the concrete adapter (graph store, spreader, profile store).
	ReadStore            domain.VectorStore
	WriteStore           domain.VectorStore // ADR-0085: write classification over ReadStore
	ProfileStore         memstore.ProfileStore
	Agent                *memory.Agent
	Hippocampus          *memory.Hippocampus
	QueryService         *memory.QueryService
	Embedder             domain.Embedder
	WorkspaceStage       domain.WorkspaceStage        // ADR-0016: may be nil
	IngestionManager     *memory.IngestionManager     // ADR-0028: ALWAYS non-nil (a zero queue size defaults to 1000). The old "may be nil" note was false and implied a raw-write fallback that could not fire.
	EntityIndex          *memory.EntityIndex          // ADR-0053 (T-Mem associative hop): in-memory entity→docs index; nil = surface-only recall
	ChunkTripletsBatcher *memory.ChunkTripletsBatcher // ADR-0053 Phase 0: batched per-chunk (h,r,t) extractor
	// ProcedureScheduler runs the ADR-0094 induction pass. nil ⇒ disabled, which is
	// the default (execution.procedure_induction_interval_hours = 0).
	ProcedureScheduler *memory.ProcedureScheduler
}

// NewMemoryStack constructs the memory layer from infrastructure primitives.
// It does not start background workers — call Start() for that.
//
// authorizer is the decision point consulted by the write chokepoint. A nil
// authorizer means the OSS default (unrestricted): writes keep their authored
// classification. The kernel always ASKS; what the answer is depends on whether a
// policy plugin is installed (ADR-0085 §4.1).
// chunkerCfg is the operator's `chunker` block. It is a SEPARATE parameter rather
// than a field on execCfg because that is where the schema already lives
// (config.Config.Chunker, ADR-0060 D7). Threading it here is what makes the block
// take effect at all: it was parsed, defaulted and validated, and then never
// reached the ingestion pipeline, which built its routing table from a Go literal.
func NewMemoryStack(vec domain.VectorStore, gen domain.Generator, embed domain.Embedder, execCfg config.ExecutionConfig, authorizer domain.Authorizer, chunkerCfg config.ChunkerConfig) *MemoryStack {
	// ADR-0085: every read passes the fail-closed read chokepoint and every write
	// passes the classification chokepoint — including LLM-driven writes. There is
	// no "trusted in-process" carve-out: process membership does not constrain a
	// model's output. Agent reads (QueryService) carry the principal's resolved
	// predicate; kernel-internal reads carry domain.ScopeSystem explicitly.
	//
	// Raw vec is retained ONLY for the GraphStore assertions (a component that needs
	// the concrete adapter). The spreading engine used to take it too and fetched
	// every BFS edge target by id with no predicate — the one enrichment lane that
	// still bypassed this chokepoint (2026-10-31 audit,
	// T-20261031-spreading-activation-lane-fetches-graph-targets). It now takes
	// scopedRead.
	scopedRead := authz.NewEnforcingVectorStore(vec, slog.Default())
	writeStore := authz.NewEnforcingStoreWriter(scopedRead, authorizer, slog.Default())
	// The profile store writes agent profiles and JUDICIAL RECORDS — verifier
	// critiques of agent output, carrying caller-supplied metadata. It used to hold
	// the raw adapter, so those were the one write class that reached Postgres with
	// no classification stamped and were read back with no predicate applied, which
	// is exactly the carve-out the comment above says does not exist.
	//
	// Its reads legitimately have no principal (the Gatekeeper consults a profile to
	// decide whether an agent may bid at all), so they seed the explicit ScopeSystem
	// bypass inside the store rather than inheriting whatever the caller carried —
	// see kernelRead in pgvector_profile_store.go.
	profileStore := memstore.NewProfileStore(writeStore)
	memoryManager := memory.NewMemoryManager(writeStore, embed)
	memoryAgent := memory.NewAgent(memoryManager, gen,
		execCfg.Memory.MemoryRelevanceThreshold, execCfg.Memory.MaxMemoryResults, execCfg.Memory.MaxNeighborExpansion,
		execCfg.Memory.Tier1ChannelCapacity, execCfg.Memory.Tier2BatchSize, execCfg.Memory.Tier2MaxIdleSeconds, execCfg.Memory.Tier2LLMTimeout)
	// Passive experiential capture (auto-embedding step results + tool outputs into LTM) is
	// off unless explicitly enabled. See execution.experiential_memory_enabled.
	memoryAgent.RecordExperiential = execCfg.Memory.ExperientialMemoryEnabled
	// ADR-0049 §A2.2: the outcome record — one abstracted record per completed plan.
	// A separate arm from the raw path above, because they are opposite designs: that
	// one embeds whole tool payloads (removed 2026-07-18, stays off), this one embeds
	// only the D7 projection and references everything else. `vec` is the raw adapter
	// on purpose — this is a kernel-owned write of a row the kernel itself stamps, not
	// a principal-facing one.
	memoryAgent.RecordOutcomes = execCfg.Memory.ExperienceRecordsEnabled
	memoryAgent.SurpriseFloor = execCfg.Memory.ExperienceSurpriseFloor
	memoryAgent.ProcedureDeprecateBelow = execCfg.Procedure.ProcedureDeprecateBelow
	// Asserted rather than required on the port: a store that cannot persist episode
	// parents (a test fake, a future backend) leaves this nil, and the outcome record
	// is then written with a NULL parent instead of not written at all.
	if es, ok := vec.(domain.ExperienceStore); ok {
		memoryAgent.ExperienceStore = es
	}
	hippocampus := memory.NewHippocampus(scopedRead, embed,
		config.NewStaticPolicyProvider(execCfg.Hippocampus.HippocampusPolicies, execCfg.Hippocampus.HippocampusDefaultPolicy))
	// ADR-0085: the agent recall path is built ON the read chokepoint, not on the
	// raw adapter. This used to be `NewQueryService(embed, vec)` with the enforcing
	// store swapped in afterwards by app.go calling EnableAuthorization — correct in
	// the one boot path that remembered to call it, and silently unguarded anywhere
	// else. The enforcing store and the decision point are constructor arguments now,
	// so there is no order of operations that yields an unenforced QueryService.
	queryService := memory.NewQueryService(embed, scopedRead, authorizer)
	// ADR-0048 #1: gate agent recall on a relevance floor so irrelevant promoted
	// facts (a prior task's web search, a stray shell error) are dropped instead of
	// padding the top-k — and an all-irrelevant query returns empty, which the agent
	// reads as "no relevant memory" rather than treating junk as grounding.
	queryService.SetRelevanceFloor(execCfg.Retrieval.RecallSimilarityFloor)
	// ADR-0054 retrieval tuning: widen the seed/ANN fetch + returned window. The
	// previous hardcoded 25/10 (+ HNSW ef_search=40) capped the candidate pool too
	// small for the gold chunk to surface. 0 ⇒ built-in defaults.
	queryService.SetRecallSizes(execCfg.Retrieval.RecallTopK, execCfg.Retrieval.RecallOverFetch)

	// ADR-0016: Cross-session workspace enrichment.
	ws := memory.NewWorkspaceStage(scopedRead, embed, gen,
		execCfg.Workspace.WorkspacePlanningSlots, execCfg.Workspace.WorkspaceExecutionSlots,
		execCfg.Retrieval.RetrievalFloor, execCfg.Workspace.WorkspaceEnableDriftGuard, execCfg.Workspace.WorkspaceDriftThreshold)

	// PLANNERREQ REQ1: wire MinFactCosine — raw cosine floor before Planner injection.
	ws.MinFactCosine = execCfg.Workspace.WorkspaceMinFactCosine
	// PrimeForStep, its ActivationThreshold / LRU-cache tunables and the
	// RegisterCacheInvalidator hook were deleted 2026-11-01 (owner decision,
	// T-20261031-primeforstep-is-unwired-so-activation-threshold): ADR-0048 D3 had
	// left the executor-push path unwired since agent-pull replaced it (ADR-0036),
	// so the three config keys configured nothing live.
	// The ADR-0029 episodic retrieval lane used to be armed here by handing the
	// WorkspaceStage a PolicyProvider. Both the lane and the field are gone: its writer
	// (EpisodicExtractor / ConsolidatorAgent) was removed on 2026-07-18, so the lane
	// searched a corpus nothing could add to. The "episodic" HippocampusPolicy default
	// that armed it was retired on 2026-10-31 (owner decision; an operator file that
	// still declares it loads unchanged — see internal/config/retired_keys_test.go).

	// ADR-0017: Spreading activation layer (optional — depends on GraphStore).
	var entityIdx *memory.EntityIndex
	var chunkTripletsBatcher *memory.ChunkTripletsBatcher
	if gs, ok := vec.(domain.GraphStore); ok {
		// ADR-0053 (T-Mem associative hop; cited as "ADR-0052" until 2026-10-31): the
		// in-memory entity reverse index, built BEFORE the spreader so
		// queryService.EnableEntityRouting has an index to read on the first recall.
		//
		// The EdgeBatcher/EdgeWriter pair of the original ADR-0053 D2 LLM edge lane used
		// to be constructed here as well
		// and described as the "production path". It was not one: nothing in the kernel
		// ever called Enqueue on it, so it started a drain goroutine and an idle ticker
		// that consumed an always-empty queue for the lifetime of the process. Its
		// persistent graph write had already been disabled in favour of the
		// kg_extractor system agent writing chunk_triplets (ADR-0053 D2 revised), so
		// the wiring outlived its purpose by two ADRs.
		//
		// Removing it here changes no runtime behaviour, because there was no runtime
		// behaviour to change: the queue never received a document.
		//
		// The memory.EdgeBatcher / EdgeWriter / EdgeExtractor types have since been
		// deleted as well; cmd/chunk-fill builds a ChunkTripletsBatcher.
		//
		// Consequence worth stating plainly rather than discovering (re-checked
		// 2026-10-31): NOTHING populates this EntityIndex. EntityIndex.Load has no
		// caller and no GraphStore adapter implements its loader, EntityIndex.Add has
		// no production caller, and cmd/chunk-fill writes chunk_triplets rows, not
		// this index. execution.retrieval.recall_spreading_enabled defaults ON
		// (ADR-0049 D10), so EnableEntityRouting below IS armed — but against an
		// always-empty index, where every entity lane returns its input unchanged
		// (query.go: `len(embs) == 0` early return). Harmless (fail-open, no extra
		// cost beyond a map snapshot), but entity routing contributes nothing until
		// a writer exists: T-20261031-entityindex-is-never-populated-so-entity.
		entityIdx = memory.NewEntityIndex()

		// ADR-0085 D7 / ADR-0095 D9: the spreader fetches every BFS edge target by
		// id (spreading_engine.go), and its output REPLACES the already-scoped
		// result set in query.go, so it must read through the enforcing store —
		// a cross-scope discussed_in/co_activated edge otherwise returns the
		// restricted target to the caller. Reads fail closed on a missing
		// predicate; the engine treats that like any other fetch error and skips
		// the target.
		spEngine := memory.NewSpreadingEngine(gs, scopedRead,
			execCfg.Graph.DecayFactor, execCfg.Graph.MaxDepth, execCfg.Graph.EnergyFloor)
		// ADR-0053 D2: the per-type weight map is gone; edge.Weight is the LLM/Hebbian
		// confidence. The four `weight_*` config keys were retired 2026-10-31
		// (T-20261031-graph-edge-weight-and-consolidator-timeout).
		spEngine.HebbianDecayPerDay = execCfg.Retrieval.HebbianDecayPerDay // ADR-0049 D10: decay-on-spread-read
		ws.SpreadingEngine = spEngine

		// ADR-0053 Phase 0: build the batched per-chunk (h, r, t) extractor.
		// The ChunkTripletsBatcher is the production path for back-filling
		// the chunk_triplets table; it uses the same LLM (deepseek, via the
		// same purposeGenerator wrapper) and the same streaming-or-Generate
		// routing the removed EdgeBatcher used. Re-uses the EdgeExtraction* config knobs
		// (the prompt is a similar size; 16-fact batches stream fine on
		// the hosted reasoning model). nil = no enrichment (legacy).
		if cts, ok := vec.(domain.ChunkTripletsStore); ok {
			chunkTripletsBatcher = memory.NewChunkTripletsBatcher(gen, cts, memory.ChunkTripletsBatcherConfig{
				QueueSize:  execCfg.Ingestion.EdgeExtractionQueueSize,
				BatchSize:  execCfg.Ingestion.EdgeExtractionBatchSize,
				MaxIdle:    time.Duration(execCfg.Ingestion.EdgeExtractionMaxIdleMs) * time.Millisecond,
				LLMTimeout: time.Duration(execCfg.Ingestion.EdgeExtractionLLMTimeoutMs) * time.Millisecond,
			})
		}
		// ADR-0048 D2: enrich the agent's PULL recall with the same associative
		// spreading. Flag-gated; default ON since ADR-0049 D10 (config.go defaults).
		if execCfg.Retrieval.RecallSpreadingEnabled {
			queryService.EnableSpreading(spEngine)
		}
		// ADR-0053 (T-Mem associative hop): entity-aware routing. The "first hop" — finds the
		// top-K entity keys by query-embedding cosine and seeds the BFS with
		// their doc associations. Flag-gated by the same RecallSpreadingEnabled
		// knob (default on, ADR-0049 D10) — see the empty-index note above.
		if execCfg.Retrieval.RecallSpreadingEnabled {
			queryService.EnableEntityRouting(entityIdx)
		}
		// ADR-0049 D10: Hebbian co-activation edge reinforcement on recall (flag-gated;
		// default off — the constants are HITL-tuned against real traces).
		if execCfg.Retrieval.HebbianEnabled {
			queryService.EnableHebbian(gs,
				execCfg.Retrieval.HebbianLearningRate, execCfg.Retrieval.HebbianMaxWeight, execCfg.Retrieval.HebbianCoActivationFloor,
				execCfg.Retrieval.HebbianDecayPerDay, execCfg.Retrieval.HebbianBaseWeight, execCfg.Retrieval.HebbianTopN)
		}
		memoryAgent.GraphStore = gs
		// ADR-0025: wire EdgeWriter for Tier-2 discussed_in edges.
		memoryAgent.EdgeWriter = &memory.GraphStoreEdgeWriter{GraphStore: gs}
	}

	// ADR-0028: ingestion pipeline — SceneGenerator + IngestionManager + DirectoryWatcher.
	sceneGen := memory.NewSceneGenerator(gen)
	ingestionMgr := memory.NewIngestionManager(sceneGen, embed, memoryAgent, ingestionConfigFrom(execCfg.Ingestion), chunkerCfg)
	// NOTE: the legacy single-directory DirectoryWatcher (ADR-0028/0031) was removed
	// from the boot path — it delivered file events to a NoOpSignalReceiver (dead
	// weight) and its fixed-dir fsnotify watch errored at startup when InboxDir was
	// absent. On-demand + reactive watch sources (ADR-0032/REACT-06) supersede it.

	// ADR-0094 D3 + ADR-0049 A2.5: the offline induction pass. Constructed only when
	// an interval is configured, so a deployment that has not opted in carries no
	// goroutine, no ticker and no store reads. The inducer writes through the RAW
	// adapter because a procedure is a kernel-authored artifact, not a principal's
	// write — the same reasoning as the experience parent row.
	var procScheduler *memory.ProcedureScheduler
	if h := execCfg.Procedure.ProcedureInductionIntervalHours; h > 0 {
		inducer := &memory.ProcedureInducer{
			Store:      vec,
			Embedder:   embed,
			MinSamples: execCfg.Procedure.ProcedureMinSamples,
		}
		if es, ok := vec.(domain.ExperienceStore); ok {
			inducer.Experience = es
		}
		procScheduler = &memory.ProcedureScheduler{
			Inducer:     inducer,
			Store:       vec,
			Interval:    time.Duration(h) * time.Hour,
			MaxEpisodes: execCfg.Procedure.ProcedureMaxEpisodesPerPass,
		}
	}

	return &MemoryStack{
		VecDB:                vec,
		ReadStore:            scopedRead,
		WriteStore:           writeStore,
		ProfileStore:         profileStore,
		Agent:                memoryAgent,
		Hippocampus:          hippocampus,
		QueryService:         queryService,
		Embedder:             embed,
		WorkspaceStage:       ws,
		IngestionManager:     ingestionMgr,
		EntityIndex:          entityIdx,
		ChunkTripletsBatcher: chunkTripletsBatcher,
		ProcedureScheduler:   procScheduler,
	}
}

// Start launches the memory Agent's background workers and the ingestion pipeline.
func (s *MemoryStack) Start(ctx context.Context) error {
	// The Tier-1 channel the drain serves is filled ONLY by the raw experiential path
	// (RecordExecution / recordReadFact), which is gated on RecordExperiential — off in
	// production since 2026-07-18. Starting the drain regardless meant a goroutine, an
	// idle timer and (on any drain) an LLM judge sitting on a channel nothing could
	// write to. Gated here so the cost is paid only by a deployment that enables the arm.
	if s.Agent.RecordExperiential {
		s.Agent.StartTier2Drain(ctx)
	}
	// The original ADR-0053 D2 LLM-lane EdgeBatcher used to be started here. It is no longer constructed
	// — see NewMemoryStack for why. The ADR-0053 batcher below is the one that
	// actually receives work from the ingest path.
	// ADR-0053 Phase 0: start the per-chunk (h, r, t) extraction batcher.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Start(ctx)
	}
	// ADR-0028: start the ingestion pipeline. (The legacy DirectoryWatcher that
	// used to start here was removed — see NewMemoryStack.)
	if s.IngestionManager != nil {
		s.IngestionManager.Start(ctx)
	}
	// ADR-0094: the induction pass. Start is a no-op when the scheduler is nil
	// (no interval configured), so the default deployment is unaffected.
	s.ProcedureScheduler.Start(ctx)
	return s.Agent.StartMemoryWorker(ctx, false)
}

// Shutdown is a no-op for MemoryStack — its resources (VecDB) are closed by
// the Kernel which owns the infrastructure handle.
func (s *MemoryStack) Shutdown(_ context.Context) {
	slog.Info("🧠 MemoryStack: shutdown acknowledged")
	// ADR-0053 Phase 0: flush the ChunkTripletsBatcher's tail (Stop blocks until the
	// drain goroutine exits and the last batch is written). Best-effort: a failed
	// tail-flush is logged inside the batcher, not surfaced here.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Stop()
	}
}

// ingestionConfigFrom maps the operator's execution.ingestion block onto the
// IngestionManager's settings. A function of its own so a test can prove each
// field is read (AGENTS.md §3.4: a validated-but-unread setting is a defect).
func ingestionConfigFrom(c config.IngestionConfig) memory.IngestionConfig {
	return memory.IngestionConfig{
		QueueSize:      c.IngestionQueueSize,
		BatchSize:      c.IngestionBatchSize,
		Workers:        c.IngestionWorkers,
		BatchWait:      time.Duration(c.IngestionBatchWaitMs) * time.Millisecond,
		ContextPrepend: c.ChunkContextPrepend,
		// T-20261102-ingestion-structure-junk-filter-silently-drops: junk leaves are
		// dropped only for these parser backends (default ["docling"]; chat keeps all).
		JunkFilterBackends: append([]string(nil), c.StructureJunkFilterBackends...),
	}
}
