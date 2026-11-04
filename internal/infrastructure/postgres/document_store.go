package postgres

import (
	"context"
	"encoding/json"

	"github.com/cambrian-sh/core/domain"
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
func (p *PgVectorAdapter) SaveDocument(ctx context.Context, doc domain.SourceDocument) ([]string, error) {
	if doc.ID == "" {
		return nil, nil
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
	//
	// ADR-0134: the event-time stamp follows the same keep-unless-restated rule as the
	// chunk upsert (keepStatedTime, owner decision Q6): an ingest default never
	// overwrites a stored real time; a restated time wins. updated_at is the record
	// clock of the latest ingest ("ingested_at always updates"); created_at keeps the
	// first. A writer that states no stamp at all leaves the stored one alone.
	//
	// ADR-0138 D3: written_by is set once and never replaced (a different writer was
	// already refused by DocumentLifecycleStore.ReviseDocument before this write);
	// content_digest follows the latest stored version. Empty values write NULL and keep
	// what is stored.
	st := eventStampValues(doc.EventStamp)
	set := `
			title       = EXCLUDED.title,
			source_type = EXCLUDED.source_type,
			text        = EXCLUDED.text,
			tags        = EXCLUDED.tags,
			metadata    = EXCLUDED.metadata,
			updated_at  = (now() AT TIME ZONE 'UTC'),
			written_by  = COALESCE(` + TableDocuments + `.written_by, EXCLUDED.written_by),
			content_digest = COALESCE(EXCLUDED.content_digest, ` + TableDocuments + `.content_digest)`
	if !doc.EventStamp.IsZero() {
		for _, col := range []string{"event_time", "event_time_end", "time_source", "time_precision", "claimed_time"} {
			set += ",\n\t\t\t" + col + " = " + keepStatedTime(TableDocuments, col)
		}
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO `+TableDocuments+` (id, title, source_type, text, tags, metadata,
			event_time, event_time_end, time_source, time_precision, claimed_time,
			written_by, content_digest)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, NULLIF($12, ''), NULLIF($13, ''))
		ON CONFLICT (id) DO UPDATE SET`+set,
		doc.ID, doc.Title, doc.SourceType, doc.Text, tags, string(meta),
		st["event_time"], st["event_time_end"], st["time_source"], st["time_precision"], st["claimed_time"],
		doc.WrittenBy, doc.ContentDigest)
	if err != nil {
		return nil, mapError("SaveDocument", err)
	}
	return tags, nil
}

// compile-time proof the adapter satisfies the ingest port.
var _ domain.DocumentStore = (*PgVectorAdapter)(nil)
