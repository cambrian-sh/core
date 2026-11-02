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
