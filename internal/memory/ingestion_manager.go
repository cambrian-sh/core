package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"

	"github.com/google/uuid"
)

// IngestionConfig holds tuning parameters for the IngestionManager pipeline.
type IngestionConfig struct {
	QueueSize int
	BatchSize int
	Workers   int
	BatchWait time.Duration
}

// IngestionManager orchestrates the external document ingestion pipeline:
//
//	Adapter → IngestionQueue → Batcher → Worker Pool → SceneGenerator → Chunker → Agent.EnqueueExternal
//
// (ADR-0028)
type IngestionManager struct {
	queue    chan domain.ExternalDocument
	sceneGen *SceneGenerator
	embedder domain.Embedder
	agent    *Agent
	cfg      IngestionConfig
}

// NewIngestionManager constructs an IngestionManager. Call Start(ctx) to begin processing.
func NewIngestionManager(sceneGen *SceneGenerator, embedder domain.Embedder, agent *Agent, cfg IngestionConfig) *IngestionManager {
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
	return &IngestionManager{
		queue:    make(chan domain.ExternalDocument, cfg.QueueSize),
		sceneGen: sceneGen,
		embedder: embedder,
		agent:    agent,
		cfg:      cfg,
	}
}

// Enqueue submits a document for ingestion. Returns true if accepted, false if the queue is full.
func (im *IngestionManager) Enqueue(doc domain.ExternalDocument) bool {
	select {
	case im.queue <- doc:
		return true
	default:
		slog.Warn("IngestionManager: queue full, dropping document", "source_uri", doc.SourceURI)
		return false
	}
}

// Start launches the batcher and worker pool goroutines. Stops when ctx is cancelled.
func (im *IngestionManager) Start(ctx context.Context) {
	jobs := make(chan []domain.ExternalDocument, im.cfg.Workers*2)

	// Batcher: collects up to BatchSize docs or flushes after BatchWait.
	go im.batchLoop(ctx, jobs)

	// Worker pool.
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
	scenes, _ := im.sceneGen.Generate(ctx, batch)

	var items []pendingItem
	for i, doc := range batch {
		scene := ""
		if i < len(scenes) {
			scene = scenes[i]
		}

		chunks := ChunkDocument(doc)
		for _, ch := range chunks {
			ch.Metadata["snapshot"] = scene

			text := ch.Body
			vec, err := im.embedder.Embed(ctx, text)
			if err != nil {
				slog.Warn("IngestionManager: embed failed, skipping chunk",
					"source_uri", doc.SourceURI, "err", err)
				continue
			}

			domainDoc := &domain.Document{
				ID:                 fmt.Sprintf("ext-%s", uuid.New().String()),
				DocumentType:       domain.DocTypeMnemonicFact,
				Text:               text,
				ActivationStrength: 0.1,
				Metadata:           ch.Metadata,
			}
			items = append(items, pendingItem{Embedding: vec, Doc: domainDoc, SceneID: scene})
		}
	}

	if len(items) == 0 {
		return
	}

	_ = im.agent.EnqueueExternal(ctx, items)

	// Emit audit event via slog (EventSink wiring deferred to Slice 4).
	for _, doc := range batch {
		slog.Info("ExternalDocumentIngested",
			"source_uri", doc.SourceURI,
			"chunk_count", len(items))
	}
}
