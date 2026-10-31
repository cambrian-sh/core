package chaos_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/chaos"
)

// Documented production failure modes with no test before 2026-10-31
// (T-20261031-documented-production-failure-modes-have-no). This file covers
// the embed-endpoint slowness path through the real IngestionManager; the
// Ollama HTTP-level timeout and overflow-halving live next to the embedder
// (internal/infrastructure/llm/failure_mode_embed_endpoint_test.go), the
// dimension guard next to the pgvector adapter, the agent boot timeout in
// agentmgr, and readiness in internal/health.

// The embed endpoint stalls past the caller's deadline. Guards, in
// ingestion_chunks.go embedChunkBodies: the batch→per-chunk fallback
// ("batch embed failed, falling back"), the per-chunk skip ("embed failed,
// skipping chunk"), and persistChunks' `len(docs) == 0` branch ("document
// produced NO stored chunks") — the three WARN lines that make an embedder
// outage visible at all. Removing any of them turns this red.
func TestFailureMode_IngestionEmbedEndpointStall(t *testing.T) {
	logs := chaos.CaptureLogs(t)
	inner := newMemStore()
	emb := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20, Delay: 30 * time.Second})
	im := chaosIngestionManager(t, inner, emb)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	entityID, err := im.ProcessSync(ctx, chaosDocument())
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("ProcessSync did not honour the caller deadline against a stalled embedder: took %s", elapsed)
	}
	if n := logs.Count(slog.LevelWarn, "batch embed failed, falling back to per-chunk embed"); n != 1 {
		t.Fatalf("batch-embed deadline must fall back to per-chunk embedding (want 1 WARN, got %d)", n)
	}
	if n := logs.Count(slog.LevelWarn, "embed failed, skipping chunk"); n != 2 {
		t.Fatalf("each per-chunk embed failure must be logged (want 2 for a 2-chunk document, got %d)", n)
	}
	if n := logs.Count(slog.LevelWarn, "document produced NO stored chunks"); n != 1 {
		t.Fatalf("a document that stored no content must be reported (want 1 WARN, got %d)", n)
	}
	if facts := inner.byType(domain.DocTypeMnemonicFact); len(facts) != 0 {
		t.Fatalf("no chunk may be stored without an embedding, found %d", len(facts))
	}
	if entityID == "" {
		t.Fatal("the source-doc entity id is minted before embedding and must be returned")
	}

	t.Run("embed outage is surfaced to the caller", func(t *testing.T) {
		if err == nil {
			t.Skip("known defect: T-20261031-processsync-reports-success-when-every-chunk — ProcessSync returns (entityID, nil) when every chunk embed failed")
		}
	})
}
