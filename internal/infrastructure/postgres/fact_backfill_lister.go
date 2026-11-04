package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// FactBackfillExtractor is the extractor name the memory_fact extraction marker is
// written under (knowledge_extractions.extractor). memory.FactExtractor writes it; the
// lister below excludes documents that carry it.
const FactBackfillExtractor = domain.KindMemoryFact

var _ domain.FactBackfillLister = (*PgKnowledgeStore)(nil)

// ListFactBackfillCandidates implements domain.FactBackfillLister over the documents
// and chunks tables (ADR-0136 A1.7; T-20261103-no-backfill-path-populates-memory-facts-for).
// It returns identifiers, tags, times and source metadata (title, time label, total text
// length, and the client's descriptive source kind / author label from documents.metadata,
// ADR-0136 A3.2) only — never content: the chunk TEXT, and the chunks' A3.3 speaker
// metadata with it, is read by the backfill through the enforcing read store.
//
// A document is a candidate when it has stored content chunks and (unless
// IncludeMarked) no memory_fact marker in knowledge_extractions. Chunk ids come in
// ingest order — the numeric "-chunk-N" suffix the ingest path mints
// (externalChunkID) — because that order is what the extractor's content hash digests
// and its turn indexes count. The session time is the document's event time, else its
// earliest chunk event time, else
// the document's creation time (the same "stated event time, else the ingest clock"
// the ingest path hands the extractor). The writing principal is the chunks'
// source_agent_id (the ingest path's doc.Author).
func (s *PgKnowledgeStore) ListFactBackfillCandidates(ctx context.Context, f domain.FactBackfillFilter) ([]domain.FactBackfillCandidate, error) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.SourceType != "" {
		conds = append(conds, "d.source_type = "+arg(f.SourceType))
	}
	if tags := nonEmpty(f.Tags); len(tags) > 0 {
		conds = append(conds, "d.tags @> "+arg(tags)+"::text[]")
	}
	conds = append(conds, documentWriterConds(f.Principal, f.Namespace, arg)...)
	if !f.IncludeMarked {
		// ADR-0136 A2.4: with the running extractor's version, only a COMPLETE marker of
		// that version excludes — another version, a legacy row ('') or a partial run
		// (covered < total) keeps the document a candidate. The per-content decision
		// (hash, fact budget) is the extractor's (AlreadyExtracted).
		done := ""
		if f.ExtractorVersion != "" {
			done = ` AND k.extractor_version = ` + arg(f.ExtractorVersion) +
				` AND COALESCE(k.passages_covered, 0) >= COALESCE(k.passages_total, 0)`
		}
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM knowledge_extractions k
			WHERE k.extractor = `+arg(FactBackfillExtractor)+` AND k.document_id = d.id`+done+`)`)
	}
	where := ""
	if len(conds) > 0 {
		where = " AND " + strings.Join(conds, " AND ")
	}
	// The time label follows the session time it qualifies: the document's own, else the
	// chunks' (when only they carry an event time), else none — the creation clock, which
	// reads as ingest_default (A2.1: basis "ingested").
	q := `SELECT d.id, d.source_type, d.tags,
			COALESCE(min(d.event_time), min(c.event_time), min(d.created_at AT TIME ZONE 'UTC')) AS session_time,
			array_agg(c.id ORDER BY NULLIF(substring(c.id from '-chunk-([0-9]+)$'), '')::int NULLS LAST, c.id) AS chunk_ids,
			COALESCE(max(c.metadata->>'source_agent_id'), '') AS author,
			d.title,
			COALESCE(CASE WHEN min(d.event_time) IS NOT NULL THEN d.time_source END,
				CASE WHEN min(c.event_time) IS NOT NULL THEN min(c.time_source) END, '') AS time_source,
			COALESCE(CASE WHEN min(d.event_time) IS NOT NULL THEN d.time_precision END,
				CASE WHEN min(c.event_time) IS NOT NULL THEN min(c.time_precision) END, '') AS time_precision,
			COALESCE(sum(length(c.text)), 0)::int AS text_chars,
			COALESCE(max(d.metadata->>'` + domain.DocMetaSourceKind + `'), '') AS source_kind,
			COALESCE(max(d.metadata->>'` + domain.DocMetaAuthorLabel + `'), '') AS author_label
		FROM documents d
		JOIN chunks c ON c.document_id = d.id
		WHERE c.text <> ''` + where + `
		GROUP BY d.id, d.source_type, d.tags, d.title, d.time_source, d.time_precision
		ORDER BY session_time, d.id`
	if f.Limit > 0 {
		q += " LIMIT " + arg(f.Limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fact backfill candidates: %w", err)
	}
	defer rows.Close()
	var out []domain.FactBackfillCandidate
	for rows.Next() {
		var c domain.FactBackfillCandidate
		var at time.Time
		var ts, tp string
		if err := rows.Scan(&c.DocumentID, &c.SourceType, &c.Tags, &at, &c.ChunkIDs, &c.Author,
			&c.Title, &ts, &tp, &c.TextChars, &c.SourceKind, &c.AuthorLabel); err != nil {
			return nil, err
		}
		c.TimeSource, c.TimePrecision = domain.TimeSource(ts), domain.TimePrecision(tp)
		c.SessionTime = at.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// documentWriterConds selects documents (alias d, joined to their chunks c) written by
// one principal or by a principal-id prefix: the document's provenance tag
// (ProvenanceSourceTagPrefix+id) or a chunk's source_agent_id. Shared by the backfill
// lister and the fact reset's marker selection, so both select the same documents.
func documentWriterConds(principal, namespace string, arg func(any) string) []string {
	var conds []string
	if principal != "" {
		p := arg(principal)
		conds = append(conds, fmt.Sprintf("(d.tags @> ARRAY['%s' || %s] OR c.metadata->>'source_agent_id' = %s)",
			domain.ProvenanceSourceTagPrefix, p, p))
	}
	if namespace != "" {
		// A principal-id PREFIX: LIKE with the prefix's own wildcards escaped.
		p := arg(likePrefix(namespace))
		conds = append(conds, fmt.Sprintf(`(c.metadata->>'source_agent_id' LIKE %s ESCAPE '\' OR EXISTS (
			SELECT 1 FROM unnest(d.tags) t WHERE t LIKE '%s' || %s ESCAPE '\'))`, p, domain.ProvenanceSourceTagPrefix, p))
	}
	return conds
}

// likePrefix is a LIKE pattern (ESCAPE '\') matching every string that starts with
// prefix, the prefix's own wildcards escaped.
func likePrefix(prefix string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
