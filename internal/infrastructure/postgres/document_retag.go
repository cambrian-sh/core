package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cambrian-sh/core/domain"
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
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The owner fence (domain/owner_scope.go) is not a label a retag may move:
		// dropping owner:X would publish X's personal material to everyone, and
		// adding one would hide a row behind someone else's identity. So owner tags
		// are carried over from the row as stored (locked, so a concurrent retag
		// cannot interleave) and any owner tag in the request is ignored.
		var current []string
		if terr := tx.QueryRow(ctx,
			`SELECT tags FROM `+TableDocuments+` WHERE id = $1 FOR UPDATE`, documentID).Scan(&current); terr != nil && !errors.Is(terr, pgx.ErrNoRows) {
			return terr
		}
		final := retagPreservingOwner(current, tags)
		// The chunk cache is JSON, so the tag list is marshalled once and spliced into
		// each chunk's metadata by key — leaving every other metadata key untouched.
		tagsJSON, merr := json.Marshal(final)
		if merr != nil {
			return merr
		}
		tags := final
		if _, terr := tx.Exec(ctx,
			`UPDATE `+TableDocuments+` SET tags = $2, updated_at = (now() AT TIME ZONE 'UTC') WHERE id = $1`, // UTC wall clock (0019_event_time.sql)
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

// retagPreservingOwner is the requested classification minus any owner-namespace
// tag, plus the owner tags the document already carries. Never nil.
func retagPreservingOwner(current, requested []string) []string {
	out := []string{}
	for _, t := range requested {
		if !domain.IsOwnerTag(t) {
			out = append(out, t)
		}
	}
	for _, t := range current {
		if domain.IsOwnerTag(t) {
			out = append(out, t)
		}
	}
	return out
}

// DocumentTags returns the authoritative classification for a document.
//
// Reads the document row rather than sampling a chunk: sampling is what the old shape
// forced, and it cannot tell a correct answer from one chunk of a half-classified
// document.
// It reports found separately from an empty result, because "this document has no
// labels" and "there is no such document" demand different handling and an untagged
// document is the common case rather than an error.
func (p *PgVectorAdapter) DocumentTags(ctx context.Context, documentID string) (tags []string, found bool, err error) {
	err = p.pool.QueryRow(ctx,
		`SELECT tags FROM `+TableDocuments+` WHERE id = $1`, documentID).Scan(&tags)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, mapError("DocumentTags", err)
	}
	return tags, true, nil
}

// ParentDocumentOf resolves the document a chunk belongs to, if any.
//
// Retrieval returns CHUNKS, so an operator labelling something they found in search is
// holding a chunk id. Tagging that row directly would label one chunk of a document and
// leave the rest — the half-classified state ADR-0093 exists to prevent, produced by the
// very console feature meant to fix classification.
//
// A memory an agent wrote has no parent and correctly reports none: it is its own
// authority, and tagging it in place is right.
func (p *PgVectorAdapter) ParentDocumentOf(ctx context.Context, chunkID string) (string, bool, error) {
	var parent *string
	err := p.pool.QueryRow(ctx,
		`SELECT document_id FROM `+TableChunks+` WHERE id = $1`, chunkID).Scan(&parent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, mapError("ParentDocumentOf", err)
	}
	if parent == nil || *parent == "" {
		return "", false, nil
	}
	return *parent, true, nil
}
