package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
)

// MemoryStack is the memory substrate of the system. It owns everything that
// persists agent knowledge: the vector database, cognitive fingerprints
// (ProfileStore), memory curation (Agent) and cross-session workspace enrichment (WorkspaceStage).
//
// Biologically: this is the hippocampus + long-term memory cortex.
type MemoryStack struct {
	// The three tiers (AGENTS.md §2.1) are unexported and reached only through
	// Reader(), Writer() and KernelArtifactStore(reason) (ENG-1476). They were public
	// domain.VectorStore fields, so a raw store and an enforced one had the same type
	// and only review kept principal-serving code off the raw one.
	//
	// vecDB is tier 0, the raw adapter: no enforcement.
	vecDB domain.VectorStore
	// readStore is tier 1, the fail-closed read chokepoint over vecDB
	// (*authz.EnforcingVectorStore). It is a VectorReader only: it cannot write.
	readStore domain.VectorReader
	// writeStore is tier 2, write classification (*authz.EnforcingStoreWriter, ADR-0085)
	// over vecDB's write side. It is a VectorWriter only: it cannot read.
	writeStore domain.VectorWriter
	// eraser is the RAW ADR-0137 memory eraser (MemoryBackend.Eraser), composed here
	// with the other tier-0 components and deliberately UNEXPORTED with no accessor:
	// it enforces nothing about who may erase what, so no production code may reach it
	// until ADR-0137 S3 (ENG-411 job B) wraps it in authz.EnforcingEraser — that
	// constructor-injected decorator is what this stack will expose, never this field
	// (AGENTS.md §2.3). Stage A (S1–S2) ships the storage and transaction only; the
	// field is read by kernel tests (memory_eraser_composition_test.go). It is NOT dead
	// code: job B consumes it. nil when the backend has no eraser.
	eraser domain.MemoryEraser
	// DocumentStore is the ADR-0093 write chokepoint for the source-document row
	// (memory.EnforcingDocumentStore over the raw adapter), already held by
	// IngestionManager. nil when the backend cannot store documents (a test fake).
	DocumentStore        domain.DocumentStore
	ProfileStore         memstore.ProfileStore
	Agent                *memory.Agent
	QueryService         *memory.QueryService
	Embedder             domain.Embedder
	WorkspaceStage       domain.WorkspaceStage        // ADR-0016: may be nil
	IngestionManager     *memory.IngestionManager     // ADR-0028: ALWAYS non-nil (a zero queue size defaults to 1000). The old "may be nil" note was false and implied a raw-write fallback that could not fire.
	ChunkTripletsBatcher *memory.ChunkTripletsBatcher // ADR-0053 Phase 0: batched per-chunk (h,r,t) extractor
	// FactWriter is the ENFORCING write side of the ADR-0136 A1 fact layer
	// (authz.EnforcingKnowledgeWriter over the raw knowledge substrate), the only write
	// handle on memory_fact items in the kernel. FactReader is the enforcing read side
	// (authz.EnforcingFactItemReader over postgres.PgFactItemReader). Both nil when no
	// substrate was handed in (a test fake). They are built whether or not the fact
	// layer is on, so the QueryService always holds the enforcing reader; the flags
	// decide use.
	FactWriter domain.FactSubstrateWriter
	FactReader domain.FactItemReader
	// FactInspector is the operator plane's read-only fact inspection
	// (authz.EnforcingFactInspector over FactSubstrate.Inspector; ListFacts,
	// GetFactChain, GetFactCoverage). nil when no inspector was handed in.
	FactInspector domain.FactInspector
	// FactExtractor runs ADR-0136 D4 extraction off the ingest path, writing items and
	// (with assessments on) keyed assessments. nil unless execution.facts.extract_enabled
	// and a substrate exists.
	FactExtractor *memory.FactExtractor
	// FactBackfill feeds stored documents without a memory_fact extraction marker
	// through FactExtractor (cmd/facts-backfill). nil exactly when FactExtractor is nil
	// or no lister was handed in.
	FactBackfill *memory.FactBackfill
	// FactReset is the ADR-0136 A2-Q3 hard reset of the fact layer for a re-run
	// (cmd/facts-reset): FactSubstrate.Resetter, handed through unchanged. nil unless a
	// maintenance command handed one in — the kernel never resets facts.
	FactReset domain.FactResetter
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
//
// factGen is the generator of the ADR-0136 fact extractor (the "facts" system role);
// nil falls back to gen. It is read only when execution.facts.extract_enabled.
//
// facts is the RAW knowledge substrate the ADR-0136 A1 fact layer is built on. It is a
// constructor argument (AGENTS.md §2.1, §2.3): this function wraps it in the enforcing
// writer and reader and hands only those to the extractor and the QueryService. The
// zero value means no fact layer (every fact switch is then inert, and the boot line
// says so).
//
// backend is the raw adapter's capabilities as typed fields (MemoryBackend; production
// passes NewMemoryBackend(adapter)). It replaced runtime probing of a single
// domain.VectorStore for the graph, experience, triplet and document capabilities
// (ENG-1504): an absent capability is now a compile error in production and an Error
// line for a hand-built partial backend, never a silent feature switch-off.
func NewMemoryStack(backend MemoryBackend, gen domain.Generator, embed domain.Embedder, execCfg config.ExecutionConfig, authorizer domain.Authorizer, chunkerCfg config.ChunkerConfig, factGen domain.Generator, facts FactSubstrate) *MemoryStack {
	// ADR-0085: every read passes the fail-closed read chokepoint and every write
	// passes the classification chokepoint — including LLM-driven writes. There is
	// no "trusted in-process" carve-out: process membership does not constrain a
	// model's output. Agent reads (QueryService) carry the principal's resolved
	// predicate; kernel-internal reads carry domain.ScopeSystem explicitly.
	//
	// The raw backend is retained ONLY for components that need the concrete adapter
	// (graph store, triplet store, experience parents — typed MemoryBackend fields). The spreading engine used to take it too and fetched
	// every BFS edge target by id with no predicate — the one enrichment lane that
	// still bypassed this chokepoint (2026-10-31 audit,
	// T-20261031-spreading-activation-lane-fetches-graph-targets). It now takes
	// scopedRead.
	if backend.Vector == nil {
		panic("kernel.NewMemoryStack: MemoryBackend.Vector is nil")
	}
	backend.reportMissing()
	vec := backend.Vector
	scopedRead := authz.NewEnforcingVectorStore(vec, slog.Default())
	// The write chokepoint wraps the raw adapter's WRITE side directly. It used to wrap
	// scopedRead, whose write methods were unenforced pass-throughs to the same adapter
	// — the rows written are identical; the read tier just no longer has a write path
	// to forward through (ENG-1476).
	writeStore := authz.NewEnforcingStoreWriter(vec, authorizer, slog.Default())
	// enforced is the view handed to the consumers that both read and write (the
	// memory manager, the profile store): reads through tier 1, writes through tier 2,
	// and the kernel's own maintenance (IncrementAccess, the decay sweep's
	// GetStaleMemories) on the raw adapter — exactly what they reached before, when
	// they held writeStore and its reads and maintenance fell through scopedRead's
	// pass-throughs to the adapter. Maintenance serves no principal.
	enforced := tieredStore{VectorReader: scopedRead, VectorWriter: writeStore, VectorMaintenance: vec}
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
	profileStore := memstore.NewProfileStore(enforced)
	memoryManager := memory.NewMemoryManager(enforced, embed)
	memoryAgent := memory.NewAgent(memoryManager, execCfg.Memory.MaxNeighborExpansion)
	// The passive raw capture (auto-embedded step results and tool outputs behind
	// `experiential_memory_enabled`, curated by the ADR-0015 tier-2 judge) was deleted
	// 2026-11-05 with its config keys (decisions/scenes-cancelled.md).
	//
	// ADR-0049 §A2.2: the plan-outcome hook (episode open/close, procedure feedback).
	// The mutation action events and the A2.3 failure record were stopped 2026-11-14
	// (owner decision, ENG-1595), and execution.memory.experience_surprise_floor that
	// gated the failure record was retired with them (internal/config retiredConfigKeys).
	memoryAgent.RecordOutcomes = execCfg.Memory.ExperienceRecordsEnabled
	memoryAgent.ProcedureDeprecateBelow = execCfg.Procedure.ProcedureDeprecateBelow
	// backend.Experience is the raw adapter on purpose (ADR-0095 D1): the episode parent
	// row is a kernel-owned write of a row the kernel itself stamps, not a
	// principal-facing one. A partial backend without it (a test fake; reported at
	// Error above) leaves this nil, and the outcome record is then written with a NULL
	// parent instead of not written at all.
	if backend.Experience != nil {
		memoryAgent.ExperienceStore = backend.Experience
	}
	// The ADR-0027 Hippocampus (procedural plan memory) was deleted 2026-11-14
	// (owner decision, third round; ENG-1301): its writer had no production caller.
	// ADR-0085: the agent recall path is built ON the read chokepoint, not on the
	// raw adapter. This used to be `NewQueryService(embed, vec)` with the enforcing
	// store swapped in afterwards by app.go calling EnableAuthorization — correct in
	// the one boot path that remembered to call it, and silently unguarded anywhere
	// else. The enforcing store and the decision point are constructor arguments now,
	// so there is no order of operations that yields an unenforced QueryService.
	//
	// ADR-0136 A1: the fact layer's chokepoints are composed HERE and nowhere else — the
	// classified writer (ClassifyWrite per item) and the fail-closed reader over the raw
	// substrate — and the extractor and the QueryService receive them as constructor
	// arguments, the same discipline as the chunk read store. Nothing in the kernel
	// reads or writes memory_fact items through the raw store.
	var factWriter domain.FactSubstrateWriter
	var factItemReader domain.FactItemReader
	var laneReader memory.FactReader
	if facts.Store != nil && facts.Reader != nil {
		factWriter = authz.NewEnforcingKnowledgeWriter(facts.Store, authorizer, slog.Default())
		efr := authz.NewEnforcingFactItemReader(facts.Reader, slog.Default())
		factItemReader, laneReader = efr, efr
	}
	// T-20261103-operator-plane-cannot-inspect-facts-no-rpc: the operator plane's fact
	// inspection is composed here with the other fact chokepoints, fail-closed.
	var factInspector domain.FactInspector
	if facts.Inspector != nil {
		factInspector = authz.NewEnforcingFactInspector(facts.Inspector, slog.Default())
	}
	queryService := memory.NewQueryServiceWithFacts(embed, scopedRead, authorizer, laneReader)
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
	// ENG-465: priming reads as the planning principal through the QueryService.
	ws := memory.NewWorkspaceStage(queryService, embed, gen,
		execCfg.Workspace.WorkspacePlanningSlots, execCfg.Workspace.WorkspaceExecutionSlots,
		execCfg.Retrieval.RetrievalFloor)

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

	// ADR-0017: Spreading activation layer. backend.Graph is the raw adapter (the
	// engine's graph walk reads edges only; every edge TARGET is fetched through
	// scopedRead below — ADR-0085 D7 / ADR-0095 D9).
	var chunkTripletsBatcher *memory.ChunkTripletsBatcher
	if gs := backend.Graph; gs != nil {
		// The ADR-0053 (T-Mem) EntityIndex and its entity-routing lane were deleted
		// 2026-11-15 (owner decision, ENG-76): nothing ever populated the index.

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

		// ADR-0053 Phase 0: build the batched per-chunk (h, r, t) extractor.
		// The ChunkTripletsBatcher is the production path for back-filling
		// the chunk_triplets table; it uses the same LLM (deepseek, via the
		// same purposeGenerator wrapper) and the same streaming-or-Generate
		// routing the removed EdgeBatcher used. Re-uses the EdgeExtraction* config knobs
		// (the prompt is a similar size; 16-fact batches stream fine on
		// the hosted reasoning model). nil = no enrichment (legacy).
		// backend.ChunkTriplets is the raw adapter: a kernel-authored write of
		// LLM-free triplets keyed by chunk id (ADR-0053), not a principal-facing read.
		if cts := backend.ChunkTriplets; cts != nil {
			chunkTripletsBatcher = memory.NewChunkTripletsBatcher(gen, cts, chunkTripletsBatcherConfigFrom(execCfg.Ingestion))
		}
		// ADR-0048 D2: enrich the agent's PULL recall with the same associative
		// spreading. Flag-gated; default ON since ADR-0049 D10 (config.go defaults).
		if execCfg.Retrieval.RecallSpreadingEnabled {
			queryService.EnableSpreading(spEngine)
			// ENG-484: keep the assembled (non-displacing) order through the spread re-rank.
			queryService.SetSpreadingKeepsAssemblyOrder(execCfg.Retrieval.RecallSpreadingKeepAssemblyOrder)
			slog.Info(fmt.Sprintf("retrieval: recall_spreading_enabled=true recall_spreading_keep_assembly_order=%t",
				execCfg.Retrieval.RecallSpreadingKeepAssemblyOrder))
		} else if execCfg.Retrieval.RecallSpreadingKeepAssemblyOrder {
			slog.Warn("retrieval: recall_spreading_keep_assembly_order needs recall_spreading_enabled; it has no effect")
		}
		// ADR-0049 D10: Hebbian co-activation edge reinforcement on recall (flag-gated by
		// execution.retrieval.hebbian_enabled, which DefaultConfig turns ON; the constants
		// are HITL-tuned against real traces).
		if execCfg.Retrieval.HebbianEnabled {
			queryService.EnableHebbian(gs,
				execCfg.Retrieval.HebbianLearningRate, execCfg.Retrieval.HebbianMaxWeight, execCfg.Retrieval.HebbianCoActivationFloor,
				execCfg.Retrieval.HebbianDecayPerDay, execCfg.Retrieval.HebbianBaseWeight, execCfg.Retrieval.HebbianTopN)
		}
	}

	// ADR-0028: ingestion pipeline. (Its SceneGenerator was deleted 2026-11-05 with
	// scenes — decisions/scenes-cancelled.md.)
	ingestionMgr := memory.NewIngestionManager(embed, memoryAgent, ingestionConfigFrom(execCfg.Ingestion), chunkerCfg)
	// ADR-0093: the source-document row's write chokepoint. `documents.tags` is the
	// authoritative classification every access decision reads, so it goes through the
	// decision point exactly like the chunk writes above (ADR-0085). It is composed
	// HERE, beside the tiers it mirrors, so the ingest path holds the enforcing store
	// whenever the adapter can store documents — it used to be built in app/app.go and
	// attached there, so its presence depended on a post-construction call outside the
	// composition point being reached (AGENTS.md §2.1/§2.3,
	// T-20261113-memory-tier-stores-are-composed-outside). The raw adapter is wrapped,
	// never handed over. A partial backend without Documents (a test fake; reported at
	// Error above) leaves the pre-ADR-0093 behaviour: documents are not recorded.
	var documentStore domain.DocumentStore
	if ds := backend.Documents; ds != nil {
		documentStore = memory.NewEnforcingDocumentStore(ds, authorizer, slog.Default())
		ingestionMgr.SetDocumentStore(documentStore)
	}
	// ADR-0138 D2-D4: the document lifecycle is composed HERE, with the manager, so no
	// ingest path can run without it once a database is wired. Its writer check is
	// computed inside the store's transaction (the writer of record against the
	// kernel-stamped Author), not attached as a policy.
	if facts.Documents != nil {
		ingestionMgr.SetDocumentLifecycle(facts.Documents)
	} else {
		slog.Info("ADR-0138: no document lifecycle store: re-ingesting a changed document does not retire its previous version, and a caller-named document id is not protected from another writer")
	}
	if execCfg.Ingestion.DocumentIdentity == config.DocumentIdentityLegacy {
		slog.Warn("ADR-0138: execution.ingestion.document_identity=legacy: document ids omit the writer and a binary's bytes, so two principals, or two binary uploads with one tag or session, can share an id")
	}
	// ADR-0136 D4/A1: the fact extractor, constructed only when the operator turned
	// extraction on, so the default deployment carries no queue and no goroutine. It
	// writes through the enforcing writer above, never the raw substrate. The backfill
	// (cmd/facts-backfill) schedules the SAME extractor over stored documents, reading
	// chunk text through the enforcing read store.
	var factExtractor *memory.FactExtractor
	var factBackfill *memory.FactBackfill
	if execCfg.Facts.ExtractEnabled && factWriter != nil {
		if factGen == nil {
			factGen = gen
		}
		fcfg := factExtractorConfigFrom(execCfg.Facts)
		fcfg.EmbedModelID, fcfg.AssessorModel, fcfg.GeneratorModel = facts.EmbedModelID, facts.AssessorModel, facts.GeneratorModel
		// ENG-494 cost guard: which writers' ingests are extracted (empty = all).
		fcfg.ExtractionSources = append([]string(nil), execCfg.Memory.FactExtractionSources...)
		factExtractor = memory.NewFactExtractor(factGen, embed, factWriter, factItemReader, fcfg)
		ingestionMgr.SetFactExtractor(factExtractor)
		if facts.Lister != nil {
			factBackfill = memory.NewFactBackfill(facts.Lister, scopedRead, factExtractor)
		}
	}
	// NOTE: the legacy single-directory DirectoryWatcher (ADR-0028/0031) was removed
	// from the boot path — it delivered file events to a NoOpSignalReceiver (dead
	// weight) and its fixed-dir fsnotify watch errored at startup when InboxDir was
	// absent. On-demand + reactive watch sources (ADR-0032/REACT-06) supersede it.

	// The ADR-0094 D3 offline induction pass (ProcedureScheduler / ProcedureInducer)
	// was deleted 2026-11-05: it induced routines from `mnemonic_scene` metadata, and
	// scenes were cancelled (decisions/scenes-cancelled.md). Stored procedures are
	// still served by the WorkspaceStage procedure lane and still fed outcomes.

	return &MemoryStack{
		vecDB:                vec,
		readStore:            scopedRead,
		writeStore:           writeStore,
		eraser:               backend.Eraser,
		DocumentStore:        documentStore,
		ProfileStore:         profileStore,
		Agent:                memoryAgent,
		QueryService:         queryService,
		Embedder:             embed,
		WorkspaceStage:       ws,
		IngestionManager:     ingestionMgr,
		ChunkTripletsBatcher: chunkTripletsBatcher,
		FactWriter:           factWriter,
		FactReader:           factItemReader,
		FactInspector:        factInspector,
		FactExtractor:        factExtractor,
		FactBackfill:         factBackfill,
		FactReset:            facts.Resetter,
	}
}

// tieredStore composes the tiers into the whole port for a consumer that needs all
// three sides. Each side is a different tier; the embedded method sets are disjoint.
type tieredStore struct {
	domain.VectorReader
	domain.VectorWriter
	domain.VectorMaintenance
}

// Reader is tier 1, the fail-closed read chokepoint. Every principal-serving read
// uses it (AGENTS.md §2.2); the caller's predicate travels on SearchOptions.Scope and
// on ctx (domain.WithScope), and a read with neither is refused.
func (s *MemoryStack) Reader() domain.VectorReader { return s.readStore }

// Writer is tier 2, the classification chokepoint. Every principal-serving write uses
// it (AGENTS.md §2.2): the decision point's classification replaces the authored tags.
func (s *MemoryStack) Writer() domain.VectorWriter { return s.writeStore }

// KernelArtifactStore is tier 0, the raw adapter, for a kernel-authored artifact that
// serves no principal (the tool and skill indexes, for example). reason names the ADR
// that permits the use; it replaces the "comment naming the ADR" AGENTS.md §2.2 asked
// for, and TestKernelArtifactStoreCallSites lists every call site with its reason, so
// a new raw use is a visible test change. An empty reason panics: it is a programming
// error at boot, never a runtime condition.
func (s *MemoryStack) KernelArtifactStore(reason domain.ADRRef) domain.VectorStore {
	if !strings.HasPrefix(string(reason), "ADR-") {
		panic(fmt.Sprintf("kernel.MemoryStack.KernelArtifactStore: reason %q must name an ADR (\"ADR-NNNN ...\")", reason))
	}
	return s.vecDB
}

// Start launches the memory Agent's background workers and the ingestion pipeline.
func (s *MemoryStack) Start(ctx context.Context) error {
	// The ADR-0015 tier-2 drain that used to start here was deleted 2026-11-05 with the
	// raw capture path it served (decisions/scenes-cancelled.md).
	// The original ADR-0053 D2 LLM-lane EdgeBatcher used to be started here. It is no longer constructed
	// — see NewMemoryStack for why. The ADR-0053 batcher below is the one that
	// actually receives work from the ingest path.
	// ADR-0053 Phase 0: start the per-chunk (h, r, t) extraction batcher.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Start(ctx)
	}
	// ADR-0136 D4: the fact extractor's workers (nil-safe; nil unless extract_enabled).
	s.FactExtractor.Start(ctx)
	// ADR-0028: start the ingestion pipeline. (The legacy DirectoryWatcher that
	// used to start here was removed — see NewMemoryStack.)
	if s.IngestionManager != nil {
		s.IngestionManager.Start(ctx)
	}
	return s.Agent.StartMemoryWorker(ctx, false)
}

// Shutdown is a no-op for MemoryStack — its resources (the raw adapter) are closed by
// the Kernel which owns the infrastructure handle.
func (s *MemoryStack) Shutdown(_ context.Context) {
	slog.Info("🧠 MemoryStack: shutdown acknowledged")
	// ADR-0053 Phase 0: flush the ChunkTripletsBatcher's tail (Stop blocks until the
	// drain goroutine exits and the last batch is written). Best-effort: a failed
	// tail-flush is logged inside the batcher, not surfaced here.
	if s.ChunkTripletsBatcher != nil {
		s.ChunkTripletsBatcher.Stop()
	}
	// ADR-0136 D4: let in-flight extractions finish; queued ones are released (their
	// documents' chunks are stored, only their facts are missing). nil-safe.
	s.FactExtractor.Stop()
}

// factExtractorConfigFrom maps execution.facts onto the FactExtractor's settings. A
// function of its own so a test can prove each field is read (AGENTS.md §3.4).
func factExtractorConfigFrom(c config.FactsConfig) memory.FactExtractorConfig {
	return memory.FactExtractorConfig{
		QueueSize:           c.QueueSize,
		Workers:             c.Workers,
		LLMTimeout:          time.Duration(c.LLMTimeoutMs) * time.Millisecond,
		LLMTimeoutPerKChar:  time.Duration(c.LLMTimeoutPerKCharMs) * time.Millisecond,
		LLMTimeoutMax:       time.Duration(c.LLMTimeoutMaxMs) * time.Millisecond,
		ParseRetries:        c.ParseRetries,
		MaxFactsPerDocument: c.MaxFactsPerDocument,
		MaxPromptChars:      c.MaxPromptChars,
		MaxCallsPerDocument: c.MaxCallsPerDocument,
		MaxFactsPerSource:   c.MaxFactsPerSource,
		SingleAttributes:    append([]string(nil), c.SingleAttributes...),
		MultiAttributes:     append([]string(nil), c.MultiAttributes...),
		Supersession:        c.SupersessionEnabled,
		Assessments:         c.SupersessionEnabled && c.AssessmentsEnabled,
		FuzzyCandidates:     c.FuzzyCandidates,
		CardinalityMinVotes: c.CardinalityMinVotes,
	}
}

// FactSubstrate is the RAW knowledge substrate NewMemoryStack builds the ADR-0136 A1
// fact layer on: app.go constructs it (the kind registry carries core's memory_fact
// declaration) and NewMemoryStack wraps it. The zero value means no fact layer.
type FactSubstrate struct {
	// Store is postgres.PgKnowledgeStore (items, assessments, profiles, item
	// embeddings, extraction markers).
	Store domain.FactSubstrateStore
	// Reader is postgres.PgFactItemReader (SQL-side scope).
	Reader domain.FactItemReader
	// Inspector is the operator plane's read-only fact inspection — the same
	// postgres.PgFactItemReader, passed as its own port (no type assertion on Reader;
	// T-20261113-replace-optional-interface-probing-in-the). nil = no fact RPCs.
	Inspector domain.FactInspector
	// Lister selects backfill candidates (postgres.PgKnowledgeStore); nil = no backfill.
	Lister domain.FactBackfillLister
	// Resetter is the fact layer's hard reset (postgres.PgKnowledgeStore.ResetFacts),
	// handed in only by cmd/facts-reset; nil everywhere else.
	Resetter domain.FactResetter
	// EmbedModelID is the embedder identity of the item projection.
	EmbedModelID string
	// AssessorModel is the generator id of the facts role, part of every assessment's
	// authority_version.
	AssessorModel string
	// GeneratorModel is the facts generator's model (FactsGeneratorModel), part of the
	// extractor version on every extraction marker (ADR-0136 A2.4).
	GeneratorModel string
	// Documents is the ADR-0138 document lifecycle (postgres.PgDocumentLifecycle): the
	// identity lookup, the cross-writer refusal and the retirement of a changed
	// document's previous version. It is carried here because its cascade erases the
	// document's fact items through this substrate's EraseItemsTx in the SAME
	// transaction as its chunks. nil = ids are derived with no lookup and nothing is
	// retired (logged by NewMemoryStack).
	Documents domain.DocumentLifecycleStore
}

// FactsGeneratorModel names the model behind the "facts" system role for the ADR-0136
// A2.4 extractor version: the generator the role names (else the provider's default
// generator), reported by its configured model, else by its id, else "default". A
// different model is a different extractor, so its markers read as pending.
func FactsGeneratorModel(p config.LLMProviderConfig, roles map[string]string) string {
	id := roles[string(domain.PurposeFacts)]
	if id == "" {
		id = p.Roles[string(domain.PurposeFacts)]
	}
	if id == "" {
		id = p.Default
	}
	for _, g := range p.Generators {
		if g.ID == id && g.Model != "" {
			return g.Model
		}
	}
	if id == "" {
		return "default"
	}
	return id
}

// chunkTripletsBatcherConfigFrom maps the execution.ingestion block onto the
// ChunkTripletsBatcher's settings. A function of its own so a test can prove
// each field is read (AGENTS.md §3.4).
func chunkTripletsBatcherConfigFrom(c config.IngestionConfig) memory.ChunkTripletsBatcherConfig {
	return memory.ChunkTripletsBatcherConfig{
		QueueSize:  c.EdgeExtractionQueueSize,
		BatchSize:  c.EdgeExtractionBatchSize,
		MaxIdle:    time.Duration(c.EdgeExtractionMaxIdleMs) * time.Millisecond,
		LLMTimeout: time.Duration(c.EdgeExtractionLLMTimeoutMs) * time.Millisecond,
		// T-20261102-kg-extractor-cold-start-drops-the-first-kg: chunks whose
		// batch failed because the extractor was unavailable get this many more
		// passes before they settle as kg_failed.
		MaxRequeues: c.KgTripletsMaxRequeues,
		// ADR-0139 C1/C4: the LLM extractor's reply format; "" and "completion"
		// keep the KG²RAG completion prompt until an A/B/A run moves the default.
		StructuredLLMFormat: c.KgTripletsLLMFormat == config.KgTripletsFormatStructured,
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
		// T-20261102-async-kg-triplet-extraction-30-chunks-s-lags: a sync ingest
		// may wait for its own chunks' KG triplets (default off).
		SyncWaitForKG:     c.SyncIngestWaitForKG,
		SyncKGWaitTimeout: time.Duration(c.SyncIngestKGWaitTimeoutMs) * time.Millisecond,
		// ADR-0136 D4: a sync ingest may wait for its document's facts (default off).
		SyncWaitForFacts:     c.SyncIngestWaitForFacts,
		SyncFactsWaitTimeout: time.Duration(c.SyncIngestFactsWaitTimeoutMs) * time.Millisecond,
		// ADR-0138 D2: the document-id scheme ("" reads as v2).
		LegacyDocumentIdentity: c.DocumentIdentity == config.DocumentIdentityLegacy,
	}
}
