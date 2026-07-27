package postgres

import (
	"context"
	"encoding/json"

	"github.com/cambrian-sh/core/internal/memory"
)

// SaveDocument upserts the source-document entity (ADR-0093).
//
// `documents` is the one table here with no embedding and no vector index: a full
// document is not a retrieval unit, chunks are. Giving it a vector would have put it
// straight back into the recall index this split exists to clean.
//
// Tags are written as a real column rather than a metadata key because they are now
// load-bearing for access control. A classification you cannot constrain, index, or
// see in the schema is one that drifts — which is precisely what happened when the
// only copies lived in per-chunk JSON.
func (p *PgVectorAdapter) SaveDocument(ctx context.Context, doc memory.SourceDocument) error {
	if doc.ID == "" {
		return nil
	}
	meta := []byte("{}")
	if len(doc.Metadata) > 0 {
		if b, err := json.Marshal(doc.Metadata); err == nil {
			meta = b
		}
	}
	tags := doc.Tags
	if tags == nil {
		tags = []string{}
	}
	// Re-ingesting a document must not silently drop its classification, so tags are
	// updated with the incoming set. The caller owns them; this is the only writer.
	_, err := p.pool.Exec(ctx, `
		INSERT INTO `+TableDocuments+` (id, title, source_type, text, tags, metadata)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			title       = EXCLUDED.title,
			source_type = EXCLUDED.source_type,
			text        = EXCLUDED.text,
			tags        = EXCLUDED.tags,
			metadata    = EXCLUDED.metadata,
			updated_at  = NOW()`,
		doc.ID, doc.Title, doc.SourceType, doc.Text, tags, string(meta))
	return mapError("SaveDocument", err)
}

// compile-time proof the adapter satisfies the ingest port.
var _ memory.DocumentStore = (*PgVectorAdapter)(nil)
