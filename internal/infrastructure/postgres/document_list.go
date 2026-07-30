package postgres

import (
	"context"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/internal/memory"
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
func (p *PgVectorAdapter) ListDocuments(ctx context.Context, f memory.DocumentFilter) ([]memory.DocumentSummary, string, int, error) {
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
	if f.UnlabelledOnly {
		filters = append(filters, "cardinality(d.tags) = 0")
	}
	if len(f.Tags) > 0 {
		// @> is the GIN-indexed containment operator: "carries all of these".
		// Intersection, matching DocumentFilter.Tags — OR would make a second
		// label widen the scope rather than narrow it.
		filters = append(filters, "d.tags @> "+placeholder(f.Tags))
	}
	if f.IDPrefix != "" {
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
	if f.Cursor != "" {
		pageWhere = where("d.id > " + placeholder(f.Cursor))
	}
	// Over-fetch one row: it is the evidence that another page exists.
	rowLimit := placeholder(limit + 1)

	rows, err := p.pool.Query(ctx,
		`SELECT d.id, d.title, d.source_type, d.tags, d.created_at,
		        (SELECT count(*) FROM `+TableChunks+` c WHERE c.document_id = d.id)
		 FROM `+TableDocuments+` d`+pageWhere+`
		 ORDER BY d.id
		 LIMIT `+rowLimit, args...)
	if err != nil {
		return nil, "", 0, mapError("ListDocuments", err)
	}
	defer rows.Close()

	out := make([]memory.DocumentSummary, 0, limit)
	for rows.Next() {
		var s memory.DocumentSummary
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
		next = out[len(out)-1].ID
	}
	return out, next, total, nil
}

// escapeLike neutralises the LIKE metacharacters, paired with an explicit
// ESCAPE '\' in the query.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
