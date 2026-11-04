package memory

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 D4/D5, query side: the fact lane.
//
// A fact is a KEY into the chunk it came from. The lane searches the memory_fact items of
// the knowledge substrate (ADR-0136 A1: the item embedding projection, filtered IN SQL on
// the item classification) by the query vector under the SAME read predicate as the
// dense and lexical lanes, and each
// fact hit contributes its SOURCE CHUNK, fetched by id through the enforcing
// read store with the predicate on ctx (AGENTS.md §2.4) — so a fact whose chunk the
// reader may not see yields nothing, whatever the fact's own tags say
// (TestFacts_ScopeOnByIDRead). The chunks it reaches count as PRIMARY hits
// (owner-assumed Q3: they are real matches on a different key), so they enter before
// the primaryIDs snapshot; they still pass admit() (session isolation, the event-time
// window, the relevance floor on their own cosine) like every other row.
//
// The matched fact text rides on the hit as metadata (fact_matches), and for a
// superseded fact it says so: "superseded by <value> on <date>". A superseded fact's
// chunk is never suppressed — LongMemEval's error analysis shows knowledge-update
// evidence includes the old value — the reader sees both, annotated.

// defaultFactLaneTopK is the lane's fetch size when the configured one is unusable.
const defaultFactLaneTopK = 20

// maxFactChains bounds how many value chains one history/earliest query expands.
const maxFactChains = 3

// FactReader is the fact lane's narrowing of domain.FactItemReader. The value handed to
// NewQueryServiceWithFacts is authz.EnforcingFactItemReader over the substrate-backed
// postgres.PgFactItemReader (kernel.NewMemoryStack).
type FactReader interface {
	SearchFacts(ctx context.Context, vec []float32, opts domain.FactSearchOptions) ([]domain.FactHit, error)
	FactChain(ctx context.Context, f domain.Fact, scope *domain.TagPredicate) ([]domain.Fact, error)
}

// NewQueryServiceWithFacts is NewQueryService with the ADR-0136 fact reader as a
// CONSTRUCTOR argument (AGENTS.md §2.3: the reader is the enforcing fact reader, and
// there is no mutator that could attach an unenforced one later). nil facts ⇒ no fact
// lane, whatever RankingV2.FactLane says.
func NewQueryServiceWithFacts(embedder domain.Embedder, readStore domain.VectorStore, authorizer domain.Authorizer, facts FactReader) *QueryService {
	q := NewQueryService(embedder, readStore, authorizer)
	q.facts = facts
	return q
}

// HasFactReader reports whether the service was built with a fact reader (boot log).
func (q *QueryService) HasFactReader() bool { return q.facts != nil }

// factAnnotation is what the lane knows about one chunk.
type factAnnotation struct {
	score   float64  // best fact cosine to the query — the fact_match feature
	isCur   bool     // the chunk states the current value of a matched single fact
	lines   []string // reader-facing fact texts
	seenIDs map[string]bool
}

// factLaneHits maps a chunk id to its fact annotation. nil-safe.
type factLaneHits map[string]*factAnnotation

func (h factLaneHits) match(id string) float64 {
	if a := h[id]; a != nil {
		return a.score
	}
	return 0
}

func (h factLaneHits) current(id string) float64 {
	if a := h[id]; a != nil && a.isCur {
		return 1
	}
	return 0
}

func (h factLaneHits) anyCurrent() bool {
	for _, a := range h {
		if a.isCur {
			return true
		}
	}
	return false
}

func (h factLaneHits) add(chunkID string, score float64, current bool, f domain.Fact, line string) {
	a := h[chunkID]
	if a == nil {
		a = &factAnnotation{seenIDs: map[string]bool{}}
		h[chunkID] = a
	}
	if score > a.score {
		a.score = score
	}
	if current {
		a.isCur = true
	}
	if !a.seenIDs[f.ID] {
		a.seenIDs[f.ID] = true
		a.lines = append(a.lines, line)
	}
}

type factHitsKey struct{}

func withFactHits(ctx context.Context, h factLaneHits) context.Context {
	return context.WithValue(ctx, factHitsKey{}, h)
}

func factHitsFromContext(ctx context.Context) factLaneHits {
	h, _ := ctx.Value(factHitsKey{}).(factLaneHits)
	return h
}

// wantsCurrentValue: D3 orders under which the CURRENT value of a chain is the answer.
func wantsCurrentValue(ti TimeIntent) bool {
	return ti.Order == OrderCurrent || ti.Order == OrderLatest
}

// wantsHistory: D3 orders under which every version of a chain is the answer.
func wantsHistory(ti TimeIntent) bool {
	return ti.Order == OrderHistory || ti.Order == OrderEarliest
}

// isCurrentFact: the derivation says the value holds now (open, not a negation), and
// its predicate is not known to accumulate. A multi predicate's open values all hold at
// once, so "the current one" is not an answer the lane can point at; an unknown one is
// treated like single, because a closure there came from a cue or a verdict.
func isCurrentFact(f domain.Fact) bool {
	return f.Current && f.Cardinality != domain.FactMulti && f.Polarity != domain.FactNegative
}

func day(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// describeFactForReader renders one fact as the reader sees it.
func describeFactForReader(f domain.Fact, supersededByValue string, supersededAt *time.Time) string {
	s := f.Subject + " " + f.Attribute + ": " + f.Value
	if f.Polarity == domain.FactNegative {
		s = f.Subject + " " + f.Attribute + ": not " + f.Value
	}
	switch {
	case f.ValidFrom != nil && f.ValidFromSource == domain.ValidFromStated:
		s += " (since " + day(f.ValidFrom) + ")"
	case f.ValidFrom != nil:
		s += " (as of " + day(f.ValidFrom) + ")"
	}
	if f.SupersededBy != "" {
		at := supersededAt
		if at == nil {
			at = f.ValidTo
		}
		if supersededByValue != "" {
			s += fmt.Sprintf("; superseded by %s on %s", supersededByValue, day(at))
		} else {
			s += "; superseded on " + day(at)
		}
	} else if isCurrentFact(f) {
		s += "; current"
	}
	return s
}

// metaDocumentID is the chunk metadata key holding the chunk's source document id, the
// parent stamped at ingest (ingestion_chunks.go) and promoted to chunks.document_id by
// the adapter. The A4 scoped lane groups hits by it; the id itself is opaque.
const metaDocumentID = "document_id"

// hitDocumentID is the source document of a candidate row ("" when it has none — an
// episode or a kernel artifact, which the scoped lane simply skips).
func hitDocumentID(d domain.Document) string {
	id, _ := d.Metadata[metaDocumentID].(string)
	return id
}

// scopedFactDocuments is ADR-0136 A4.1's document set: the first n DISTINCT source
// documents of the candidate list, in rank order. The rows come from the scoped dense
// and lexical reads, so every document named is one the caller can already read.
func scopedFactDocuments(results []domain.SearchResult, n int) []string {
	if n <= 0 {
		return nil
	}
	seen := make(map[string]bool, n)
	var out []string
	for _, r := range results {
		id := hitDocumentID(r.Document)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= n {
			break
		}
	}
	return out
}

// mergeFactHits appends the scoped lane's hits that the global lane did not already
// return (by fact id), keeping the global hits first.
func mergeFactHits(global, scoped []domain.FactHit) []domain.FactHit {
	if len(scoped) == 0 {
		return global
	}
	seen := make(map[string]bool, len(global))
	for _, h := range global {
		seen[h.Fact.ID] = true
	}
	for _, h := range scoped {
		if !seen[h.Fact.ID] {
			seen[h.Fact.ID] = true
			global = append(global, h)
		}
	}
	return global
}

// injectFactHits is the lane. It returns results with the fact-reached chunks that
// were not already candidates appended, and the per-chunk annotations. Fail-open: a
// fact-store error leaves results unchanged (retrieval reads fail open, §3.2).
//
// ADR-0136 A4: with FactLaneScopedDocuments = N > 0 the lane runs a SECOND fact search,
// restricted to the facts of the first N distinct source documents of results (the
// fused dense+lexical pool, taken before this lane appends anything), under the same
// predicate. Its hits join the global ones (de-duplicated by fact id) and go through the
// same annotation, chain and by-id fetch below, so a scoped fact reaches its chunk
// exactly like a global one. N = 0 runs nothing new: the lane is byte-identical.
func (q *QueryService) injectFactHits(ctx context.Context, results []domain.SearchResult, vec []float32, eff *domain.TagPredicate, docType string) ([]domain.SearchResult, factLaneHits) {
	rv := q.rankingV2
	scopedDocs := scopedFactDocuments(results, rv.FactLaneScopedDocuments)
	hits, err := q.facts.SearchFacts(ctx, vec, domain.FactSearchOptions{TopK: rv.FactLaneTopK, Scope: eff})
	if err != nil {
		slog.WarnContext(ctx, "fact lane: fact search failed; lane skipped", "err", err)
		hits = nil
	}
	scopedFound := 0
	if len(scopedDocs) > 0 {
		scoped, serr := q.facts.SearchFacts(ctx, vec, domain.FactSearchOptions{
			TopK: rv.FactLaneScopedTopK, Scope: eff, SourceDocumentIDs: scopedDocs})
		if serr != nil {
			slog.WarnContext(ctx, "fact lane: scoped fact search failed; scoped lane skipped", "err", serr)
		} else {
			scopedFound = len(scoped)
			hits = mergeFactHits(hits, scoped)
		}
	}
	if len(hits) == 0 {
		return results, nil
	}
	ann := factLaneHits{}
	type pick struct {
		chunkID string
		order   float64 // append order key: descending score, or chain position for history
	}
	var picks []pick
	picked := map[string]bool{}
	pickChunk := func(id string, order float64) {
		if !picked[id] {
			picked[id] = true
			picks = append(picks, pick{id, order})
		}
	}
	for _, h := range hits {
		f := h.Fact
		cur := rv.Supersession && isCurrentFact(f)
		ann.add(f.SourceChunkID, h.Score, cur, f, describeFactForReader(f, h.SupersededByValue, h.SupersededAt))
		pickChunk(f.SourceChunkID, h.Score)
	}

	// D5 history/earliest: bring each matched chain in, in derived chain order,
	// so every version of the value (and when it changed) reaches the reader.
	if ti, ok := timeIntentFromContext(ctx); ok && rv.Supersession && rv.TemporalIntent && wantsHistory(ti) {
		chains := 0
		seenChain := map[string]bool{}
		for _, h := range hits {
			f := h.Fact
			if f.Cardinality == domain.FactMulti {
				continue
			}
			key := f.EntityID + "|" + f.Actor
			if seenChain[key] || chains >= maxFactChains {
				continue
			}
			seenChain[key] = true
			chains++
			chain, err := q.facts.FactChain(ctx, f, eff)
			if err != nil {
				slog.WarnContext(ctx, "fact lane: chain read failed", "err", err)
				continue
			}
			for pos, c := range chain {
				ann.add(c.SourceChunkID, h.Score, false, c, describeFactForReader(c, "", nil))
				// Chain members keep the matched hit's score as their fact_match (they
				// state the same attribute of the same subject) and enter in chain order.
				pickChunk(c.SourceChunkID, h.Score-float64(pos)*1e-9)
			}
		}
	}

	present := make(map[string]bool, len(results))
	for _, r := range results {
		present[r.Document.ID] = true
	}
	var missing []string
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].order > picks[j].order })
	for _, p := range picks {
		if !present[p.chunkID] {
			missing = append(missing, p.chunkID)
		}
	}
	if len(missing) == 0 {
		return results, ann
	}
	// The SOURCE CHUNKS, by id, through the enforcing read store: ctx carries the
	// predicate (searchByType seeded it), so a chunk the reader may not see is simply
	// absent — and with it the fact's contribution (its annotation is dropped below).
	docs, err := q.vectorStore.GetBatch(ctx, missing)
	if err != nil {
		slog.WarnContext(ctx, "fact lane: source chunk fetch failed; fact-reached chunks skipped", "err", err)
		docs = nil
	}
	byID := make(map[string]domain.Document, len(docs))
	for _, d := range docs {
		if docType != "" && d.DocumentType != "" && d.DocumentType != docType {
			continue
		}
		byID[d.ID] = d
	}
	added := 0
	for _, id := range missing {
		d, ok := byID[id]
		if !ok {
			delete(ann, id) // unreadable or gone: the fact contributes nothing
			continue
		}
		cos := 0.0
		if len(vec) > 0 && len(d.Embedding.Vector) > 0 {
			cos = cosineSimilarity(vec, d.Embedding.Vector)
		}
		score := cos
		if score <= 0 {
			score = ann.match(id)
		}
		results = append(results, domain.SearchResult{Document: d, Score: score, RawScore: cos})
		added++
	}
	slog.DebugContext(ctx, "fact lane", "facts", len(hits), "chunks", len(ann), "added", added,
		"scoped_documents", len(scopedDocs), "scoped_facts", scopedFound)
	return results, ann
}

// annotateFactHits stamps the reader-facing fact text onto the returned rows. The
// metadata map is COPIED before it is written: a document's map may be shared with
// whatever produced the row.
func annotateFactHits(rows []domain.SearchResult, hits factLaneHits) []domain.SearchResult {
	if len(hits) == 0 {
		return rows
	}
	for i := range rows {
		a := hits[rows[i].Document.ID]
		if a == nil {
			continue
		}
		meta := make(map[string]any, len(rows[i].Document.Metadata)+3)
		maps.Copy(meta, rows[i].Document.Metadata)
		meta[domain.MetaFactMatches] = append([]string(nil), a.lines...)
		meta[domain.MetaFactMatchScore] = a.score
		if a.isCur {
			meta[domain.MetaFactCurrent] = true
		}
		rows[i].Document.Metadata = meta
	}
	return rows
}
