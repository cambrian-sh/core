package memory

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Ingest stage names, recorded on the document's domain.StageClock and logged as
// "<stage>_ms" by logIngestTiming. One line per ingested document, like the
// retrieval "memory pass timing" line, so the cost of an ingest can be attributed
// without a profiler (T-20261102-ingestion-logs-no-per-stage-timing-so-the-1-s).
// Stages are recorded in the order they run; a stage that did not run is absent.
const (
	StageEvidence         = "evidence"              // ADR-0105 blob put + verify + evidence row + outbox
	StageScene            = "scene"                 // ADR-0049 scene LLM call (default off)
	StageChunk            = "chunk"                 // flat chunker (its output is replaced when structure parsing yields leaves)
	StageDocumentRow      = "document_row"          // ADR-0093 source-document row, via the write chokepoint
	StageStructureParse   = "structure_parse"       // ADR-0060 docling_agent round-trip, as the kernel sees it
	StageStructureAgent   = "structure_agent"       // the sidecar's own parse time (reported by it); subset of structure_parse
	StageStructureLeaves  = "structure_leaves"      // junk filter + leaves-as-chunks
	StagePrepare          = "prepare"               // ADR-0134 chunk stamps + context prepend + ids
	StageEmbed            = "embed"                 // EmbedBatch (or the per-chunk fallback)
	StageChunkSave        = "chunk_save"            // SaveBatch: classify + one transaction of chunk + projection upserts
	StageChunkSaveBegin   = "chunk_save_begin"      // pool acquire + BEGIN inside chunk_save (reported by the adapter)
	StageChunkSaveUpsert  = "chunk_save_upsert"     // chunk row upserts inside chunk_save (reported by the adapter)
	StageChunkSaveProject = "chunk_save_projection" // ADR-0107 projection upserts inside chunk_save (reported by the adapter)
	StageChunkSaveCommit  = "chunk_save_commit"     // COMMIT inside chunk_save (reported by the adapter)
	StageTripletsEnqueue  = "triplets_enqueue"      // ADR-0053 non-blocking enqueue; extraction is async
	StageKGWait           = "kg_wait"               // sync_ingest_wait_for_kg: wait for this document's triplets (sync path only)
	StageStructurePersist = "structure_persist"     // ADR-0060 sections + PART_OF/NEXT edges + chunk stamps
	StageEntity           = "entity"                // source-doc entity: content-store put + upsert
	StagePublish          = "publish"               // operator feed event

	// Counts.
	CountEmbedCalls    = "embed_calls"    // EmbedBatch/Embed calls made by the ingest
	CountEmbedRequests = "embed_requests" // HTTP requests the embedder actually sent (reported by it)
	CountChunks        = "chunks"         // chunks embedded
	CountKGPending     = "kg_pending"     // chunks whose triplets were not yet extracted when the kg_wait ended
	CountKGFailed      = "kg_failed"      // chunks whose triplets will not be persisted (queue full, save error, batcher stopped)
)

// slowIngestThreshold promotes an ingest's timing line from DEBUG to INFO, the
// memory-pass pattern (slowPassThreshold). A ~14-chunk chat session's embedding
// alone is ~0.3 s on bge-large; a whole ingest over a second is worth a line.
const slowIngestThreshold = time.Second

// ingestClock returns the clock already on ctx (the gRPC handler installs one to
// report the breakdown to its caller) or installs a fresh one.
func ingestClock(ctx context.Context) (context.Context, *domain.StageClock) {
	if c := domain.StageClockFrom(ctx); c != nil {
		return ctx, c
	}
	c := domain.NewStageClock()
	return domain.WithStageClock(ctx, c), c
}

// logIngestTiming writes the one per-document timing line.
func logIngestTiming(ctx context.Context, c *domain.StageClock, total time.Duration, sourceURI string, stored int, err error) {
	attrs := []slog.Attr{
		slog.Int64("total_ms", total.Milliseconds()),
		slog.String("source_uri", clip(sourceURI, 120)),
		slog.Int("stored_chunks", stored),
	}
	var parse, agent time.Duration
	for _, s := range c.Stages() {
		attrs = append(attrs, slog.Int64(s.Stage+"_ms", s.Duration.Milliseconds()))
		switch s.Stage {
		case StageStructureParse:
			parse = s.Duration
		case StageStructureAgent:
			agent = s.Duration
		}
	}
	if agent > 0 {
		// Queue wait (the sidecar serves one request at a time) + gRPC + JSON.
		attrs = append(attrs, slog.Int64("structure_wait_ms", (parse-agent).Milliseconds()))
	}
	counts := c.Counts()
	for _, k := range sortedKeys(counts) {
		attrs = append(attrs, slog.Int(k, counts[k]))
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	level, msg := slog.LevelDebug, "ingest timing"
	if total > slowIngestThreshold || err != nil {
		level, msg = slog.LevelInfo, "ingest timing (SLOW)"
		if err != nil {
			msg = "ingest timing (FAILED)"
		}
	}
	slog.LogAttrs(ctx, level, msg, attrs...)
}

// FormatStageClock renders a clock as "stage=ms,...;count=n,..." for transport in
// gRPC metadata (the sync ingest RPC returns it as a trailer, no proto change).
func FormatStageClock(c *domain.StageClock, total time.Duration) string {
	var b strings.Builder
	b.WriteString("total=")
	b.WriteString(strconv.FormatFloat(float64(total.Microseconds())/1000, 'f', 1, 64))
	for _, s := range c.Stages() {
		b.WriteByte(',')
		b.WriteString(s.Stage)
		b.WriteByte('=')
		b.WriteString(strconv.FormatFloat(float64(s.Duration.Microseconds())/1000, 'f', 1, 64))
	}
	counts := c.Counts()
	for _, k := range sortedKeys(counts) {
		b.WriteByte(',')
		b.WriteString(k)
		b.WriteString("#=")
		b.WriteString(strconv.Itoa(counts[k]))
	}
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
