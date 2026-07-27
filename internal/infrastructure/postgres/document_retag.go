package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// RetagDocument changes a document's classification and rewrites the derived copy on
// every one of its chunks — in ONE transaction (ADR-0093).
//
// This method is the reason the split was worth doing.
//
// Before it, a document's tags existed only as N independent copies in chunk metadata
// with no authoritative row. Re-classifying meant N updates with no transaction
// boundary, so a partial failure left the document HALF-CLASSIFIED: some chunks
// carrying the new tags and reachable, some carrying the old ones and not. Nothing
// reported it, because nothing knew what the document's tags were supposed to be.
//
// A boundary that can be half-applied is not a boundary. Now the document row is the
// single source of truth, the chunk copies are an explicitly derived cache, and both
// move together or neither does.
//
// The cache is kept rather than dropped in favour of a join because the tag filter sits
// on the retrieval hot path behind a GIN index (`metadata @> '{"tags":[...]}'`).
// Replacing that with a join to `documents` on every search would trade a correctness
// win for a latency regression on the most frequent query in the system.
func (p *PgVectorAdapter) RetagDocument(ctx context.Context, documentID string, tags []string) error {
	if documentID == "" {
		return nil
	}
	if tags == nil {
		tags = []string{}
	}
	// The chunk cache is JSON, so the tag list is marshalled once and spliced into
	// each chunk's metadata by key — leaving every other metadata key untouched.
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return mapError("RetagDocument", err)
	}

	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, terr := tx.Exec(ctx,
			`UPDATE `+TableDocuments+` SET tags = $2, updated_at = NOW() WHERE id = $1`,
			documentID, tags); terr != nil {
			return terr
		}
		// jsonb_set with create_missing=true adds the key when a chunk never had one,
		// so a document ingested before it was classified is fully covered too.
		_, terr := tx.Exec(ctx,
			`UPDATE `+TableChunks+`
			 SET metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{tags}', $2::jsonb, true)
			 WHERE document_id = $1`,
			documentID, string(tagsJSON))
		return terr
	})
	return mapError("RetagDocument", err)
}

// DocumentTags returns the authoritative classification for a document.
//
// Reads the document row rather than sampling a chunk: sampling is what the old shape
// forced, and it cannot tell a correct answer from one chunk of a half-classified
// document.
func (p *PgVectorAdapter) DocumentTags(ctx context.Context, documentID string) ([]string, error) {
	var tags []string
	err := p.pool.QueryRow(ctx,
		`SELECT tags FROM `+TableDocuments+` WHERE id = $1`, documentID).Scan(&tags)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, mapError("DocumentTags", err)
	}
	return tags, nil
}
