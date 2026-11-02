package kernel

import (
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// The structure junk filter's backend list must reach the IngestionManager
// (T-20261102-ingestion-structure-junk-filter-silently-drops; AGENTS.md §3.4).
func TestIngestionConfigFrom_ThreadsJunkFilterBackends(t *testing.T) {
	got := ingestionConfigFrom(config.IngestionConfig{
		IngestionQueueSize:          7,
		IngestionBatchSize:          3,
		IngestionWorkers:            2,
		IngestionBatchWaitMs:        250,
		ChunkContextPrepend:         true,
		StructureJunkFilterBackends: []string{"docling", "text"},
	})
	if !slices.Equal(got.JunkFilterBackends, []string{"docling", "text"}) {
		t.Fatalf("JunkFilterBackends = %v, want [docling text]", got.JunkFilterBackends)
	}
	if got.QueueSize != 7 || got.BatchSize != 3 || got.Workers != 2 ||
		got.BatchWait != 250*time.Millisecond || !got.ContextPrepend {
		t.Fatalf("ingestion settings not threaded: %+v", got)
	}
}

// The shipped default filters Docling output only, so chat and other text
// ingests (markdown/text backends) keep every leaf.
func TestIngestionConfigFrom_DefaultFiltersDoclingOnly(t *testing.T) {
	got := ingestionConfigFrom(config.DefaultConfig().Execution.Ingestion)
	if !slices.Equal(got.JunkFilterBackends, []string{"docling"}) {
		t.Fatalf("default JunkFilterBackends = %v, want [docling]", got.JunkFilterBackends)
	}
}

// sync_ingest_wait_for_kg and its timeout reach the IngestionManager
// (T-20261102-async-kg-triplet-extraction-30-chunks-s-lags; AGENTS.md §3.4).
func TestIngestionConfigFrom_ThreadsSyncWaitForKG(t *testing.T) {
	got := ingestionConfigFrom(config.IngestionConfig{SyncIngestWaitForKG: true, SyncIngestKGWaitTimeoutMs: 1500})
	if !got.SyncWaitForKG || got.SyncKGWaitTimeout != 1500*time.Millisecond {
		t.Fatalf("sync KG wait not threaded: %+v", got)
	}
	def := ingestionConfigFrom(config.DefaultConfig().Execution.Ingestion)
	if def.SyncWaitForKG || def.SyncKGWaitTimeout != 30*time.Second {
		t.Fatalf("default: SyncWaitForKG=%v timeout=%v, want false and 30s", def.SyncWaitForKG, def.SyncKGWaitTimeout)
	}
}

// kg_triplets_max_requeues and the batcher's existing knobs reach the
// ChunkTripletsBatcher (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
func TestChunkTripletsBatcherConfigFrom_Threads(t *testing.T) {
	got := chunkTripletsBatcherConfigFrom(config.IngestionConfig{
		EdgeExtractionQueueSize: 11, EdgeExtractionBatchSize: 5, EdgeExtractionMaxIdleMs: 300,
		EdgeExtractionLLMTimeoutMs: 4000, KgTripletsMaxRequeues: 2,
	})
	if got.QueueSize != 11 || got.BatchSize != 5 || got.MaxIdle != 300*time.Millisecond ||
		got.LLMTimeout != 4*time.Second || got.MaxRequeues != 2 {
		t.Fatalf("batcher settings not threaded: %+v", got)
	}
	if def := chunkTripletsBatcherConfigFrom(config.DefaultConfig().Execution.Ingestion); def.MaxRequeues != 1 {
		t.Fatalf("default MaxRequeues = %d, want 1", def.MaxRequeues)
	}
}
