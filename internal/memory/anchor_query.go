package memory

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Document-local ANCHOR retrieval (companion to the deterministic anchor tier in
// the kg_extractor agent). When chunks share a prose template — every "Chapter X,
// scene Y" paragraph looks alike to the embedder — dense similarity cannot pick
// the one the query names. The anchor tier stored `<doc_id --has_anchor--> kind:value>`
// triplets at ingest; here we parse the SAME normalized anchors out of the query
// and, when present, promote the chunks that carry them above the relevance floor
// so they survive into the returned window. No anchor in the query ⇒ no-op, and
// retrieval falls back to the ordinary hybrid/KG pipeline (the "fallback" arm of
// the retrieval policy).
//
// The normalization here MUST match anchor_extractor.py exactly, or the ingest
// token and the query token never meet in ChunksMentioningEntity.

var (
	anchorContainerKinds = []string{"chapter", "part", "book", "act", "section", "article", "title"}
	anchorMemberKinds    = []string{"scene", "step", "item", "clause", "verse", "stanza", "paragraph", "page", "line"}
	anchorOtherKinds     = []string{"appendix", "figure", "table"}

	// "<kind> <ordinal>" — ordinal is digits or a single alphabetic token
	// (number-word or a bare letter like Appendix B). Case-insensitive.
	anchorKindRE = regexp.MustCompile(`(?i)\b(` +
		strings.Join(append(append(append([]string{}, anchorContainerKinds...), anchorMemberKinds...), anchorOtherKinds...), "|") +
		`)\s+(\d{1,4}|[A-Za-z]+)\b`)

	anchorDecimalRE = regexp.MustCompile(`\b(\d+(?:\.\d+)+)\b`)
	// A hyphenated id-ish token that contains a digit: chunk ids the query cites
	// (tidebound-archive-chunk-5) and in-text ids (inv-2024). Lowercased input.
	anchorHyphenIDRE = regexp.MustCompile(`\b([a-z0-9]+(?:-[a-z0-9]+)+)\b`)
	anchorStatuteRE  = regexp.MustCompile(`§\s*(\d+(?:\.\d+)*[a-z]?)`)
	anchorHashRE     = regexp.MustCompile(`#(\d{2,})\b`)

	anchorRoman = map[rune]int{'i': 1, 'v': 5, 'x': 10, 'l': 50, 'c': 100, 'd': 500, 'm': 1000}
	anchorWords = map[string]int{
		"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
		"thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
		"eighteen": 18, "nineteen": 19, "twenty": 20, "first": 1, "second": 2,
		"third": 3, "fourth": 4, "fifth": 5, "sixth": 6, "seventh": 7, "eighth": 8,
		"ninth": 9, "tenth": 10,
	}
)

func anchorContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// normalizeAnchorOrdinal folds an ordinal token to the same canonical form the
// Python extractor uses: digits pass through, number-words and roman numerals
// fold to ints, a lone letter (Appendix B) stays a letter. "" ⇒ not an ordinal.
func normalizeAnchorOrdinal(token string) string {
	t := strings.ToLower(strings.TrimSpace(token))
	if t == "" {
		return ""
	}
	if n, err := strconv.Atoi(t); err == nil {
		return strconv.Itoa(n)
	}
	if v, ok := anchorWords[t]; ok {
		return strconv.Itoa(v)
	}
	if roman := anchorRomanToInt(t); roman > 0 {
		return strconv.Itoa(roman)
	}
	if len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' {
		return t
	}
	return ""
}

func anchorRomanToInt(s string) int {
	total, prev := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		v, ok := anchorRoman[rune(s[i])]
		if !ok {
			return 0
		}
		if v < prev {
			total -= v
		} else {
			total += v
			prev = v
		}
	}
	if total <= 0 {
		return 0
	}
	return total
}

// extractQueryAnchors parses the normalized anchor tokens out of a query string.
// Returns kind:value atomics, the compound container_member, decimal sections,
// explicit/hash/statute ids, and raw hyphenated id tokens (for chunk-id
// citations). Deduped, in a stable order (specific → atomic). Empty ⇒ no anchors.
func extractQueryAnchors(query string) []string {
	low := strings.ToLower(query)

	kindValues := map[string]string{}
	order := []string{}
	for _, m := range anchorKindRE.FindAllStringSubmatch(low, -1) {
		kind, ord := m[1], normalizeAnchorOrdinal(m[2])
		if ord == "" {
			continue
		}
		if _, seen := kindValues[kind]; !seen { // first match per kind wins (the heading)
			kindValues[kind] = ord
			order = append(order, kind)
		}
	}

	var atomics, specific []string
	for _, kind := range order {
		atomics = append(atomics, kind+":"+kindValues[kind])
	}

	// Compound: first present container + first present member.
	var container, member string
	for _, k := range anchorContainerKinds {
		if _, ok := kindValues[k]; ok {
			container = k
			break
		}
	}
	for _, k := range anchorMemberKinds {
		if _, ok := kindValues[k]; ok {
			member = k
			break
		}
	}
	if container != "" && member != "" {
		specific = append(specific, container+"_"+member+":"+kindValues[container]+"/"+kindValues[member])
	}

	for _, m := range anchorDecimalRE.FindAllStringSubmatch(low, -1) {
		specific = append(specific, "section:"+m[1])
	}
	for _, m := range anchorStatuteRE.FindAllStringSubmatch(query, -1) { // § needs the raw (unlowered is fine too)
		specific = append(specific, "statute:"+strings.ToLower(m[1]))
	}
	for _, m := range anchorHashRE.FindAllStringSubmatch(low, -1) {
		specific = append(specific, "id:"+m[1])
	}
	for _, m := range anchorHyphenIDRE.FindAllStringSubmatch(low, -1) {
		tok := m[1]
		if !strings.ContainsAny(tok, "0123456789") {
			continue // "harbor-magistrate" is not an id
		}
		specific = append(specific, tok)        // raw token matches a chunk-id subject
		specific = append(specific, "id:"+tok)  // "id:inv-2024" matches an in-text id anchor
	}

	// specific first (more selective), then atomics; dedupe preserving order.
	seen := map[string]bool{}
	out := make([]string, 0, len(specific)+len(atomics))
	for _, v := range append(specific, atomics...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// anchorIsSpecific reports whether an anchor token is a selective handle (a
// compound pair, a decimal section, an explicit/statute id, or a raw hyphenated
// id) as opposed to a broad atomic like "chapter:1" that can span many chunks.
func anchorIsSpecific(a string) bool {
	if strings.Contains(a, "_") ||
		strings.HasPrefix(a, "id:") || strings.HasPrefix(a, "section:") || strings.HasPrefix(a, "statute:") {
		return true
	}
	return strings.Contains(a, "-") && !strings.Contains(a, ":") // raw hyphenated id
}

// applyAnchorConstraint promotes the chunks that carry a query's document-local
// anchors above the relevance floor so they survive into the returned window,
// even when the reranker buried them among template-identical siblings. It runs
// AFTER the Stage-B rerank and BEFORE the floor/truncation. Policy:
//   - no anchor in the query, or no chunk carries it ⇒ return results unchanged
//     (fallback to the ordinary hybrid/KG ordering);
//   - prefer SPECIFIC anchors (compound / id / section); use broad atomics only
//     when no specific anchor is present, so "chapter:1" doesn't flood when
//     "chapter_scene:1/1" pins the single chunk;
//   - anchored chunks lead, scored 1.0 + query cosine (so multiple anchored
//     chunks — e.g. a two-id multi-hop query — all clear the floor and rank
//     first); non-anchored candidates keep their order after them.
func (q *QueryService) applyAnchorConstraint(ctx context.Context, results []domain.SearchResult, query string, vec []float32) []domain.SearchResult {
	if q.chunkTriplets == nil {
		return results
	}
	anchors := extractQueryAnchors(query)
	if len(anchors) == 0 {
		return results
	}
	use := anchors[:0:0]
	for _, a := range anchors {
		if anchorIsSpecific(a) {
			use = append(use, a)
		}
	}
	if len(use) == 0 {
		use = anchors // no specific handle ⇒ fall back to atomics
	}

	perEntity := q.kgPerEntity
	if perEntity <= 0 {
		perEntity = 5
	}
	anchored := make([]string, 0, len(use)*perEntity)
	inAnchored := map[string]bool{}
	for _, a := range use {
		ids, err := q.chunkTriplets.ChunksMentioningEntity(ctx, a, perEntity)
		if err != nil {
			slog.WarnContext(ctx, "anchor constraint: lookup failed", "anchor", a, "err", err)
			continue
		}
		for _, id := range ids {
			if !inAnchored[id] {
				inAnchored[id] = true
				anchored = append(anchored, id)
			}
		}
	}
	if len(anchored) == 0 {
		return results // anchors named nothing in the store ⇒ fallback
	}

	pos := make(map[string]int, len(results))
	for i, r := range results {
		pos[r.Document.ID] = i
	}

	const anchorBase = 1.0
	promoted := make([]domain.SearchResult, 0, len(results)+len(anchored))
	used := map[string]bool{}
	for _, id := range anchored {
		var r domain.SearchResult
		if i, ok := pos[id]; ok {
			r = results[i]
		} else {
			doc, err := q.vectorStore.GetByID(ctx, id)
			if err != nil || doc == nil {
				continue
			}
			r = domain.SearchResult{Document: *doc}
		}
		// Lift above the floor; add query cosine as a tiebreak among anchored
		// chunks. cosine in [0,1] for a materialized embedding, else 0.
		cos := 0.0
		if len(vec) > 0 && len(r.Document.Embedding.Vector) > 0 {
			cos = cosineSimilarity(vec, r.Document.Embedding.Vector)
		}
		r.Score = anchorBase + cos
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

// EnableAnchorConstraint turns on document-local anchor promotion (companion to
// the deterministic anchor tier). LLM-free; needs the chunk_triplets store.
// Flag-gated so the benchmark can A/B it independently of query-entity seeding.
func (q *QueryService) EnableAnchorConstraint() { q.anchorConstraint = true }
