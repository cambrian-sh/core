package memory

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// Structure-graph retrieval (ADR-0060 Phase 3). When a query names a document
// location — "what does section 3.2 say", "in the genetics chapter" — the chunks
// physically under that section (resolved via the parser-derived section graph +
// ltree) are promoted above the relevance floor. This is the structural analog of
// applyAnchorConstraint: it generalizes the regex anchor to the real hierarchy,
// so it also works when the heading text isn't in the chunk. No section match ⇒
// no-op (retrieval falls back to hybrid + KG as before).

// SectionScopedStore resolves query location terms to chunk ids under matching
// sections. The pgvector adapter implements it.
type SectionScopedStore interface {
	ChunksInMatchingSections(ctx context.Context, terms []string, limit int) ([]string, error)
}

// EnableSectionScopedRetrieval wires the structure-graph section boost.
func (q *QueryService) EnableSectionScopedRetrieval(store SectionScopedStore) {
	if q != nil && store != nil {
		q.sectionStore = store
	}
}

// sectionKeywordStop are the structural keywords themselves + generic words we
// must NOT match a section title on (else "section" matches every section).
var sectionKeywordStop = map[string]bool{
	"chapter": true, "section": true, "subsection": true, "part": true, "book": true,
	"unit": true, "lecture": true, "appendix": true, "article": true, "module": true,
	"about": true, "which": true, "where": true, "there": true, "these": true,
	"those": true, "their": true, "would": true, "should": true, "could": true,
}

// sectionNumRE matches a hierarchical section number (requires a dot so a bare
// digit doesn't match every "Chapter 3"/"Section 3.x" title): 3.2, 4.10.1.
var sectionNumRE = regexp.MustCompile(`\b\d+(?:\.\d+)+\b`)

// extractSectionTerms pulls the DISTINCTIVE tokens that can localize a query to a
// section: hierarchical section numbers (3.2) and content words (len >= 5, not a
// structural keyword). Generic/short words are dropped so a title match is
// selective. A non-matching term simply returns no chunks downstream, so extra
// candidates are harmless.
func extractSectionTerms(query string) []string {
	low := strings.ToLower(query)
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, m := range sectionNumRE.FindAllString(low, -1) {
		add(m)
	}
	// A hierarchical number (3.2) is a precise locator — a topic word like
	// "sampling" matches the same-named section in every chapter. So when a number
	// is present, use ONLY it (prefer-specific); fall back to content words when
	// the query names a section by title alone ("the photosynthesis section").
	if len(out) > 0 {
		return out
	}
	for _, w := range strings.FieldsFunc(low, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len(w) >= 5 && !sectionKeywordStop[w] {
			add(w)
		}
	}
	return out
}

// applySectionConstraint promotes chunks under a section the query names. Runs
// after Stage-B rerank, before the floor/truncation — same shape as
// applyAnchorConstraint. Anchored chunks lead (score 1.0 + query cosine); the
// rest keep their order.
func (q *QueryService) applySectionConstraint(ctx context.Context, results []domain.SearchResult, query string, vec []float32) []domain.SearchResult {
	if q.sectionStore == nil {
		return results
	}
	terms := extractSectionTerms(query)
	if len(terms) == 0 {
		return results
	}
	budget := q.kgMaxExpanded
	if budget <= 0 {
		budget = 20
	}
	ids, err := q.sectionStore.ChunksInMatchingSections(ctx, terms, budget)
	if err != nil {
		slog.WarnContext(ctx, "section constraint: lookup failed", "err", err)
		return results
	}
	if len(ids) == 0 {
		return results // no section named / matched ⇒ fallback
	}

	pos := make(map[string]int, len(results))
	for i, r := range results {
		pos[r.Document.ID] = i
	}
	const sectionBase = 1.0
	promoted := make([]domain.SearchResult, 0, len(results)+len(ids))
	used := make(map[string]bool, len(ids))
	for _, id := range ids {
		var r domain.SearchResult
		if i, ok := pos[id]; ok {
			r = results[i]
		} else {
			doc, derr := q.vectorStore.GetByID(ctx, id)
			if derr != nil || doc == nil {
				continue
			}
			r = domain.SearchResult{Document: *doc}
		}
		cos := 0.0
		if len(vec) > 0 && len(r.Document.Embedding.Vector) > 0 {
			cos = cosineSimilarity(vec, r.Document.Embedding.Vector)
		}
		r.Score = sectionBase + cos
		used[id] = true
		promoted = append(promoted, r)
	}
	for _, r := range results {
		if used[r.Document.ID] {
			continue
		}
		promoted = append(promoted, r)
	}
	return promoted
}
