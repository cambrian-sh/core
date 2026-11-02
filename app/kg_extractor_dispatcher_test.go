package app

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// The kg_extractor retry settings reach the dispatcher (AGENTS.md §3.4;
// T-20261102-kg-extractor-cold-start-drops-the-first-kg).
func TestKgExtractorDispatcherFrom_ThreadsRetry(t *testing.T) {
	d := kgExtractorDispatcherFrom(config.IngestionConfig{KgExtractorCallAttempts: 4, KgExtractorRetryBackoffMs: 1500}, nil)
	if d.Attempts != 4 || d.Backoff != 1500*time.Millisecond || d.AgentID != "kg_extractor_agent" {
		t.Fatalf("dispatcher = %+v", d)
	}
	def := kgExtractorDispatcherFrom(config.DefaultConfig().Execution.Ingestion, nil)
	if def.Attempts != 3 || def.Backoff != 2*time.Second {
		t.Fatalf("default dispatcher = attempts %d backoff %v, want 3 and 2s", def.Attempts, def.Backoff)
	}
}
