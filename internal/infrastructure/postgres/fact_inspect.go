package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// The operator plane's read-only fact inspection (domain.FactInspector; owner
// decision 2026-11-14 on T-20261103-operator-plane-cannot-inspect-facts-no-rpc).
// PgFactItemReader already serves FactsByID and FactChain; ListFacts and
// FactCoverage are added here. Like every read of this reader, the predicate is
// applied IN SQL on knowledge_items.classification through sqlfence (the one fence
// renderer, ADR-0149 D8), and a nil predicate adds no term — the decision belongs to
// authz.EnforcingFactInspector, which refuses a read without one.

var _ domain.FactInspector = (*PgFactItemReader)(nil)

// factCursor is the ListFacts keyset position: the (asserted_at, id) of the last row
// of the previous page, ordered DESC. Opaque to the caller (base64 of
// "<RFC3339Nano>|<id>"); a cursor that does not decode is refused, never ignored —
// ignoring it would silently restart at page one.
func encodeFactCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeFactCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", domain.ErrFactCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return time.Time{}, "", domain.ErrFactCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", domain.ErrFactCursor
	}
	return t, id, nil
}

// factListWhere builds the WHERE clause of ListFacts after the fixed kind/namespace
// terms ($1, $2): the optional filters, the keyset cursor and the scope fence.
// Exposed to the unit test, which pins the SQL without a database.
func factListWhere(f domain.FactListFilter, scope *domain.TagPredicate) (string, []any, error) {
	args := []any{}
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args)+2)
	}
	var conds []string
	if s := domain.NormalizeFactText(f.Subject); s != "" {
		conds = append(conds, "split_part(ki.entity_id, '/', 2) = "+next(s))
	}
	if a := domain.NormalizeFactAttribute(f.Attribute); a != "" {
		conds = append(conds, "split_part(ki.entity_id, '/', 3) = "+next(a))
	}
	if f.Actor != "" {
		conds = append(conds, "ki.asserted_by = "+next(f.Actor))
	}
	if f.After != "" {
		at, id, err := decodeFactCursor(f.After)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, "(ki.asserted_at, ki.id) < ("+next(at)+", "+next(id)+")")
	}
	where := ""
	for _, c := range conds {
		where += " AND " + c
	}
	// Where numbers its placeholders from startIdx+1; the last one used so far is
	// $(len(args)+2), after the fixed $1/$2. (+3 left a gap: Postgres could not type
	// the unused parameter, SQLSTATE 42P18.)
	scopeSQL, scopeArgs := sqlfence.TextArray("ki.classification").Where(scope, len(args)+2)
	return where + scopeSQL, append(args, scopeArgs...), nil
}

// factListLimit clamps a requested page size.
func factListLimit(n int) int {
	switch {
	case n <= 0:
		return domain.FactListDefaultLimit
	case n > domain.FactListMaxLimit:
		return domain.FactListMaxLimit
	}
	return n
}

// ListFacts implements domain.FactInspector: stored memory_fact items (stated bounds,
// no derivation), newest assertion first, keyset-paged on (asserted_at, id).
func (r *PgFactItemReader) ListFacts(ctx context.Context, f domain.FactListFilter, scope *domain.TagPredicate) ([]domain.Fact, string, error) {
	where, args, err := factListWhere(f, scope)
	if err != nil {
		return nil, "", err
	}
	limit := factListLimit(f.Limit)
	// One row beyond the page tells whether a next page exists.
	items, err := loadItems(ctx, r.store.pool,
		`WHERE ki.kind = $1 AND ki.namespace_id = $2`+where+
			fmt.Sprintf(` ORDER BY ki.asserted_at DESC, ki.id DESC LIMIT %d`, limit+1),
		append([]any{domain.KindMemoryFact, r.namespace}, args...)...)
	if err != nil {
		return nil, "", fmt.Errorf("list facts: %w", err)
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeFactCursor(last.AssertedAt, string(last.ID))
	}
	out := make([]domain.Fact, 0, len(items))
	for _, it := range items {
		out = append(out, domain.FactFromItem(it))
	}
	return out, next, nil
}

// FactCoverage implements domain.FactInspector: counts only, over this reader's
// namespace. A document is "extracted" when it carries any memory_fact marker; the
// per-version rows split complete markers (every passage covered) from partial ones.
func (r *PgFactItemReader) FactCoverage(ctx context.Context) (domain.FactCoverage, error) {
	var c domain.FactCoverage
	var last *time.Time
	if err := r.store.pool.QueryRow(ctx, `SELECT
			(SELECT count(DISTINCT c.document_id) FROM chunks c WHERE c.document_id IS NOT NULL),
			(SELECT count(DISTINCT k.document_id) FROM knowledge_extractions k
				WHERE k.extractor = $1 AND k.namespace_id = $2),
			(SELECT count(*) FROM knowledge_items ki WHERE ki.kind = $3 AND ki.namespace_id = $2),
			(SELECT max(k.extracted_at) FROM knowledge_extractions k
				WHERE k.extractor = $1 AND k.namespace_id = $2)`,
		FactBackfillExtractor, r.namespace, domain.KindMemoryFact).Scan(
		&c.DocumentsWithChunks, &c.DocumentsExtracted, &c.FactItems, &last); err != nil {
		return c, fmt.Errorf("fact coverage: %w", err)
	}
	if last != nil {
		c.LastExtractedAt = last.UTC()
	}
	rows, err := r.store.pool.Query(ctx, `SELECT k.extractor_version,
			count(DISTINCT k.document_id) FILTER (WHERE COALESCE(k.passages_covered, 0) >= COALESCE(k.passages_total, 0)),
			count(DISTINCT k.document_id) FILTER (WHERE COALESCE(k.passages_covered, 0) < COALESCE(k.passages_total, 0)),
			max(k.extracted_at)
		FROM knowledge_extractions k
		WHERE k.extractor = $1 AND k.namespace_id = $2
		GROUP BY k.extractor_version ORDER BY max(k.extracted_at) DESC`,
		FactBackfillExtractor, r.namespace)
	if err != nil {
		return c, fmt.Errorf("fact coverage: versions: %w", err)
	}
	vs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.FactExtractorVersionCoverage, error) {
		var v domain.FactExtractorVersionCoverage
		var at time.Time
		err := row.Scan(&v.Version, &v.DocumentsComplete, &v.DocumentsPartial, &at)
		v.LastExtractedAt = at.UTC()
		return v, err
	})
	if err != nil {
		return c, fmt.Errorf("fact coverage: versions: %w", err)
	}
	c.Versions = vs
	return c, nil
}
