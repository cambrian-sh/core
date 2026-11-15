package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/evidence"
)

type IngestionConfig struct {
	QueueSize int
	BatchSize int
	Workers   int
	BatchWait time.Duration
	// ContextPrepend prepends "<title> › <section breadcrumb>" to each chunk's
	// body before embedding/storage (execution.ingestion.chunk_context_prepend).
	ContextPrepend bool
	// JunkFilterBackends lists the structure-parser backends whose junk-classified
	// leaves are dropped (execution.ingestion.structure_junk_filter_backends). Empty
	// = drop nothing anywhere: content is kept unless the operator opted in.
	JunkFilterBackends []string
	// SyncWaitForKG makes ProcessSync return only after its own chunks' KG
	// triplets are extracted and persisted (read-your-writes for the KG lane;
	// execution.ingestion.sync_ingest_wait_for_kg, owner decision 2026-11-02).
	// false = the async lane as before: ProcessSync returns once chunks are
	// stored and extraction catches up in the background.
	SyncWaitForKG bool
	// SyncKGWaitTimeout bounds that wait (execution.ingestion.
	// sync_ingest_kg_wait_timeout_ms). On expiry the ingest still SUCCEEDS — the
	// content is stored and the KG lane is enrichment — with a WARN and
	// kg_pending in the timing line. <=0 means the 30 s default.
	SyncKGWaitTimeout time.Duration
	// SyncWaitForFacts makes ProcessSync return only after its document's ADR-0136
	// facts are extracted and stored (execution.ingestion.sync_ingest_wait_for_facts):
	// read-your-writes for the fact lane. It applies to EVERY ProcessSync caller alike —
	// agent-plane IngestMemory, operator ingest and plugin/reactive ingests (owner
	// decision 2026-11-14, ENG-488): there is one ingest path, so one wait rule.
	// false = async, as the KG lane. A no-op without a fact extractor.
	SyncWaitForFacts bool
	// SyncFactsWaitTimeout bounds that wait (sync_ingest_facts_wait_timeout_ms), so a
	// stuck extractor can never hang an ingest. On expiry ProcessSync returns
	// ErrFactsNotReady (the content IS stored; a retry is an idempotent no-op that
	// waits again). <=0 means the 120 s default.
	SyncFactsWaitTimeout time.Duration
	// LegacyDocumentIdentity derives document ids exactly as before ADR-0138
	// (execution.ingestion.document_identity = "legacy"): no writer, Body-only digest,
	// no grandfathering lookup. Default false (scheme v2).
	LegacyDocumentIdentity bool
}

// ErrFactsNotReady is returned by ProcessSync when sync_ingest_wait_for_facts is on and
// the document's facts were not stored within sync_ingest_facts_wait_timeout_ms (or the
// extractor stopped first). The ingest's content IS stored — chunks, entity, evidence —
// and a retry is safe (deterministic ids); only the read-your-writes guarantee for the
// fact lane was not met. Owner decision 2026-11-14 (ENG-488): a bounded wait with a
// clear timeout error, never a silent success and never an unbounded hang.
var ErrFactsNotReady = errors.New("facts not stored within the sync wait (the document content is stored; retry is safe)")

// defaultSyncFactsWaitTimeout is the fallback for IngestionConfig.SyncFactsWaitTimeout:
// one LLM call per extraction window (ADR-0136 A2.3), so it is longer than the KG wait.
const defaultSyncFactsWaitTimeout = 120 * time.Second

// defaultSyncKGWaitTimeout is the fallback for IngestionConfig.SyncKGWaitTimeout.
const defaultSyncKGWaitTimeout = 30 * time.Second

type IngestionManager struct {
	queue    chan queuedDoc
	embedder domain.Embedder
	agent    *Agent
	registry *Registry
	cfg      IngestionConfig
	// tripletsBatcher enqueues each persisted chunk for per-chunk (h, r, t) +
	// anchor extraction (ADR-0053). nil = no KG enrichment (legacy). Without it,
	// uploaded-document chunks never populate chunk_triplets, so KG2RAG expansion,
	// query-entity seeding, and anchor promotion all no-op on the ingest path.
	tripletsBatcher *ChunkTripletsBatcher
	// factExtractor queues each stored document for ADR-0136 D4 fact extraction (one
	// async LLM call per document). nil = the fact layer is off (the default).
	factExtractor *FactExtractor
	// structureParser + structureStore build the document-structure graph
	// (ADR-0060): section nodes + PART_OF/NEXT edges, and every chunk inherits
	// its section path. Both nil = structure graph disabled.
	structureParser StructureParser
	structureStore  StructureGraphStore
	// documentStore records the source-document entity (ADR-0093). Optional.
	documentStore domain.DocumentStore
	// lifecycle owns document identity checks and the version chain (ADR-0138 D2–D4):
	// the grandfathering lookup, the cross-writer refusal and the retirement of a changed
	// document's previous version. nil (tests, tools without a database) = ids are derived
	// without a lookup and nothing is retired, logged once at boot by the composer.
	lifecycle domain.DocumentLifecycleStore
	// bus publishes MemoryWrittenEvent so an ingest reaches the operator feed
	// (ADR-0047 D3). nil ⇒ no-op.
	//
	// This lived only on RememberService until 2026-07-31 — which sat on the
	// unreachable raw-write fallback, so the operator's memory feed had a
	// publisher wired to a dead path and a consumer receiving nothing.
	bus domain.EventBus
	// evidenceCapture preserves the delivery as immutable evidence (ADR-0105)
	// before ANY semantic processing touches it.
	// nil = the substrate's evidence foundation is disabled (the default).
	evidenceCapture EvidenceCapture
	// structureLeavesDropped counts structure leaves dropped as junk since start
	// (StructureLeavesDropped). See structure_junk.go.
	structureLeavesDropped atomic.Int64
}

// EvidenceCapture is the evidence foundation's write seam (ADR-0105 D6).
type EvidenceCapture interface {
	Ingest(ctx context.Context, raw evidence.Raw) (domain.EvidenceID, bool, error)
}

// SetEvidenceCapture wires the evidence foundation. Call before Start; nil is
// ignored. Once set, a capture failure FAILS the ingest: a lane that accepted
// content and cannot preserve it must not pretend it did.
func (im *IngestionManager) SetEvidenceCapture(c EvidenceCapture) {
	if im != nil && c != nil {
		im.evidenceCapture = c
	}
}

// plural keeps the feed readable: "1 chunks" is the kind of small wrongness that
// makes an operator trust a surface slightly less.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// SetEventBus wires the operator feed. Call before Start; nil is ignored.
func (im *IngestionManager) SetEventBus(bus domain.EventBus) {
	if im != nil {
		im.bus = bus
	}
}

// publishWritten emits ONE event per ingested DOCUMENT, keyed on the source-doc
// entity id.
//
// Per document, not per chunk, and the choice is deliberate: a chunk is an
// internal unit of retrieval, so a 200-chunk upload would put 200 rows on an
// operator's feed describing one action they took. The document is the thing the
// operator did; the chunk count is detail, and it rides in the summary.
func (im *IngestionManager) publishWritten(doc domain.ExternalDocument, entityID string, chunkCount int) {
	if im == nil || im.bus == nil || entityID == "" {
		return
	}
	summary := doc.Title
	if summary == "" {
		summary = doc.SourceURI
	}
	_ = im.bus.Publish(domain.MemoryWrittenEvent{
		DocID: entityID,
		// The SOURCE DOCUMENT, not a chunk type: the id is the source-doc entity and
		// the event describes the document the operator ingested.
		DocType: sourceDocumentMarker,
		// The ingest THREAD, not a task session — an ingestion thread is not a run
		// (see chunkMetadata), and reporting it as one would make every upload look
		// like the output of an execution on the feed.
		SessionID: doc.ThreadID,
		Source:    doc.SourceURI,
		Summary:   fmt.Sprintf("%s (%d %s)", summary, chunkCount, plural(chunkCount, "chunk")),
	})
}

// SetChunkTripletsBatcher wires the per-chunk triplet/anchor extractor onto the
// document-ingest path (mirrors RememberService.SetChunkTripletsBatcher). Call
// before Start; nil is ignored. Enqueue is non-blocking + nil-safe.
func (im *IngestionManager) SetChunkTripletsBatcher(b *ChunkTripletsBatcher) {
	if im != nil {
		im.tripletsBatcher = b
	}
}

// SetFactExtractor wires the ADR-0136 D4 fact extractor onto the ingest path (the
// SetChunkTripletsBatcher pattern: an enrichment lane, not an enforcement point - the
// extractor writes through the enforcing fact store it was constructed with). Call
// before Start; nil is ignored.
func (im *IngestionManager) SetFactExtractor(e *FactExtractor) {
	if im != nil && e != nil {
		im.factExtractor = e
	}
}

// SetStructureGraph wires the structure-aware parser (docling_agent) + the graph
// store onto the ingest path (ADR-0060). Both required; a nil pair is ignored.
func (im *IngestionManager) SetStructureGraph(parser StructureParser, store StructureGraphStore) {
	if im != nil && parser != nil && store != nil {
		im.structureParser = parser
		im.structureStore = store
	}
}

// fallbackRegistry is the back-compat floor: option_c only, no routes.
//
// It is reached when the caller supplied no registry at all. It is NOT the
// configured path — NewIngestionManager builds a real registry from the
// operator's config.ChunkerConfig. This used to be `defaultRegistry()` and it
// WAS the only path: every deployment ran option_c regardless of what the
// `chunker` block said, because the routing table was this Go literal.
func fallbackRegistry() *Registry {
	reg, err := NewRegistry(
		map[string]domain.Chunker{OptionCChunker{}.Name(): OptionCChunker{}},
		config.ChunkerConfig{Default: OptionCChunker{}.Name()},
	)
	if err != nil {
		// Unreachable: the map and the default are both literals defined right
		// here. Panicking beats returning nil, which would nil-deref at the first
		// Resolve on the ingest path instead of at the line that is wrong.
		panic(fmt.Sprintf("memory: fallback chunker registry is invalid: %v", err))
	}
	return reg
}

// NewIngestionManager builds the manager with a chunker registry derived from the
// OPERATOR'S configuration: every known chunker is registered and chunkerCfg
// supplies the default plus the sourceType/ext routing table.
//
// An invalid chunkerCfg is a startup error, not a silent downgrade: the kernel's
// config load runs config.ChunkerConfig.Validate (via Config.Validate), so a
// config that names an unknown chunker never reaches this constructor on the
// boot path. NewRegistry re-checks against the chunkers actually registered; if
// that still fails (a direct caller with a hand-built config), the error is
// logged and the manager falls back to option_c so ingestion still runs. The
// log line is the loud part.
func NewIngestionManager(embedder domain.Embedder, agent *Agent, cfg IngestionConfig, chunkerCfg config.ChunkerConfig) *IngestionManager {
	registry := fallbackRegistry()
	if chunkerCfg.Default != "" {
		reg, err := NewRegistry(NewDefaultChunkers(embedder, chunkerCfg), chunkerCfg)
		if err != nil {
			slog.Error("IngestionManager: invalid chunker config; falling back to option_c",
				"err", err, "default", chunkerCfg.Default)
		} else {
			registry = reg
			// ADR-0060 D6: the late chunker is opt-in. The gate is consulted per
			// Resolve so a route naming "late" degrades to the default while the
			// gate is closed, rather than being rejected at startup.
			enabled := chunkerCfg.Late.Enabled
			registry.SetLateGate(func() bool { return enabled })
		}
	}
	return NewIngestionManagerWithRegistry(embedder, agent, registry, cfg)
}

// chunkerFor resolves doc's chunker through the registry. The policy matches
// NewIngestionManager's: a resolution failure is LOGGED and the document is
// chunked with the documented default (option_c) rather than dropped. Resolve
// fails only when the default chunker is unregistered, which NewRegistry already
// refuses at construction, so this is a guard against a future registry bug, not
// an expected path — and it must never be a silent one (AGENTS.md §3.2: a
// discarded resolver error is how a misconfiguration degrades unseen).
func (im *IngestionManager) chunkerFor(doc domain.ExternalDocument) domain.Chunker {
	ext := docExt(doc.SourceURI)
	chunker, err := im.registry.Resolve(doc.SourceType, ext)
	if err != nil || chunker == nil {
		slog.Error("IngestionManager: chunker resolution failed; chunking with option_c",
			"err", err, "source_type", doc.SourceType, "ext", ext, "source_uri", doc.SourceURI)
		return OptionCChunker{}
	}
	return chunker
}

func NewIngestionManagerWithRegistry(embedder domain.Embedder, agent *Agent, registry *Registry, cfg IngestionConfig) *IngestionManager {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 5
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 5
	}
	if cfg.BatchWait <= 0 {
		cfg.BatchWait = time.Second
	}
	if cfg.SyncKGWaitTimeout <= 0 {
		cfg.SyncKGWaitTimeout = defaultSyncKGWaitTimeout
	}
	if cfg.SyncFactsWaitTimeout <= 0 {
		cfg.SyncFactsWaitTimeout = defaultSyncFactsWaitTimeout
	}
	if registry == nil {
		registry = fallbackRegistry()
	}
	return &IngestionManager{queue: make(chan queuedDoc, cfg.QueueSize), embedder: embedder, agent: agent, registry: registry, cfg: cfg}
}

// queuedDoc is one asynchronously ingested document with the principal that
// pushed it. The batch workers run on the manager's own context, so the writer's
// identity has to travel WITH the document: without it every batch write reached
// the classification chokepoint as no one (ENG-494) — refused by a premium decision
// point, and its facts actorless.
type queuedDoc struct {
	doc       domain.ExternalDocument
	principal domain.PrincipalRef
	// origin is when the queue accepted the document (or the earlier origin the
	// pusher's ctx carried): the ADR-0137 D5 fence refuses its writes if the document
	// was erased after that, so a queued ingest that predates an erase never restores it.
	origin time.Time
}

// Enqueue queues doc for the batch lane, carrying the principal on ctx (the
// authenticated pusher: /v1/ingest stamps it) to the worker that writes it.
// Non-blocking: false when the queue is full.
func (im *IngestionManager) Enqueue(ctx context.Context, doc domain.ExternalDocument) bool {
	select {
	case im.queue <- queuedDoc{doc: doc, principal: domain.PrincipalFromContext(ctx), origin: ingestOrigin(ctx)}:
		return true
	default:
		slog.Warn("IngestionManager: queue full, dropping document", "source_uri", doc.SourceURI)
		return false
	}
}

// ProcessSync processes a single document synchronously: chunk it
// via the registry, mint a source-doc entity, ingest every chunk
// with chunk_relations populated, and return the source-doc entity
// ID. Used by the gRPC IngestMemory path so a synchronous RPC call
// gets the entity ID back without waiting for the batch loop's
// BatchWait window.
//
// This is the entry point the harness uses when the gRPC handler
// wants "treat this IngestMemory as a document": the caller
// passes the full body, the IngestionManager handles chunking +
// source-doc entity minting + chunk ingestion. The DirectoryWatcher
// (ADR-0028) still feeds the same manager via Enqueue, so both
// paths share the chunker registry + agent
// write path.
func (im *IngestionManager) ProcessSync(ctx context.Context, doc domain.ExternalDocument) (entityID string, err error) {
	// ADR-0137 D5: the ingest's origin — when this act was accepted, or the earlier
	// origin a replaying door stamped — rides ctx to every write of the document, its
	// triplet batch and its fact job, so a write that predates an erase of the document
	// is refused instead of resurrecting it.
	ctx = domain.EnsureIngestOrigin(ctx)
	// One timing line per document (ingestion_timing.go). The clock rides ctx so
	// the structure dispatcher and the store adapter can add what only they see.
	started := time.Now()
	ctx, clock := ingestClock(ctx)
	stored := 0
	defer func() { logIngestTiming(ctx, clock, time.Since(started), doc.SourceURI, stored, err) }()
	// ADR-0138 D1.4: the document's identity, resolved ONCE for every step below.
	ident := im.resolveDocumentID(ctx, doc)
	report := domain.IngestOutcomeFrom(ctx)
	// ADR-0138 B1.1: an operator upload this writer already stores byte for byte is a
	// no-op — no evidence, no chunks, no derivatives — so the same file uploaded in
	// another session or under another tag is one document, not a second copy that
	// crowds retrieval. Only the association (session, scope) is recorded. Checked
	// before evidence capture: nothing of a duplicate is written anywhere.
	if dup := im.uploadDuplicate(ctx, ident); dup.Exists {
		im.recordUpload(ctx, doc, ident, dup.ID)
		if report != nil {
			report.DocumentID, report.DuplicateOf, report.Version = dup.ID, dup.ID, dup.Version
		}
		slog.InfoContext(ctx, "IngestionManager: duplicate upload; nothing written",
			"upload", doc.UploadName, "duplicate_of", dup.ID, "resolved", ident.ID)
		return dup.ID, nil
	}
	// Evidence first (ADR-0105 D2): the original bytes must be durable and
	// published as evidence before chunking or any other
	// semantic step — so a failure anywhere downstream still leaves source
	// material that can be reprocessed.
	if im.evidenceCapture != nil {
		t := time.Now()
		evID, err := im.captureEvidence(ctx, doc, ident.ID)
		clock.Since(StageEvidence, t)
		if err != nil {
			return "", fmt.Errorf("ingestion manager: evidence capture: %w", err)
		}
		// ADR-0136 A1.6: the evidence id rides ctx to the fact job, so every
		// memory_fact item links to the evidence revision it was extracted from (a
		// changed document is a new revision, ADR-0105 D4).
		ctx = withIngestEvidence(ctx, evID)
	}
	// Chunks FIRST, entity LAST (T-20261031-processsync-leaves-the-source-doc-entity-stub).
	// The entity id is deterministic (source_doc:<uri>) and the chunks carry it as a
	// string, not a foreign key, so the order is free — and this order means a
	// failed chunk write (store down, every embed failed) leaves NOTHING behind: no
	// entity stub that a reader could find for a document the caller was told
	// FAILED. The inverse partial state (chunks stored, entity mint failed) is
	// returned as an error below and is self-healing on retry, because every id
	// involved is deterministic and a retry overwrites both. Rolling the entity
	// back after a failed chunk write was the rejected alternative: it is one more
	// store call against a store that just failed.
	entityID = sourceDocEntityID(doc)
	t := time.Now()
	chunker := im.chunkerFor(doc)
	chunks, err := chunker.Chunk(ctx, &doc)
	if err != nil {
		slog.Warn("IngestionManager: chunker failed, falling back to OptionC", "source_uri", doc.SourceURI, "err", err)
		chunks, _ = OptionCChunker{}.Chunk(ctx, &doc)
	}
	clock.Since(StageChunk, t)
	outcome, err := im.persistChunksTracked(ctx, doc, ident, chunks, entityID, im.cfg.SyncWaitForKG, im.cfg.SyncWaitForFacts)
	chunkCount := outcome.Stored
	stored = chunkCount
	if err != nil {
		// Includes ErrNoChunksStored: an ingest that stored no content is a FAILED
		// ingest, and the caller is told so rather than handed an entity id
		// (T-20261031-processsync-reports-success-when-every-chunk).
		return "", err
	}
	t = time.Now()
	_, _, err = im.mintSourceDoc(ctx, doc, ident.ID)
	clock.Since(StageEntity, t)
	if err != nil {
		return "", fmt.Errorf("ingestion manager: source-doc entity for %q (its %d chunk(s) are stored; a retry overwrites both by id): %w",
			doc.SourceURI, chunkCount, err)
	}
	im.recordUpload(ctx, doc, ident, ident.ID)
	if report != nil {
		report.DocumentID, report.Version, report.Replaced = ident.ID, max(outcome.Version, 1), outcome.Replaced
	}
	if chunkCount == 0 {
		return entityID, nil
	}
	// Read-your-writes for the KG lane (sync_ingest_wait_for_kg): return only once
	// this document's chunks have their triplets. After the entity mint, so the
	// wait overlaps nothing the caller still needs and a failed mint never waits.
	im.waitForKG(ctx, clock, doc.SourceURI, outcome.KGWait)
	// Read-your-writes for the fact lane (sync_ingest_wait_for_facts), the same shape,
	// except that a timed-out wait is an ERROR (ENG-488). The content is stored either
	// way, so the written event is still published before the error is returned.
	factsErr := im.waitForFacts(ctx, clock, doc.SourceURI, outcome.FactsWait)
	slog.Info("IngestionManager: sync ingest complete", "source_uri", doc.SourceURI, "entity_id", entityID, "chunk_count", chunkCount,
		"dropped_leaves", outcome.DroppedLeaves)
	t = time.Now()
	im.publishWritten(doc, entityID, chunkCount)
	clock.Since(StagePublish, t)
	if factsErr != nil {
		return entityID, factsErr
	}
	return entityID, nil
}

// captureEvidence maps one ExternalDocument onto the evidence write path.
//
// SourceKey is the document's stable identity (the same externalDocumentID the
// rest of the pipeline uses), and SourceRevision is the content digest: a
// replayed identical delivery dedupes, while the SAME key arriving with CHANGED
// bytes becomes a NEW evidence revision rather than an update (ADR-0105 D4) —
// which is exactly the re-ingest-a-changed-file case.
func (im *IngestionManager) captureEvidence(ctx context.Context, doc domain.ExternalDocument, documentID string) (domain.EvidenceID, error) {
	body := doc.Data
	if len(body) == 0 {
		body = []byte(doc.Body)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("document carries no bytes")
	}
	sourceID := doc.SourceType
	if sourceID == "" {
		sourceID = "unknown"
	}
	digest := sha256.Sum256(body)
	// The id is returned (ADR-0136 A1.6): it used to be discarded here, which left the
	// fact layer no way to link an item to its evidence.
	id, _, err := im.evidenceCapture.Ingest(ctx, evidence.Raw{
		SourceID:       sourceID,
		SourceKey:      documentID,
		SourceRevision: hex.EncodeToString(digest[:]),
		SourceTime:     doc.Timestamp,
		Bytes:          body,
		Classification: doc.Tags,
	})
	return id, err
}

func (im *IngestionManager) Start(ctx context.Context) {
	jobs := make(chan []queuedDoc, im.cfg.Workers*2)
	go im.batchLoop(ctx, jobs)
	for range im.cfg.Workers {
		go im.worker(ctx, jobs)
	}
}

func (im *IngestionManager) batchLoop(ctx context.Context, jobs chan<- []queuedDoc) {
	var batch []queuedDoc
	ticker := time.NewTicker(im.cfg.BatchWait)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b := batch
		batch = nil
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case doc, ok := <-im.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, doc)
			if len(batch) >= im.cfg.BatchSize {
				flush()
				ticker.Reset(im.cfg.BatchWait)
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (im *IngestionManager) worker(ctx context.Context, jobs <-chan []queuedDoc) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch, ok := <-jobs:
			if !ok {
				return
			}
			im.processBatch(ctx, batch)
		}
	}
}

func (im *IngestionManager) processBatch(ctx context.Context, batch []queuedDoc) {
	for _, q := range batch {
		dctx := domain.WithIngestOrigin(ctx, q.origin)
		if !q.principal.IsZero() {
			dctx = domain.WithPrincipal(dctx, q.principal)
		}
		im.processBatchDoc(dctx, q.doc)
	}
}

// processBatchDoc is one document of processBatch, with its own timing line.
// Same order as ProcessSync: chunks first, entity last.
func (im *IngestionManager) processBatchDoc(ctx context.Context, doc domain.ExternalDocument) {
	started := time.Now()
	// A fresh clock per document: the batch shares one ctx across documents.
	clock := domain.NewStageClock()
	dctx := domain.WithStageClock(ctx, clock)
	var (
		outcome persistOutcome
		err     error
	)
	defer func() { logIngestTiming(dctx, clock, time.Since(started), doc.SourceURI, outcome.Stored, err) }()
	entityID := sourceDocEntityID(doc)
	ident := im.resolveDocumentID(dctx, doc) // ADR-0138 D1.4: once per document
	// ADR-0138 B1.1, the same rule as ProcessSync (only operator uploads carry an
	// UploadName, and they arrive synchronously; kept here so the rule is the
	// document's, not the path's).
	if dup := im.uploadDuplicate(dctx, ident); dup.Exists {
		im.recordUpload(dctx, doc, ident, dup.ID)
		slog.Info("IngestionManager: duplicate upload; nothing written", "upload", doc.UploadName, "duplicate_of", dup.ID)
		return
	}
	t := time.Now()
	chunker := im.chunkerFor(doc)
	chunks, cerr := chunker.Chunk(dctx, &doc)
	if cerr != nil {
		slog.Warn("IngestionManager: chunker failed, falling back to OptionC", "source_uri", doc.SourceURI, "err", cerr)
		chunks, _ = OptionCChunker{}.Chunk(dctx, &doc)
	}
	clock.Since(StageChunk, t)
	outcome, err = im.persistChunksTracked(dctx, doc, ident, chunks, entityID, false, false) // async path: never waits for the KG or fact lane
	if err != nil {
		slog.Warn("IngestionManager: failed to persist chunks", "source_uri", doc.SourceURI, "err", err)
		return
	}
	t = time.Now()
	_, _, err = im.mintSourceDoc(dctx, doc, ident.ID)
	clock.Since(StageEntity, t)
	if err != nil {
		slog.Warn("IngestionManager: failed to mint source-doc entity", "source_uri", doc.SourceURI, "err", err)
		return
	}
	im.recordUpload(dctx, doc, ident, ident.ID)
	slog.Info("ExternalDocumentIngested", "source_uri", doc.SourceURI, "entity_id", entityID, "chunk_count", outcome.Stored,
		"dropped_leaves", outcome.DroppedLeaves)
}

// waitForKG blocks until w completes, ctx is done, or SyncKGWaitTimeout expires,
// and records the wait as the kg_wait stage. It never fails the ingest: the
// chunks are already stored and the KG lane is enrichment, so an incomplete
// wait is a WARN plus kg_pending (chunks not yet settled) and kg_failed (chunks
// whose triplets were not persisted) on the timing line and trailer. Cancelling
// ctx (a client disconnect) ends the wait; the chunks stay queued and are
// extracted as usual. w == nil (flag off, or no batcher) is a no-op, so the
// flag-off path records no stage at all.
func (im *IngestionManager) waitForKG(ctx context.Context, clock *domain.StageClock, sourceURI string, w *TripletsWait) {
	if w == nil {
		return
	}
	t := time.Now()
	wctx, cancel := context.WithTimeout(ctx, im.cfg.SyncKGWaitTimeout)
	err := w.Wait(wctx)
	cancel()
	clock.Since(StageKGWait, t)
	pending, failed := w.Pending(), w.Failed()
	if pending > 0 {
		clock.Count(CountKGPending, pending)
	}
	if failed > 0 {
		clock.Count(CountKGFailed, failed)
	}
	if err == nil && failed == 0 {
		return
	}
	reason := "incomplete"
	switch {
	case errors.Is(err, ErrTripletsBatcherStopped):
		reason = "batcher_stopped"
	case ctx.Err() != nil:
		reason = "caller_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case failed > 0:
		reason = "extraction_failed"
	}
	slog.WarnContext(ctx, "IngestionManager: sync ingest returned before its KG triplets were complete",
		"source_uri", sourceURI, "reason", reason, "kg_pending", pending, "kg_failed", failed,
		"waited_ms", time.Since(t).Milliseconds(), "timeout_ms", im.cfg.SyncKGWaitTimeout.Milliseconds(),
		"effect", "the content is stored; KG2RAG, entity seeding and anchors see this document's graph once extraction catches up")
}

// waitForFacts is waitForKG for the ADR-0136 fact extractor: it blocks until this
// document's facts are stored, ctx is done, or SyncFactsWaitTimeout expires, and records
// facts_wait. A wait that ran out of time, or whose extractor stopped, returns
// ErrFactsNotReady (owner decision 2026-11-14, ENG-488: a clear timeout error); an
// extraction that FINISHED with failures, or a caller that went away, only warns.
// w == nil (flag off, or no extractor) is a no-op.
func (im *IngestionManager) waitForFacts(ctx context.Context, clock *domain.StageClock, sourceURI string, w *FactsWait) error {
	if w == nil {
		return nil
	}
	t := time.Now()
	wctx, cancel := context.WithTimeout(ctx, im.cfg.SyncFactsWaitTimeout)
	err := w.Wait(wctx)
	cancel()
	clock.Since(StageFactsWait, t)
	if !w.Done() {
		clock.Count(CountFactsPending, 1)
	}
	if w.Failed() {
		clock.Count(CountFactsFailed, 1)
	}
	if err == nil && !w.Failed() {
		return nil
	}
	reason := "incomplete"
	switch {
	case errors.Is(err, ErrFactExtractorStopped):
		reason = "extractor_stopped"
	case ctx.Err() != nil:
		reason = "caller_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case w.Failed():
		reason = "extraction_failed"
	}
	slog.WarnContext(ctx, "IngestionManager: sync ingest returned before its facts were stored",
		"source_uri", sourceURI, "reason", reason, "waited_ms", time.Since(t).Milliseconds(),
		"timeout_ms", im.cfg.SyncFactsWaitTimeout.Milliseconds(),
		"effect", "the content is stored; the fact lane sees this document once extraction catches up")
	switch reason {
	case "timeout", "extractor_stopped":
		return fmt.Errorf("%w: %s after %d ms (timeout %d ms), document %q",
			ErrFactsNotReady, reason, time.Since(t).Milliseconds(), im.cfg.SyncFactsWaitTimeout.Milliseconds(), sourceURI)
	}
	return nil
}

// sourceDocEntityID is the deterministic id of a document's source-doc entity.
// It is computed, not looked up, so persistChunks can stamp it on every chunk
// BEFORE the entity row exists (see ProcessSync for why that order matters).
func sourceDocEntityID(doc domain.ExternalDocument) string {
	return "source_doc:" + doc.SourceURI
}

// mintSourceDoc writes the source-doc entity (and its CAS body). It returns the
// content id, the entity id, and an error when the entity could not be saved —
// callers decide whether that is fatal (ProcessSync: yes, the caller is told)
// or logged (processBatch).
func (im *IngestionManager) mintSourceDoc(ctx context.Context, doc domain.ExternalDocument, documentID string) (string, string, error) {
	if im.agent == nil || im.agent.Manager == nil || im.agent.Manager.Store == nil {
		return "", "", fmt.Errorf("ingestion manager: no memory manager configured")
	}
	var cid string
	if im.agent.ContentStore != nil && len(doc.Body) > 0 {
		if c, err := im.agent.ContentStore.Put(ctx, []byte(doc.Body), "source_document", nil, buildBodyPreview(doc.Body, 150)); err == nil {
			cid = string(c)
		}
	}
	entityID := sourceDocEntityID(doc)
	meta := map[string]interface{}{"kind": "source_document", "canonical_id": doc.SourceURI, "source_uri": doc.SourceURI, "source_type": doc.SourceType, "title": doc.Title, "author": doc.Author, "document_id": documentID}
	if len(doc.Tags) > 0 {
		meta["tags"] = append([]string(nil), doc.Tags...)
	}
	if cid != "" {
		meta["content_cid"] = cid
	}
	// ADR-0134: the entity carries the DOCUMENT's event stamp (its time and whole
	// span) in the typed columns, like its chunks; no metadata copy.
	if err := im.agent.Manager.Save(ctx, &domain.Document{
		ID: entityID, DocumentType: domain.DocTypeMnemonicEntity,
		Text: doc.Title, ActivationStrength: 0.1, Metadata: meta,
		EventStamp: doc.StampOf(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		return cid, "", fmt.Errorf("mint source-doc entity: %w", err)
	}
	return cid, entityID, nil
}

func docExt(uri string) string {
	dot := strings.LastIndex(uri, ".")
	if dot < 0 || dot >= len(uri)-1 {
		return ""
	}
	return strings.ToLower(uri[dot:])
}

func clip(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ingestOrigin is the origin ctx carries, else now (the queue's acceptance time).
func ingestOrigin(ctx context.Context) time.Time {
	t, _ := domain.IngestOriginFrom(domain.EnsureIngestOrigin(ctx))
	return t
}
