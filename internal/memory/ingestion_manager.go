package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
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
}

type IngestionManager struct {
	queue    chan domain.ExternalDocument
	sceneGen *SceneGenerator
	embedder domain.Embedder
	agent    *Agent
	registry *Registry
	cfg      IngestionConfig
	// tripletsBatcher enqueues each persisted chunk for per-chunk (h, r, t) +
	// anchor extraction (ADR-0053). nil = no KG enrichment (legacy). Without it,
	// uploaded-document chunks never populate chunk_triplets, so KG2RAG expansion,
	// query-entity seeding, and anchor promotion all no-op on the ingest path.
	tripletsBatcher *ChunkTripletsBatcher
	// structureParser + structureStore build the document-structure graph
	// (ADR-0060): section nodes + PART_OF/NEXT edges, and every chunk inherits
	// its section path. Both nil = structure graph disabled.
	structureParser StructureParser
	structureStore  StructureGraphStore
	// documentStore records the source-document entity (ADR-0093). Optional.
	documentStore DocumentStore
	// sceneGenEnabled gates the per-item scene-generation LLM call on the ingest
	// hot path (ADR-0049 episodic scenes). Default OFF: it stalls ingest when no
	// LLM is reachable and is not needed for document/structure retrieval.
	sceneGenEnabled bool
	// bus publishes MemoryWrittenEvent so an ingest reaches the operator feed
	// (ADR-0047 D3). nil ⇒ no-op.
	//
	// This lived only on RememberService until 2026-07-31 — which sat on the
	// unreachable raw-write fallback, so the operator's memory feed had a
	// publisher wired to a dead path and a consumer receiving nothing.
	bus domain.EventBus
	// evidenceCapture preserves the delivery as immutable evidence (ADR-0105)
	// before ANY semantic processing — including scene generation — touches it.
	// nil = the substrate's evidence foundation is disabled (the default).
	evidenceCapture EvidenceCapture
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

// SetStructureGraph wires the structure-aware parser (docling_agent) + the graph
// store onto the ingest path (ADR-0060). Both required; a nil pair is ignored.
func (im *IngestionManager) SetStructureGraph(parser StructureParser, store StructureGraphStore) {
	if im != nil && parser != nil && store != nil {
		im.structureParser = parser
		im.structureStore = store
	}
}

// SetSceneGenEnabled toggles per-item scene generation on ingest (default off).
func (im *IngestionManager) SetSceneGenEnabled(v bool) {
	if im != nil {
		im.sceneGenEnabled = v
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
// An invalid chunkerCfg is a startup error, not a silent downgrade. NewRegistry
// already rejects a route naming an unregistered chunker; the error is logged and
// the manager falls back to option_c so ingestion still runs, because refusing to
// ingest anything is a worse answer to a typo in one route than ingesting with the
// documented default. The log line is the loud part.
func NewIngestionManager(sceneGen *SceneGenerator, embedder domain.Embedder, agent *Agent, cfg IngestionConfig, chunkerCfg config.ChunkerConfig) *IngestionManager {
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
	return NewIngestionManagerWithRegistry(sceneGen, embedder, agent, registry, cfg)
}

func NewIngestionManagerWithRegistry(sceneGen *SceneGenerator, embedder domain.Embedder, agent *Agent, registry *Registry, cfg IngestionConfig) *IngestionManager {
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
	if registry == nil {
		registry = fallbackRegistry()
	}
	return &IngestionManager{queue: make(chan domain.ExternalDocument, cfg.QueueSize), sceneGen: sceneGen, embedder: embedder, agent: agent, registry: registry, cfg: cfg}
}
func (im *IngestionManager) Enqueue(doc domain.ExternalDocument) bool {
	select {
	case im.queue <- doc:
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
// paths share the chunker registry + scene generator + agent
// write path.
func (im *IngestionManager) ProcessSync(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	// Evidence first (ADR-0105 D2): the original bytes must be durable and
	// published as evidence before chunking, scene generation or any other
	// semantic step — so a failure anywhere downstream still leaves source
	// material that can be reprocessed.
	if im.evidenceCapture != nil {
		if err := im.captureEvidence(ctx, doc); err != nil {
			return "", fmt.Errorf("ingestion manager: evidence capture: %w", err)
		}
	}
	scene := ""
	if im.sceneGenEnabled && im.sceneGen != nil {
		scenes, err := im.sceneGen.Generate(ctx, []domain.ExternalDocument{doc})
		if err != nil {
			return "", fmt.Errorf("ingestion manager: scene generate: %w", err)
		}
		if len(scenes) > 0 {
			scene = scenes[0]
		}
	}
	_, entityID := im.mintSourceDoc(ctx, doc)
	if entityID == "" {
		return "", fmt.Errorf("ingestion manager: failed to mint source-doc entity for %q", doc.SourceURI)
	}
	ext := docExt(doc.SourceURI)
	chunker, _ := im.registry.Resolve(doc.SourceType, ext)
	if chunker == nil {
		chunker = OptionCChunker{}
	}
	chunks, err := chunker.Chunk(ctx, &doc)
	if err != nil {
		slog.Warn("IngestionManager: chunker failed, falling back to OptionC", "source_uri", doc.SourceURI, "err", err)
		chunks, _ = OptionCChunker{}.Chunk(ctx, &doc)
	}
	chunkCount, err := im.persistChunks(ctx, doc, chunks, entityID, scene)
	if err != nil {
		return entityID, err
	}
	if chunkCount == 0 {
		return entityID, nil
	}
	slog.Info("IngestionManager: sync ingest complete", "source_uri", doc.SourceURI, "entity_id", entityID, "chunk_count", chunkCount)
	im.publishWritten(doc, entityID, chunkCount)
	return entityID, nil
}

// captureEvidence maps one ExternalDocument onto the evidence write path.
//
// SourceKey is the document's stable identity (the same externalDocumentID the
// rest of the pipeline uses), and SourceRevision is the content digest: a
// replayed identical delivery dedupes, while the SAME key arriving with CHANGED
// bytes becomes a NEW evidence revision rather than an update (ADR-0105 D4) —
// which is exactly the re-ingest-a-changed-file case.
func (im *IngestionManager) captureEvidence(ctx context.Context, doc domain.ExternalDocument) error {
	body := doc.Data
	if len(body) == 0 {
		body = []byte(doc.Body)
	}
	if len(body) == 0 {
		return fmt.Errorf("document carries no bytes")
	}
	sourceID := doc.SourceType
	if sourceID == "" {
		sourceID = "unknown"
	}
	digest := sha256.Sum256(body)
	_, _, err := im.evidenceCapture.Ingest(ctx, evidence.Raw{
		SourceID:       sourceID,
		SourceKey:      externalDocumentID(doc),
		SourceRevision: hex.EncodeToString(digest[:]),
		SourceTime:     doc.Timestamp,
		Bytes:          body,
		Classification: doc.Tags,
	})
	return err
}

func (im *IngestionManager) Start(ctx context.Context) {
	jobs := make(chan []domain.ExternalDocument, im.cfg.Workers*2)
	go im.batchLoop(ctx, jobs)
	for range im.cfg.Workers {
		go im.worker(ctx, jobs)
	}
}

func (im *IngestionManager) batchLoop(ctx context.Context, jobs chan<- []domain.ExternalDocument) {
	var batch []domain.ExternalDocument
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

func (im *IngestionManager) worker(ctx context.Context, jobs <-chan []domain.ExternalDocument) {
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

func (im *IngestionManager) processBatch(ctx context.Context, batch []domain.ExternalDocument) {
	var scenes []string
	if im.sceneGenEnabled && im.sceneGen != nil {
		scenes, _ = im.sceneGen.Generate(ctx, batch)
	}
	for i, doc := range batch {
		scene := ""
		if i < len(scenes) {
			scene = scenes[i]
		}
		_, entityID := im.mintSourceDoc(ctx, doc)
		ext := docExt(doc.SourceURI)
		chunker, _ := im.registry.Resolve(doc.SourceType, ext)
		if chunker == nil {
			chunker = OptionCChunker{}
		}
		chunks, err := chunker.Chunk(ctx, &doc)
		if err != nil {
			slog.Warn("IngestionManager: chunker failed, falling back to OptionC", "source_uri", doc.SourceURI, "err", err)
			chunks, _ = OptionCChunker{}.Chunk(ctx, &doc)
		}
		chunkCount, err := im.persistChunks(ctx, doc, chunks, entityID, scene)
		if err != nil {
			slog.Warn("IngestionManager: failed to persist chunks", "source_uri", doc.SourceURI, "err", err)
			continue
		}
		slog.Info("ExternalDocumentIngested", "source_uri", doc.SourceURI, "entity_id", entityID, "chunk_count", chunkCount)
	}
}

func (im *IngestionManager) mintSourceDoc(ctx context.Context, doc domain.ExternalDocument) (string, string) {
	if im.agent == nil || im.agent.Manager == nil || im.agent.Manager.Store == nil {
		return "", ""
	}
	var cid string
	if im.agent.ContentStore != nil && len(doc.Body) > 0 {
		if c, err := im.agent.ContentStore.Put(ctx, []byte(doc.Body), "source_document", nil, buildBodyPreview(doc.Body, 150)); err == nil {
			cid = string(c)
		}
	}
	entityID := "source_doc:" + doc.SourceURI
	meta := map[string]interface{}{"kind": "source_document", "canonical_id": doc.SourceURI, "source_uri": doc.SourceURI, "source_type": doc.SourceType, "title": doc.Title, "author": doc.Author, "timestamp": doc.Timestamp.Format(time.RFC3339), "document_id": externalDocumentID(doc)}
	if len(doc.Tags) > 0 {
		meta["tags"] = append([]string(nil), doc.Tags...)
	}
	if cid != "" {
		meta["content_cid"] = cid
	}
	if err := im.agent.Manager.Save(ctx, &domain.Document{
		ID: entityID, DocumentType: domain.DocTypeMnemonicEntity,
		Text: doc.Title, ActivationStrength: 0.1, Metadata: meta,
	}); err != nil {
		slog.Warn("IngestionManager: failed to mint source-doc entity", "err", err)
		return cid, ""
	}
	return cid, entityID
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
