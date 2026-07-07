package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
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
	// sceneGenEnabled gates the per-item scene-generation LLM call on the ingest
	// hot path (ADR-0049 episodic scenes). Default OFF: it stalls ingest when no
	// LLM is reachable and is not needed for document/structure retrieval.
	sceneGenEnabled bool
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

func defaultRegistry() *Registry {
	reg, _ := NewRegistry(map[string]domain.Chunker{"option_c": OptionCChunker{}}, ChunkerConfig{Default: "option_c"})
	return reg
}

func NewIngestionManager(sceneGen *SceneGenerator, embedder domain.Embedder, agent *Agent, cfg IngestionConfig) *IngestionManager {
	return NewIngestionManagerWithRegistry(sceneGen, embedder, agent, defaultRegistry(), cfg)
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
		registry = defaultRegistry()
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
	return entityID, nil
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
	if err := im.agent.Manager.Store.Save(ctx, &domain.Document{
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
