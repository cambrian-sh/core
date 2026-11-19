package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

const (
	defaultDocumentPage = 50
	maxDocumentPage     = 500
)

// ListDocuments enumerates documents by ROW, for classification rather than recall.
//
// This reads `documents` directly instead of going anywhere near the retrieval path.
// A document is not a retrieval unit — it has no embedding and no search ever returns
// one — so the question "which documents have no labels?" has no vector to ask it
// with. It is a scan of a small, indexed table, and that is the honest shape for it.
//
// The unlabelled filter is the reason the method exists. Access policy acts on
// labels and never on a document by name, so an unlabelled document is not denied —
// it is invisible to the policy model. Before this there was no way to find one
// except by already knowing what it said, which is the one thing an operator
// auditing their own corpus does not know.
func (p *PgVectorAdapter) ListDocuments(ctx context.Context, f domain.DocumentFilter) ([]domain.DocumentSummary, string, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultDocumentPage
	}
	if limit > maxDocumentPage {
		limit = maxDocumentPage
	}

	// The filter is built once and shared by the count and the page, so the two can
	// never disagree about what they describe. A total computed against different
	// criteria than the rows is worse than no total: it still looks authoritative.
	var filters []string
	var args []any
	placeholder := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	// The owner fence, unconditionally (domain/owner_scope.go): a listing returns
	// titles and labels, and a personal mailbox's subject lines are content.
	if f.Scope != nil {
		// A principal-serving listing (ENG-725): the reader's whole predicate,
		// rendered by the one SQL mirror (sqlfence; AGENTS.md §2.5), owner fence
		// included. In the WHERE, so it filters before LIMIT and the count sees
		// exactly what the page sees — a post-filter would short-page and let
		// total_matching count rows the reader may not read.
		expr, fenceArgs := sqlfence.TextArray("d.tags").Expr(f.Scope, len(args))
		args = append(args, fenceArgs...)
		if expr != "TRUE" {
			filters = append(filters, expr)
		}
	} else {
		held := (&domain.TagPredicate{Owners: f.Owners}).HeldOwnerTags()
		args = append(args, held)
		filters = append(filters, sqlfence.OwnerFence("d.tags", len(args)))
	}
	if f.UnlabelledOnly {
		filters = append(filters, "cardinality(d.tags) = 0")
	}
	if len(f.Tags) > 0 {
		// @> is the GIN-indexed containment operator: "carries all of these".
		// Intersection, matching DocumentFilter.Tags — OR would make a second
		// label widen the scope rather than narrow it.
		filters = append(filters, "d.tags @> "+placeholder(f.Tags))
	}
	entity := domain.IsSourceDocEntityID(f.IDPrefix)
	switch {
	case entity:
		// ENG-2184 (owner decision 2026-11-19): a source-doc ENTITY id resolves to the
		// document row(s) it stands for — one per writer of the same source — through
		// the links the ingest itself wrote: the entity row's own document_id (the
		// latest writer's row), every chunk stamped with the entity
		// (metadata.source_doc_entity_id, GIN-indexed containment), and an embed job
		// still staging one (derivation_jobs.staged.doc.entity_id, a document accepted
		// for background embedding whose chunks are not written yet). The scope /
		// owner-fence filters above still apply: a reader resolves only rows it may
		// read. No id is re-derived from the URI.
		filters = append(filters, `d.id IN (`+entityDocumentRowsSQL(placeholder(f.IDPrefix), placeholder(entityStamp(f.IDPrefix)))+`)`)
	case f.IDPrefix != "":
		// ESCAPE is explicit because document ids routinely contain '_', which LIKE
		// reads as "any single character" — without it a prefix filter silently
		// matches more than the operator asked for.
		filters = append(filters, "d.id LIKE "+placeholder(escapeLike(f.IDPrefix))+` || '%' ESCAPE '\'`)
	}

	where := func(extra ...string) string {
		all := append(append([]string{}, filters...), extra...)
		if len(all) == 0 {
			return ""
		}
		return " WHERE " + strings.Join(all, " AND ")
	}

	var total int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+TableDocuments+` d`+where(), args...).Scan(&total); err != nil {
		return nil, "", 0, mapError("ListDocuments", err)
	}

	// The cursor narrows the PAGE only, never the count: the total describes the
	// whole matching set, which is what makes "422 of 1163" mean anything.
	pageWhere := where()
	order := "d.id"
	if entity {
		// An entity's rows (one per writer — a handful) come newest first: the most
		// recently written row is the entity's answer where one is wanted
		// (GetIngestStatus, the console's document view). One page, no cursor.
		order = "d.updated_at DESC, d.id"
	} else if f.Cursor != "" {
		pageWhere = where("d.id > " + placeholder(f.Cursor))
	}
	// Over-fetch one row: it is the evidence that another page exists.
	rowLimit := placeholder(limit + 1)

	rows, err := p.pool.Query(ctx,
		`SELECT d.id, d.title, d.source_type, d.tags, d.created_at,
		        (SELECT count(*) FROM `+TableChunks+` c WHERE c.document_id = d.id)
		 FROM `+TableDocuments+` d`+pageWhere+`
		 ORDER BY `+order+`
		 LIMIT `+rowLimit, args...)
	if err != nil {
		return nil, "", 0, mapError("ListDocuments", err)
	}
	defer rows.Close()

	out := make([]domain.DocumentSummary, 0, limit)
	for rows.Next() {
		var s domain.DocumentSummary
		if err := rows.Scan(&s.ID, &s.Title, &s.SourceType, &s.Tags, &s.CreatedAt, &s.ChunkCount); err != nil {
			return nil, "", 0, mapError("ListDocuments", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, mapError("ListDocuments", err)
	}

	// The over-fetched row is never returned. Reporting a cursor merely because a
	// page came back FULL would hand the client one guaranteed-empty round trip at
	// the end of every listing.
	next := ""
	if len(out) > limit {
		out = out[:limit]
		if !entity {
			next = out[len(out)-1].ID
		}
	}
	return out, next, total, nil
}

// escapeLike neutralises the LIKE metacharacters, paired with an explicit
// ESCAPE '\' in the query.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// entityDocumentRowsSQL is THE resolution of a source-doc entity id to the document
// rows it stands for (ENG-2184; one per writer of the same source), as a SELECT of
// document ids: the entity row's own document_id (the latest writer's row), every
// chunk stamped with the entity (metadata.source_doc_entity_id, GIN-indexed
// containment), and an embed job still staging one. entity and stamp are SQL
// placeholders bound to the entity id and to entityStamp(entity id). The document
// listing (ListDocuments) and the erasure selector (PgErasureStore.Resolve / Erase,
// ENG-2245) both resolve through it, so the id a console holds names the same rows
// everywhere.
func entityDocumentRowsSQL(entity, stamp string) string {
	return `SELECT c.document_id FROM ` + TableChunks + ` c WHERE c.id = ` + entity + ` AND c.document_id IS NOT NULL
			UNION SELECT c.document_id FROM ` + TableChunks + ` c WHERE c.metadata @> ` + stamp + `::jsonb AND c.document_id IS NOT NULL
			UNION SELECT j.document_id FROM ` + TableDerivationJobs + ` j WHERE j.kind = 'embed' AND j.staged->'doc'->>'entity_id' = ` + entity + ` AND j.document_id IS NOT NULL`
}

// entityStamp is the chunk-metadata containment value that marks a chunk as part of
// a source-doc entity.
func entityStamp(entityID string) string {
	b, _ := json.Marshal(map[string]string{"source_doc_entity_id": entityID}) // a map of strings always marshals
	return string(b)
}

// entityDocumentRows resolves a source-doc entity id to its document row ids
// (entityDocumentRowsSQL), sorted and de-duplicated.
func entityDocumentRows(ctx context.Context, q pgxQuerier, entityID string) ([]string, error) {
	return collectIDs(ctx, q, `SELECT DISTINCT r.document_id FROM (`+entityDocumentRowsSQL("$1", "$2")+`) r(document_id) ORDER BY 1`,
		entityID, entityStamp(entityID))
}

// expandEntityIDs replaces every source-doc entity id in ids by its document rows,
// keeping plain row ids; via maps each resulting row id to the entity id that named
// it (row ids named directly map to themselves). An entity with no rows contributes
// nothing.
func expandEntityIDs(ctx context.Context, q pgxQuerier, ids []string) ([]string, map[string]string, error) {
	out := make([]string, 0, len(ids))
	via := map[string]string{}
	for _, id := range ids {
		if !domain.IsSourceDocEntityID(id) {
			if _, seen := via[id]; !seen {
				out = append(out, id)
			}
			via[id] = id
			continue
		}
		rows, err := entityDocumentRows(ctx, q, id)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			if _, seen := via[r]; !seen {
				out = append(out, r)
				via[r] = id
			}
		}
	}
	return out, via, nil
}
