// chunk-fill is a one-off CLI that back-fills the chunk_triplets table for
// analyst_agent (and optionally other agent_ids). It mirrors the live LLM
// routing used by the kernel (purposeGenerator + streaming) and the same
// batched LLM prompt the ChunkTripletsBatcher uses in production. The
// batching, streaming, and queue-fall-back semantics are identical.
//
// Usage:
//   chunk-fill --agent-id analyst_agent --batch-size 16 --max-idle-ms 2000
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/postgres"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
)

func main() {
	var (
		agentID   = flag.String("agent-id", "analyst_agent", "agent_id metadata value to scope the fill to")
		batchSize = flag.Int("batch-size", 16, "chunks per LLM call")
		maxIdle   = flag.Int("max-idle-ms", 2000, "idle-time drain trigger")
		queueSize = flag.Int("queue-size", 4096, "bounded channel size")
		llmTO     = flag.Int("llm-timeout-ms", 180000, "per-batch LLM call timeout (default 3min; deepseek-v4-flash batches of 16 can take 60-180s)")
		docType   = flag.String("doc-type", "mnemonic_fact", "documents.document_type to scan")
		limit     = flag.Int("limit", 0, "max chunks to fill (0 = all)")
	)
	flag.Parse()

	// Load .env so api_key_env (e.g. OPENCODE_API_KEY) resolves the same way
	// it does in the orchestrator. Missing file is a no-op.
	if err := config.LoadDotEnv(".env"); err != nil {
		slog.Error("load .env failed", "err", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgPath := os.Getenv("CAMBRIAN_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.json"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		slog.Error("pgvector connect failed", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	llmProvider, err := llm.NewProvider(cfg.LLMProvider, logger)
	if err != nil {
		slog.Error("LLM provider build failed", "err", err)
		os.Exit(1)
	}
	gen := llmProvider.GeneratorFor(domain.PurposeMemory)

	store := pg.ChunkTripletsStore()

	b := memory.NewChunkTripletsBatcher(gen, store, memory.ChunkTripletsBatcherConfig{
		QueueSize:  *queueSize,
		BatchSize:  *batchSize,
		MaxIdle:    time.Duration(*maxIdle) * time.Millisecond,
		LLMTimeout: time.Duration(*llmTO) * time.Millisecond,
	})
	b.Start(ctx)
	defer b.Stop()

	scanned, err := scanChunks(ctx, pg, *agentID, *docType, *limit)
	if err != nil {
		slog.Error("scan failed", "err", err)
		os.Exit(1)
	}
	slog.Info("chunk-fill: scanned", "agent_id", *agentID, "doc_type", *docType, "candidates", len(scanned))

	enqueued := 0
	for _, d := range scanned {
		b.Enqueue(&d)
		enqueued++
	}

	// Wait for the batcher to drain. Poll Stats every 200ms; stop when
	// enqueued == drained and the channel is empty.
	deadline := time.Now().Add(30 * time.Minute)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		e, dr, _, llm, tr := b.Stats()
		if e == dr && time.Since(lastLog) > 500*time.Millisecond {
			slog.Info("chunk-fill: drain status",
				"enqueued", e, "drained", dr, "llm_calls", llm, "triplets_persisted", tr)
			lastLog = time.Now()
		}
		if e >= uint64(enqueued) && dr >= uint64(enqueued) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	e, dr, _, llm, tr := b.Stats()
	slog.Info("chunk-fill: done",
		"enqueued", e, "drained", dr, "llm_calls", llm, "triplets_persisted", tr)
}

// scanChunks returns the documents of `docType` for `agentID` (looked up via
// documents.metadata->>'source_agent_id') that don't already have triplets
// in the chunk_triplets table. limit <= 0 means "all".
func scanChunks(ctx context.Context, pg *postgres.PgVectorAdapter, agentID, docType string, limit int) ([]domain.Document, error) {
	q := `SELECT id, text FROM documents
		WHERE document_type = $1
		  AND metadata->>'source_agent_id' = $2
		  AND NOT EXISTS (SELECT 1 FROM chunk_triplets ct WHERE ct.chunk_id = documents.id)
		ORDER BY created_at ASC`
	args := []any{docType, agentID}
	if limit > 0 {
		q += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := pg.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Document
	for rows.Next() {
		var (
			id, text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, domain.Document{ID: id, Text: text})
	}
	return out, rows.Err()
}
