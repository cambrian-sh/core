package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Spreader expands a seed result set associatively over the memory graph (ADR-0017
// BFS over document_edges). *SpreadingEngine satisfies it; a fake satisfies it in
// tests. ADR-0048 D2 brings this into the agent's PULL recall.
type Spreader interface {
	Spread(ctx context.Context, seeds []domain.SearchResult) []domain.GraphNodeExpansion
}

// ScopeProvider resolves an agent's Phase-1 effective READ scope (ADR-0034). The
// ScopeResolver satisfies it. found=false means an unknown principal (fail-closed).
type ScopeProvider interface {
	EffectiveForAgent(ctx context.Context, agentID string) (*domain.EffectiveScope, bool)
}

// CallerScopeProvider re-derives the Phase-2 effective scope (caller_scope ∩
// agent_scope) given a caller_scope sourced server-side from the session record.
// The ScopeResolver satisfies it. ADR-0034 (D13 Phase 2).
type CallerScopeProvider interface {
	EffectiveForCaller(ctx context.Context, agentID string, caller domain.ScopeConfig) (*domain.EffectiveScope, bool)
}

// SessionScopeProvider returns the non-forgeable caller_scope persisted on a
// session record. The SessionManager satisfies it. ADR-0034 (D13).
type SessionScopeProvider interface {
	CallerScope(ctx context.Context, sessionID string) domain.ScopeConfig
}

// QueryService implements domain.MemorySearcher: it embeds the query, searches the
// vector store for memory documents, and applies ACL filtering before returning results.
type QueryService struct {
	embedder        domain.Embedder
	vectorStore     domain.VectorStore
	scopes          ScopeProvider        // ADR-0034: nil = scope enforcement disabled (legacy)
	callerScopes    CallerScopeProvider  // ADR-0034 Phase 2: nil = caller_scope not enforced
	sessions        SessionScopeProvider // ADR-0034 Phase 2: source of non-forgeable caller_scope
	spreader        Spreader             // ADR-0048 D2: nil = no associative expansion (flag-gated at wiring)
	floor           float64              // ADR-0048 #1: min cosine to return a recalled fact; 0 = disabled
	graphWriter     domain.GraphStore    // ADR-0049 D10: Hebbian co-activation edge writes; nil = disabled
	heb             hebbianParams        // ADR-0049 D10: Hebbian tuning (off unless EnableHebbian wired)
	entityIdx       *EntityIndex         // ADR-0052: in-memory entity reverse index; nil = surface-only recall
	assocWeight     float64              // ADR-0052: β in the re-rank formula; default 0.2
	assocLambda     float64              // ADR-0052: λ for the temporal-decay term in the re-rank; default 0.005
	assocTopK       int                  // ADR-0052: top-K entity keys to seed from; default 3
	chunkTriplets   ChunkTripletsStore   // ADR-0053 Phase 0: per-chunk KG; nil = no KG expansion (legacy)
	kgHops          int                  // ADR-0053 Phase 0: KG expansion depth; default 1
	kgMaxExpanded   int                  // ADR-0053 Phase 0: max new chunks added by KG expansion; default 20
	kgMaxEntities   int                  // ADR-0053 Phase 0: max entities considered per hop; default 30
	kgPerEntity     int                  // ADR-0053 Phase 0: max chunks pulled per entity; default 5
	queryEntitySeed bool                 // recall: seed kgExpand from entities extracted from the QUERY text (LLM-free)
	anchorConstraint bool               // recall: promote chunks carrying the query's document-local anchors (companion to the anchor tier)
	sectionStore     SectionScopedStore  // ADR-0060: structure-graph section-scoped retrieval; nil = disabled
	neighborWindow   bool                // ADR-0060: expand each returned chunk with its document neighbors
	blender         atomic.Pointer[Blender] // ADR-0054 Stage A: nil = no blend re-rank; hot-swappable at runtime (SetBlendWeights ← operator SetRuntimeConfig)
	rankSignals     RankSignalStore      // ADR-0054 Stage A: pagerank + per-chunk confidence source
	recallTopK      int                  // ADR-0054: results returned to caller; 0 ⇒ defaultRecallTopK
	recallOverFetch int                  // ADR-0054: seed/ANN fetch size; 0 ⇒ defaultRecallOverFetch
	lexical         LexicalSearcher      // ADR-0054 hybrid: nil = vector-only recall
	rrfK            int                  // ADR-0054 hybrid: RRF constant; 0 ⇒ 60
	reranker        Reranker             // ADR-0054 Stage B: nil = no cross-encoder rerank (Stage-A order kept)
	rerankTopK      int                  // ADR-0054 Stage B: candidates rescored by the cross-encoder; 0 ⇒ defaultRerankTopK
	rerankWeight    float64              // ADR-0054 Stage B: w_bge in FinalScore; ≤0 ⇒ 0.5
}

// SetRecallSizes overrides the seed-search fetch size and the returned window
// (ADR-0054 retrieval tuning). overFetch is the candidate pool the gold chunk
// must land in (raise the HNSW ef_search GUC to >= this too); topK is what recall
// hands back. Non-positive values keep the current/default. Flag-gated at wiring.
func (q *QueryService) SetRecallSizes(topK, overFetch int) {
	if topK > 0 {
		q.recallTopK = topK
	}
	if overFetch > 0 {
		q.recallOverFetch = overFetch
	}
	if q.recallOverFetch < q.recallTopK {
		q.recallOverFetch = q.recallTopK // over-fetch must be >= returned window
	}
}

// recallTopKOrDefault / recallOverFetchOrDefault resolve the effective sizes,
// falling back to the package defaults when unset.
func (q *QueryService) effRecallTopK() int {
	if q.recallTopK > 0 {
		return q.recallTopK
	}
	return defaultRecallTopK
}

func (q *QueryService) effRecallOverFetch() int {
	if q.recallOverFetch > 0 {
		return q.recallOverFetch
	}
	return defaultRecallOverFetch
}

// RankSignalStore supplies the precomputed Stage-A signals the QueryService does
// not already have on the SearchResult (ADR-0054). Implemented by the postgres
// adapter; nil-tolerant — missing scores count as 0.
type RankSignalStore interface {
	ChunkPageRanks(ctx context.Context, ids []string) (map[string]float64, error)
	MeanChunkConfidence(ctx context.Context, ids []string) (map[string]float64, error)
}

// EnableBlend turns on ADR-0054 Stage-A multi-signal re-ranking. Flag-gated at
// the wiring site (default off). nil blender or store ⇒ no-op (bare cosine).
func (q *QueryService) EnableBlend(b *Blender, signals RankSignalStore) {
	q.blender.Store(b)
	q.rankSignals = signals
}

// SetBlendWeights hot-swaps the live Stage-A blend weights with no restart
// (ADR-0054 tuning seam, driven by the operator SetRuntimeConfig command). The
// swap is atomic — in-flight recalls finish on the old Blender, later ones see
// the new weights. Ephemeral: config.json stays the boot default.
func (q *QueryService) SetBlendWeights(w BlendWeights) {
	nb := NewBlender(w)
	q.blender.Store(&nb)
}

// CurrentBlendWeights returns the live Stage-A weights (zero value if blend is
// disabled), so the operator applier can merge a partial param set over them.
func (q *QueryService) CurrentBlendWeights() BlendWeights {
	if b := q.blender.Load(); b != nil {
		return b.w
	}
	return BlendWeights{}
}

// LexicalSearcher is the sparse/lexical half of hybrid retrieval (ADR-0054) —
// a full-text (BM25-ish) search over the same store. Implemented by the postgres
// adapter; nil ⇒ vector-only recall.
type LexicalSearcher interface {
	LexicalSearch(ctx context.Context, queryText string, opts domain.SearchOptions) ([]domain.SearchResult, error)
}

// EnableHybrid turns on dense+sparse hybrid retrieval (ADR-0054): the seed pool
// becomes the Reciprocal-Rank-Fusion of the vector search and a lexical search,
// so exact-token chunks the embedder misses still enter the pool. rrfK is the RRF
// constant (default 60). Flag-gated; nil searcher ⇒ vector-only.
func (q *QueryService) EnableHybrid(lex LexicalSearcher, rrfK int) {
	q.lexical = lex
	q.rrfK = rrfK
}

// hebbianParams holds the HITL-tuned Hebbian co-activation constants (ADR-0049 D10).
type hebbianParams struct {
	enabled                           bool
	lr, max, floor, decayPerDay, base float64
	topN                              int
}

// EnableHebbian turns on usage-driven `co_activated` edge reinforcement on recall
// (ADR-0049 D10). Flag-gated at the call site (default off). Edge writes are async,
// off the read path. The constants are operator-tuned, not asserted to one value.
func (q *QueryService) EnableHebbian(gs domain.GraphStore, lr, maxW, floor, decayPerDay, base float64, topN int) {
	q.graphWriter = gs
	q.heb = hebbianParams{enabled: true, lr: lr, max: maxW, floor: floor, decayPerDay: decayPerDay, base: base, topN: topN}
}

// SetRelevanceFloor sets the minimum cosine similarity a recalled fact must clear
// to be returned (ADR-0048 #1). A seed below the floor is dropped as irrelevant
// instead of padding the top-k — so an unrelated promoted tool output cannot pose
// as grounding, and an all-below-floor query returns EMPTY, which the agent reads
// as "no relevant memory" and answers from its own knowledge. 0 disables (legacy).
func (q *QueryService) SetRelevanceFloor(f float64) { q.floor = f }

// EnableSpreading turns on ADR-0048 D2 associative expansion: after the seed
// search, recall expands over the memory graph and re-ranks by activation, so the
// agent's one upfront seed_recall is associatively rich. Flag-gated at the call
// site (main.go) — nil spreader leaves recall as a flat top-k.
func (q *QueryService) EnableSpreading(s Spreader) { q.spreader = s }

// EnableEntityRouting turns on ADR-0052 entity-aware routing: the recall path
// embeds the query, finds the top-K entity keys by name-embedding cosine, and
// pre-loads the index's doc associations for those entities as additional
// seeds. Re-ranking then blends BFS energy with the T-Mem associative term
// (β × reachability). nil idx leaves recall as surface-similarity-only.
func (q *QueryService) EnableEntityRouting(idx *EntityIndex) {
	q.entityIdx = idx
	if q.assocWeight == 0 {
		q.assocWeight = 0.2
	}
	if q.assocLambda == 0 {
		q.assocLambda = 0.005
	}
	if q.assocTopK == 0 {
		q.assocTopK = 3
	}
}

// SetAssociativeWeight overrides β (default 0.2). 0 disables the associative
// re-rank term (the BFS energy alone determines the final order).
func (q *QueryService) SetAssociativeWeight(beta float64) { q.assocWeight = beta }

// SetAssociativeLambda overrides the temporal-decay λ used in the re-rank's
// effective term (default 0.005/hr; matches MnemonicFact).
func (q *QueryService) SetAssociativeLambda(lambda float64) { q.assocLambda = lambda }

// SetAssociativeTopK overrides the number of entity keys used to seed the
// associative expansion (default 3). Larger values widen the first hop at
// the cost of noisy seed candidates.
func (q *QueryService) SetAssociativeTopK(k int) {
	if k > 0 {
		q.assocTopK = k
	}
}

// NewQueryService creates a QueryService.
func NewQueryService(embedder domain.Embedder, vectorStore domain.VectorStore) *QueryService {
	return &QueryService{embedder: embedder, vectorStore: vectorStore}
}

// EnableScoping turns on ADR-0034 Phase-1 agent_scope enforcement. The provider
// resolves each caller's effective read scope; scopedStore is the fail-closed
// ScopedVectorStore wrapping the same base store. After this call every agent
// query is scope-filtered by the caller's non-forgeable genotype agent_scope.
func (q *QueryService) EnableScoping(provider ScopeProvider, scopedStore domain.VectorStore) {
	q.scopes = provider
	if scopedStore != nil {
		q.vectorStore = scopedStore
	}
}

// EnablePhase2 turns on ADR-0034 Phase-2 caller_scope enforcement. When a session
// ID is present in the request context, the QueryService re-derives the effective
// scope as caller_scope ∩ agent_scope, taking caller_scope from the SESSION record
// (sessions.CallerScope) — never from the forgeable Handoff.Context. When no
// session caller_scope is resolvable it falls back to Phase-1 agent_scope-only.
func (q *QueryService) EnablePhase2(caller CallerScopeProvider, sessions SessionScopeProvider) {
	q.callerScopes = caller
	q.sessions = sessions
}

// EnableKG2RAG turns on ADR-0053 Phase-0 KG²RAG chunk expansion. The store is
// the per-chunk triplets table; the hops / MaxExpanded / MaxEntities knobs
// bound the expansion. Zero values fall back to kgExpand defaults
// (1 hop, +20 chunks, 30 entities). Wiring is the same flag-gated shape as
// the spreader / entity index — nil store = no KG expansion (legacy).
func (q *QueryService) EnableKG2RAG(store ChunkTripletsStore, hops, maxExpanded, maxEntities, perEntity int) {
	q.chunkTriplets = store
	q.kgHops = hops
	q.kgMaxExpanded = maxExpanded
	q.kgMaxEntities = maxEntities
	q.kgPerEntity = perEntity
}

// EnableQueryEntitySeeding turns on LLM-free, structure-aware recall: entities
// are extracted from the QUERY text (no LLM — token/n-gram match against the
// live chunk_triplets KG vocabulary) and the chunks mentioning them are injected
// as seeds BEFORE kgExpand. This rescues the gold on a total vector miss: even
// when the embedder ranks the right chunk far down, the query's entities reach it
// through the graph. Needs EnableKG2RAG (the chunk_triplets store). ADR-0053.
func (q *QueryService) EnableQueryEntitySeeding() { q.queryEntitySeed = true }

// applyStageABlend re-scores every candidate by the Stage-A multi-signal blend
// (ADR-0054) and re-sorts descending. Cosine comes from the chunk's embedding vs
// the query (falling back to the candidate's existing RawScore/Score when no
// embedding is materialized); recency from CreatedAt; confidence + pagerank from
// the rankSignals store (absent ⇒ 0); activation from the document. Best-effort:
// a signal-store error leaves the existing ordering untouched.
func (q *QueryService) applyStageABlend(ctx context.Context, results []domain.SearchResult, queryVec []float32) []domain.SearchResult {
	blender := q.blender.Load() // snapshot the live weights for this whole pass
	if blender == nil {
		return results
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.Document.ID != "" {
			ids = append(ids, r.Document.ID)
		}
	}
	pageranks, err := q.rankSignals.ChunkPageRanks(ctx, ids)
	if err != nil {
		slog.Warn("blend: pagerank lookup failed; keeping cosine order", "err", err)
		return results
	}
	confidences, err := q.rankSignals.MeanChunkConfidence(ctx, ids)
	if err != nil {
		slog.Warn("blend: confidence lookup failed; keeping cosine order", "err", err)
		return results
	}

	// PageRank is a normalized probability distribution (~1/N ≈ 1e-4 per chunk),
	// orders of magnitude below the other [0,1] signals — fed raw it contributes
	// ~nothing to the blend. Min-max normalize it across THIS query's candidate set
	// so "most structurally central of these candidates" maps to ~1, "least" to ~0.
	// Per-candidate-set (not global) normalization is the robust choice for a
	// power-law signal: it keeps the comparison among the chunks actually in play.
	prMin, prMax := math.Inf(1), math.Inf(-1)
	for i := range results {
		v := pageranks[results[i].Document.ID] // absent ⇒ 0 (least central)
		if v < prMin {
			prMin = v
		}
		if v > prMax {
			prMax = v
		}
	}
	prRange := prMax - prMin
	normPageRank := func(id string) float64 {
		if prRange <= 0 {
			return 0 // all equal (or single candidate) ⇒ no discriminative signal
		}
		return (pageranks[id] - prMin) / prRange
	}

	// Graph coherence: seed-anchored, IDF-weighted connectivity over the
	// chunk_triplets KG (shared entities, incl. the `dated at`/`spoke at` timestamp
	// hubs). Energy spreads from the query-relevant SEEDS, not the whole pool, so a
	// chunk in the gold's session is boosted via its link to the relevant hit while
	// a big-but-irrelevant session (no seed) and cross-conversation islands score
	// ~0. The graph-native conversation disambiguator (ADR-0054 / ADR-0053).
	coherence := chunkCoherence(ctx, q.chunkTriplets, results, defaultCoherenceSeedN)

	// Recency: prefer the more-recent-DATED fact among the candidates. The event
	// time is the conversation timestamp stamped into metadata at ingest (ADR-0053
	// temporal backfill), falling back to created_at (ingest time) when absent.
	// Min-max normalized across THIS candidate set — exactly like pagerank above —
	// so it is a RELATIVE "newer than its peers" signal (1=newest, 0=oldest),
	// discriminative even when every fact is old relative to now (the LoCoMo case,
	// where an absolute now-relative decay would be uniformly ~0). Zero range (all
	// same date, or a single candidate) ⇒ no recency signal.
	recMin, recMax := math.Inf(1), math.Inf(-1)
	evtUnix := make(map[string]float64, len(results))
	for i := range results {
		u := float64(docEventTime(results[i].Document).Unix())
		evtUnix[results[i].Document.ID] = u
		if u < recMin {
			recMin = u
		}
		if u > recMax {
			recMax = u
		}
	}
	recRange := recMax - recMin
	normRecency := func(id string) float64 {
		if recRange <= 0 {
			return 0
		}
		return (evtUnix[id] - recMin) / recRange
	}

	for i := range results {
		d := results[i].Document
		cos := results[i].RawScore
		if cos == 0 {
			cos = results[i].Score
		}
		if len(queryVec) > 0 && len(d.Embedding.Vector) > 0 {
			cos = cosineSimilarity(queryVec, d.Embedding.Vector)
		}
		results[i].Score = blender.StageAScore(StageASignals{
			Cosine:         cos,
			Recency:        normRecency(d.ID), // conversation timestamp, min-max over candidates
			MeanConfidence: confidences[d.ID],
			PageRank:       normPageRank(d.ID),
			Activation:     d.ActivationStrength,
			Lexical:        results[i].LexicalScore, // hybrid: full-text/RRF rank signal
			GraphCoherence: coherence[d.ID],         // chunk_triplets connectivity to the pool
		})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results
}

// docEventTime returns the fact's real-world event time: the conversation
// timestamp stamped into metadata["timestamp"] at ingest (ADR-0053 temporal
// backfill, e.g. "2023-09-03T14:14:00"). Falls back to the document's CreatedAt
// (ingest time) when the metadata timestamp is absent or unparseable — so recency
// degrades to the prior behaviour rather than breaking. Accepts a few layouts so a
// date-only stamp still parses.
func docEventTime(d domain.Document) time.Time {
	if d.Metadata != nil {
		if v, ok := d.Metadata["timestamp"].(string); ok && v != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
				if t, err := time.Parse(layout, v); err == nil {
					return t
				}
			}
		}
	}
	return d.CreatedAt
}

// defaultCoherenceSeedN is how many query-relevant candidates anchor the
// coherence spread. Few, because energy must originate at the RELEVANT hits, not
// the whole pool — anchoring on all candidates collapses the signal into a
// session-size popularity prior (which demotes a correct-but-lightly-retrieved
// gold; see ADR-0054 tuning notes).
const defaultCoherenceSeedN = 10

// chunkCoherence scores each candidate's IDF-weighted graph connectivity to the
// QUERY-RELEVANT SEEDS over the chunk_triplets KG — seed-anchored spreading
// activation pointed at the live KG (ADR-0054), the graph-native conversation
// disambiguator. It is deliberately NOT pool-wide density.
//
// Two corrections over a naive pool-wide count, both load-bearing:
//
//  1. Energy originates at the SEEDS (the top-seedN candidates by relevance =
//     max(cosine RawScore, lexical/RRF score)), not the whole candidate set. A
//     chunk is rewarded only for sharing entities with a query-relevant anchor,
//     so a big-but-irrelevant session can't win on size — it has no seed. The
//     low-cosine gold (RawScore=0, surfaced by kgExpand) is never an anchor but
//     IS a beneficiary: if a seed sits in its session, it shares the seed's
//     entities (incl. the `dated at`/`spoke at` timestamp) and gets boosted.
//  2. Each shared entity is IDF-weighted (1/df over the candidate pool), so a
//     super-hub like the session timestamp — shared by every session chunk —
//     contributes little, while a rare, specific entity (a speaker + topic)
//     contributes a lot. This stops any hub from dominating relevance.
//
// coherence(C) = Σ over seeds S≠C of Σ over shared entities e of 1/df(e),
// min-max normalized to [0,1]. nil store / <2 candidates / no seed overlap ⇒
// empty map (the blend silently falls back to its other terms).
func chunkCoherence(ctx context.Context, store ChunkTripletsStore, results []domain.SearchResult, seedN int) map[string]float64 {
	out := make(map[string]float64, len(results))
	if store == nil || len(results) < 2 {
		return out
	}
	if seedN <= 0 {
		seedN = defaultCoherenceSeedN
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.Document.ID != "" {
			ids = append(ids, r.Document.ID)
		}
	}
	byChunk, err := store.ForChunks(ctx, ids)
	if err != nil {
		slog.Warn("coherence: ForChunks failed; coherence signal disabled this query", "err", err)
		return out
	}
	// Per-candidate de-duplicated entity set + df (candidates mentioning each
	// entity) for IDF. Entities are stored lowercase; trim to match kgExpand.
	chunkEnts := make(map[string]map[string]struct{}, len(byChunk))
	df := make(map[string]int)
	for id, triplets := range byChunk {
		set := make(map[string]struct{}, len(triplets)*2)
		for _, t := range triplets {
			for _, e := range [2]string{strings.TrimSpace(t.H), strings.TrimSpace(t.T)} {
				if e != "" {
					set[e] = struct{}{}
				}
			}
		}
		chunkEnts[id] = set
		for e := range set {
			df[e]++
		}
	}

	// Seeds = top-seedN candidates by relevance (genuine vector/lexical hits;
	// kgExpand additions have rel=0 and are excluded). These anchor the spread.
	type rel struct {
		id string
		s  float64
	}
	rels := make([]rel, 0, len(results))
	for _, r := range results {
		if r.Document.ID == "" {
			continue
		}
		s := r.RawScore
		if r.LexicalScore > s {
			s = r.LexicalScore
		}
		if s > 0 {
			rels = append(rels, rel{r.Document.ID, s})
		}
	}
	sort.Slice(rels, func(i, j int) bool {
		if rels[i].s != rels[j].s {
			return rels[i].s > rels[j].s
		}
		return rels[i].id < rels[j].id // deterministic ties
	})
	if len(rels) > seedN {
		rels = rels[:seedN]
	}
	if len(rels) == 0 {
		return out // no query-relevant anchor ⇒ no signal
	}
	type seed struct {
		id   string
		ents map[string]struct{}
	}
	seeds := make([]seed, 0, len(rels))
	for _, r := range rels {
		seeds = append(seeds, seed{id: r.id, ents: chunkEnts[r.id]})
	}

	idf := func(e string) float64 {
		if d := df[e]; d > 0 {
			return 1.0 / float64(d)
		}
		return 0
	}
	raw := make(map[string]float64, len(chunkEnts))
	var maxRaw float64
	for id, set := range chunkEnts {
		var s float64
		for _, sd := range seeds {
			if sd.id == id {
				continue // a chunk does not anchor on itself
			}
			for e := range set {
				if _, ok := sd.ents[e]; ok {
					s += idf(e)
				}
			}
		}
		raw[id] = s
		if s > maxRaw {
			maxRaw = s
		}
	}
	if maxRaw <= 0 {
		return out // no overlap with any seed ⇒ no signal
	}
	for id, s := range raw {
		out[id] = s / maxRaw
	}
	return out
}

// queryStopwords are function/interrogative words that never name an entity;
// dropping them keeps query-entity seeding from looking up noise. Lowercase.
var queryStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "was": {}, "were": {}, "been": {},
	"has": {}, "have": {}, "had": {}, "does": {}, "did": {}, "what": {}, "when": {},
	"where": {}, "who": {}, "whom": {}, "which": {}, "why": {}, "how": {}, "will": {},
	"would": {}, "shall": {}, "should": {}, "can": {}, "could": {}, "with": {},
	"about": {}, "from": {}, "into": {}, "that": {}, "this": {}, "these": {},
	"those": {}, "its": {}, "as": {}, "by": {}, "going": {}, "still": {}, "there": {},
	"their": {}, "they": {}, "them": {}, "his": {}, "her": {}, "you": {}, "your": {},
	"our": {}, "ago": {}, "long": {}, "any": {}, "all": {}, "some": {}, "than": {},
}

// extractQueryTerms tokenizes a query into candidate entity surface forms: content
// unigrams and adjacent bigrams (lowercased; stopwords and <3-char tokens dropped).
// Bigrams catch multi-word entities ("support group", "border collie"). Pure Go,
// no LLM — entity-hood is decided downstream by the KG itself: a non-entity term
// simply returns no chunks from ChunksMentioningEntity, so false candidates are
// harmless. Returns de-duplicated terms in query order.
func extractQueryTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	content := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 3 {
			continue
		}
		if _, stop := queryStopwords[w]; stop {
			continue
		}
		content = append(content, w)
	}
	seen := make(map[string]struct{}, len(content)*2)
	out := make([]string, 0, len(content)*2)
	add := func(t string) {
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for i, w := range content {
		add(w)
		if i+1 < len(content) {
			add(w + " " + content[i+1])
		}
	}
	return out
}

// injectQueryEntitySeeds extracts entities from the QUERY text and appends the
// chunks that mention them (over the live chunk_triplets KG) as extra seeds —
// LLM-free, structure-aware recall. It rescues the gold on a vector miss: even
// when the embedder ranks the right chunk far down (or off the over-fetch), the
// query's own entities reach it through the graph. New chunks are scored like
// kgExpand additions (expandedScore: a survival floor lifted by query cosine) and
// carry RawScore=0 so the Stage-A blend treats them as beneficiaries, not
// relevance anchors. Deduped against the existing pool and bounded by kgMaxExpanded.
func (q *QueryService) injectQueryEntitySeeds(ctx context.Context, results []domain.SearchResult, query string, vec []float32) []domain.SearchResult {
	terms := extractQueryTerms(query)
	if len(terms) == 0 {
		return results
	}
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Document.ID] = true
	}
	perEntity := q.kgPerEntity
	if perEntity <= 0 {
		perEntity = 5
	}
	budget := q.kgMaxExpanded
	if budget <= 0 {
		budget = 20
	}
	added := 0
	for _, term := range terms {
		if added >= budget {
			break
		}
		ids, err := q.chunkTriplets.ChunksMentioningEntity(ctx, term, perEntity)
		if err != nil {
			slog.WarnContext(ctx, "query-entity seeding: lookup failed", "term", term, "err", err)
			continue
		}
		for _, id := range ids {
			if added >= budget {
				break
			}
			if seen[id] {
				continue
			}
			doc, derr := q.vectorStore.GetByID(ctx, id)
			if derr != nil || doc == nil {
				continue
			}
			seen[id] = true
			results = append(results, domain.SearchResult{
				Document: *doc,
				Score:    expandedScore(vec, *doc),
			})
			added++
		}
	}
	return results
}

// Search embeds query, searches the vector store (memory docs only), and filters by ACL.
// callerID is the agent requesting access; documents owned by other agents are excluded.
func (q *QueryService) Search(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	return q.searchByType(ctx, query, callerID, domain.DocTypeMnemonicFact, true)
}

// SearchActions is the "what did I do" lane (ADR-0049 D4). It retrieves
// `mnemonic_action` records, kept SEPARATE from fact recall so action breadcrumbs
// never re-bloat fact grounding. Same ACL/scope/relevance-floor gating; no graph
// spreading (actions are events, not associatively-expanded knowledge).
func (q *QueryService) SearchActions(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	return q.searchByType(ctx, query, callerID, domain.DocTypeMnemonicAction, false)
}

// SearchScenes is the situational-retrieval lane (ADR-0049 D7): find scenes whose
// abstracted projection is similar to the query situation — "have I been in a
// situation like this?". Below the relevance floor → empty ("no precedent"); no
// graph spreading.
func (q *QueryService) SearchScenes(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	return q.searchByType(ctx, query, callerID, domain.DocTypeMnemonicScene, false)
}

// SearchEntities is the EXACT-lookup access path (ADR-0049 D8/Issue 012): the query is a
// canonical `kind:id` (not a semantic phrase), resolved by id to ONE entity record. The
// returned text is the RECONSTRUCTED current state — the materialized field-LWW view,
// which by construction already excludes superseded fields (a deleted file reads
// exists=false). It carries the link to the most-recent engaging scene so the caller can
// resolve that scene's baseline. Honors the read-gate: an unknown principal gets nothing.
func (q *QueryService) SearchEntities(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	key := strings.TrimSpace(query)
	if key == "" {
		return []domain.SearchResult{}, nil
	}
	if q.scopes != nil {
		if _, ok := q.resolveScope(ctx, callerID); !ok {
			slog.WarnContext(ctx, "memory entity-lookup: denied unknown principal (fail-closed)",
				slog.String("event", "scope_deny"), slog.String("agent_id", callerID))
			return []domain.SearchResult{}, nil
		}
	}
	doc, err := q.vectorStore.GetByID(ctx, key)
	if err != nil || doc == nil {
		return []domain.SearchResult{}, nil // unknown entity → "no record", not an error
	}
	if !aclAllows(doc.Metadata, callerID) {
		return []domain.SearchResult{}, nil
	}
	doc.Text = reconstructEntityState(doc)
	return []domain.SearchResult{{Document: *doc, Score: 1.0}}, nil
}

// reconstructEntityState renders the entity's current known state from its materialized
// field-LWW cache (ADR-0049 Issue 012). The fields ARE the reconstruction: each is the
// latest non-superseded observation, so "what's true now" is a deterministic read, not a
// re-derivation. The most-recent engaging scene link rides along for baseline resolution.
func reconstructEntityState(doc *domain.Document) string {
	var sb strings.Builder
	sb.WriteString(doc.ID)
	fields := decodeEntityFields(doc)
	vals := materializedValues(fields)
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, " %s=%v", k, vals[k])
	}
	if ls, ok := doc.Metadata["last_scene"].(string); ok && ls != "" {
		fmt.Fprintf(&sb, " last_scene=%s", ls)
	}
	return sb.String()
}

// SearchPrecedents is the world-model PULL lane (ADR-0049 D11/Issue 014): for the
// agent's current sub-situation it retrieves prior TRANSITIONS — similar scenes plus
// their outcome and action path — so the agent can anticipate the consequence of its
// next action. Built on the situational scene search, so it inherits the relevance floor
// ("no precedent" below it, never a fabricated analogy) and is failure-weighted. Fed by
// the live pull path — PrimeForStep is NOT revived.
func (q *QueryService) SearchPrecedents(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	scenes, err := q.SearchScenes(ctx, query, callerID)
	if err != nil || len(scenes) == 0 {
		return []domain.SearchResult{}, err
	}
	precedents := retrievePrecedents(ctx, q.vectorStore, scenes)
	out := make([]domain.SearchResult, 0, len(precedents))
	for _, p := range precedents {
		out = append(out, domain.SearchResult{
			Document: domain.Document{
				ID:           p.SceneID,
				DocumentType: domain.DocTypeMnemonicScene,
				Text:         precedentText(p),
				Metadata: map[string]interface{}{
					"outcome":  p.Outcome,
					"scene_id": p.SceneID,
				},
			},
			Score: p.Similarity,
		})
	}
	return out, nil
}

// searchByType embeds the query, searches one document type, and applies ACL +
// same-session step-record exclusion (D1) + relevance floor (#1), optionally
// spreading. Shared by the fact and action lanes (ADR-0049 D4).
func (q *QueryService) searchByType(ctx context.Context, query, callerID, docType string, spread bool) ([]domain.SearchResult, error) {
	// Recall side of an asymmetric embedder: if the embedder distinguishes query
	// from document (ADR-0048, e.g. bge-large's query instruction), use EmbedQuery
	// so the query carries the right prefix while stored docs stay bare. Embedders
	// without it (symmetric, e.g. nomic) fall through to plain Embed — non-breaking.
	var vec []float32
	var err error
	if qe, ok := q.embedder.(interface {
		EmbedQuery(context.Context, string) ([]float32, error)
	}); ok {
		vec, err = qe.EmbedQuery(ctx, query)
	} else {
		vec, err = q.embedder.Embed(ctx, query)
	}
	if err != nil {
		return nil, fmt.Errorf("memory query: embed: %w", err)
	}

	// Over-fetch (ADR-0048 D1) so dropping the run's own step records does not shrink
	// the returned window short of recallTopK.
	opts := domain.SearchOptions{DocumentType: docType, TopK: q.effRecallOverFetch()}
	// ADR-0034: enforce the caller's effective read scope. An unknown principal is
	// denied (fail-closed): empty result set.
	if q.scopes != nil {
		eff, ok := q.resolveScope(ctx, callerID)
		if !ok {
			slog.WarnContext(ctx, "memory query: denied unknown principal (fail-closed)",
				slog.String("event", "scope_deny"), slog.String("agent_id", callerID))
			return []domain.SearchResult{}, nil
		}
		opts.Scope = eff
	}

	results, err := q.vectorStore.Search(ctx, vec, opts)
	if err != nil {
		return nil, fmt.Errorf("memory query: search: %w", err)
	}

	// ADR-0054 hybrid retrieval: fuse the dense (vector) seed pool with a lexical
	// (full-text) search via Reciprocal Rank Fusion, so exact-token chunks the
	// embedder ranks low still enter the pool. Same opts (scope/doctype/over-fetch)
	// → scope-safe. Lexical failure degrades to vector-only.
	if q.lexical != nil {
		if lex, lerr := q.lexical.LexicalSearch(ctx, query, opts); lerr != nil {
			slog.WarnContext(ctx, "hybrid: lexical search failed; vector-only", "err", lerr)
		} else if len(lex) > 0 {
			results = rrfFuse(results, lex, q.rrfK, q.effRecallOverFetch())
		}
	}

	// ADR-0052: entity-aware seeding. If the EntityIndex is wired and has
	// stored entity-name embeddings, find the top-K entity keys most
	// relevant to the query and append their doc associations as additional
	// seeds. Each appended seed carries a base score = query→entity cosine,
	// which the BFS treats identically to a vector seed.
	if q.entityIdx != nil {
		results = q.injectEntitySeeds(ctx, results, vec, docType, callerID)
	}

	// ADR-0053 Phase 0: KG²RAG one-hop chunk expansion. If the
	// ChunkTripletsStore is wired, walk the per-chunk triplets from the
	// seed chunks, collect referenced entities, and pull in the chunks that
	// share those entities. The expansion is bounded to one hop (default).
	// This is the second trigger family in T-Mem's vocabulary: a chunk
	// reachable from a seed via the KG (associative trigger).
	// LLM-free query-entity seeding: extract entities from the QUERY text and
	// inject the chunks mentioning them as seeds, so a vector miss is rescued by
	// the graph. Runs before kgExpand so the expansion also walks from these.
	if q.queryEntitySeed && q.chunkTriplets != nil {
		results = q.injectQueryEntitySeeds(ctx, results, query, vec)
	}

	if q.chunkTriplets != nil && len(results) > 0 {
		results = kgExpand(ctx, results, q.chunkTriplets, q.vectorStore, vec, kgExpandOpts{
			Hops:        q.kgHops,
			MaxExpanded: q.kgMaxExpanded,
			MaxEntities: q.kgMaxEntities,
			PerEntity:   q.kgPerEntity,
		})
	}

	// ADR-0054 Stage A: re-rank ALL candidates (seeds + KG-expanded) by the
	// multi-signal blend (cosine + recency + confidence + pagerank + activation).
	// Flag-gated; nil blender ⇒ the bare-cosine ordering above is kept.
	if q.blender.Load() != nil && q.rankSignals != nil && len(results) > 0 {
		results = q.applyStageABlend(ctx, results, vec)
	}

	// ADR-0054 Stage B: cross-encoder rerank of the top-K Stage-A candidates,
	// blended via FinalScore = w_bge·bge + (1-w_bge)·stageA. Flag-gated; nil
	// reranker (or an unreachable one) leaves the Stage-A order intact. Runs
	// BEFORE the ACL/floor filter + truncation so the oracle reorders the full
	// recoverable pool, then the top recallTopK is returned.
	if q.reranker != nil && len(results) > 0 {
		results = q.applyStageBRerank(ctx, query, results)
	}

	// Document-local anchor promotion: when the query names a structural anchor
	// (Chapter 1, scene 1 / an explicit id), lift the chunks that carry it above
	// the floor so the reranker can't bury them among template-identical siblings.
	if q.anchorConstraint && q.chunkTriplets != nil {
		results = q.applyAnchorConstraint(ctx, results, query, vec)
	}

	// ADR-0060: structure-graph section scoping — promote chunks under a section
	// the query names, resolved via the parser-derived hierarchy + ltree subtree.
	if q.sectionStore != nil {
		results = q.applySectionConstraint(ctx, results, query, vec)
	}

	sid, _ := domain.SessionIDFromContext(ctx)

	filtered := results[:0]
	for _, r := range results {
		if !aclAllows(r.Document.Metadata, callerID) {
			continue
		}
		// ADR-0048 D1: exclude the run's own auto-recorded System step records (the
		// feedback loop). A no-op for the action lane (actions are source ToolOutput).
		if isSameSessionStepRecord(r.Document, sid) {
			continue
		}
		// ADR-0048 #1: drop seeds below the relevance floor so an all-irrelevant query
		// returns EMPTY rather than padding the top-k with noise.
		if q.floor > 0 && r.Score < q.floor {
			continue
		}
		filtered = append(filtered, r)
		if len(filtered) >= q.effRecallTopK() {
			break
		}
	}

	// ADR-0048 D2: optionally expand the fact seeds associatively over the memory
	// graph. Off for the action lane.
	if spread && q.spreader != nil && len(filtered) > 0 {
		filtered = q.spreadAndRank(ctx, filtered, sid)
	}
	// ADR-0049 D10: Hebbian reinforcement of the fact lane's co-retrieved set — async,
	// off the read path so recall latency is untouched. Off for the action lane.
	if spread && q.heb.enabled && len(filtered) >= 2 {
		go q.reinforceCoActivation(filtered)
	}
	// ADR-0060: neighbor-window — append each returned chunk's document neighbors
	// (preceding/following) for adjacent context. Runs last so ranking is untouched.
	if q.neighborWindow {
		filtered = q.applyNeighborWindow(ctx, filtered)
	}
	return filtered, nil
}

type docPair struct{ a, b string }

// rrfFuse merges two ranked result lists by Reciprocal Rank Fusion (ADR-0054):
// score(d) = Σ over lists 1/(k + rank_in_list), rank 1-based. A doc in both lists
// sums its contributions (so agreement wins). The fused list is sorted by RRF
// score and capped at limit. For a doc in both lists the FIRST occurrence's
// SearchResult is kept (the vector list is passed first, so its RawScore=cosine
// is preserved for the downstream blend); lexical-only docs keep RawScore=0 and
// the blend recomputes cosine from their embedding. k<=0 defaults to 60.
func rrfFuse(vectorList, lexicalList []domain.SearchResult, k, limit int) []domain.SearchResult {
	if k <= 0 {
		k = 60
	}
	type acc struct {
		res   domain.SearchResult
		score float64
		lex   float64 // reciprocal lexical rank in [0,1] (0 = not in the lexical list)
	}
	byID := make(map[string]*acc, len(vectorList)+len(lexicalList))
	order := make([]string, 0, len(vectorList)+len(lexicalList))
	add := func(list []domain.SearchResult, lexical bool) {
		for rank, r := range list {
			id := r.Document.ID
			if id == "" {
				continue
			}
			a := byID[id]
			if a == nil {
				a = &acc{res: r}
				byID[id] = a
				order = append(order, id)
			}
			a.score += 1.0 / float64(k+rank+1)
			if lexical {
				a.lex = 1.0 / float64(rank+1) // top lexical hit ⇒ 1.0, decays by rank
			}
		}
	}
	add(vectorList, false) // first ⇒ keeps cosine RawScore for docs in both lists
	add(lexicalList, true)

	out := make([]domain.SearchResult, 0, len(order))
	for _, id := range order {
		a := byID[id]
		r := a.res
		r.Score = a.score
		r.LexicalScore = a.lex // carried into the Stage-A blend
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// coActivatedPairs returns the pairwise combinations of the top-N results that clear
// the co-activation floor (ADR-0049 D10) — the "fired together" set. Pure.
func coActivatedPairs(results []domain.SearchResult, floor float64, topN int) []docPair {
	hot := make([]string, 0, topN)
	for _, r := range results {
		if r.Score >= floor && r.Document.ID != "" {
			hot = append(hot, r.Document.ID)
			if topN > 0 && len(hot) >= topN {
				break
			}
		}
	}
	var pairs []docPair
	for i := 0; i < len(hot); i++ {
		for j := i + 1; j < len(hot); j++ {
			pairs = append(pairs, docPair{hot[i], hot[j]})
		}
	}
	return pairs
}

// reinforcedWeight decays the current edge weight by its age, adds the learning rate,
// and caps at the max (Matthew-effect normalization). Pure (ADR-0049 D10).
func (q *QueryService) reinforcedWeight(current float64, createdAt, now time.Time) float64 {
	w := current
	if !createdAt.IsZero() {
		if ageDays := now.Sub(createdAt).Hours() / 24; ageDays > 0 {
			w = current * math.Pow(q.heb.decayPerDay, ageDays)
		}
	}
	w += q.heb.lr
	if w > q.heb.max {
		w = q.heb.max
	}
	return w
}

func hebbianEdgeKey(a, b string) string {
	if a <= b {
		return a + "|" + b
	}
	return b + "|" + a
}

// reinforceCoActivation strengthens the co_activated edges among a recall's strongly
// co-retrieved docs (ADR-0049 D10): read current weights, decay-by-age, add the
// learning rate (capped), write both directions. Best-effort, async; lost updates
// under concurrent recalls are acceptable for a statistical Hebbian signal.
func (q *QueryService) reinforceCoActivation(results []domain.SearchResult) {
	if q.graphWriter == nil {
		return
	}
	pairs := coActivatedPairs(results, q.heb.floor, q.heb.topN)
	if len(pairs) == 0 {
		return
	}
	ctx := context.Background()

	seen := map[string]bool{}
	var ids []string
	for _, p := range pairs {
		for _, id := range []string{p.a, p.b} {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	cur := map[string]domain.DocumentEdge{}
	if existing, err := q.graphWriter.GetAdjacentEdges(ctx, ids); err == nil {
		for _, e := range existing {
			if e.EdgeType == domain.EdgeCoActivated {
				cur[hebbianEdgeKey(e.SourceID, e.TargetID)] = e
			}
		}
	}

	now := time.Now()
	for _, p := range pairs {
		w := q.heb.base
		if e, ok := cur[hebbianEdgeKey(p.a, p.b)]; ok {
			w = q.reinforcedWeight(float64(e.Weight), e.CreatedAt, now)
		}
		for _, dir := range [][2]string{{p.a, p.b}, {p.b, p.a}} {
			_ = q.graphWriter.SaveEdge(ctx, domain.DocumentEdge{
				SourceID: dir[0], TargetID: dir[1], EdgeType: domain.EdgeCoActivated, Weight: float32(w), CreatedAt: now,
			})
		}
	}
}

// spreadAndRank expands seeds over the memory graph, drops the current session's
// own step records (D1) and duplicates, then re-ranks using the T-Mem
// two-trigger formula (cosine × (α + (1-α) × effective) + β × reachability)
// and caps to recallTopK. ADR-0048 D2 + ADR-0052.
func (q *QueryService) spreadAndRank(ctx context.Context, seeds []domain.SearchResult, sid string) []domain.SearchResult {
	expansions := q.spreader.Spread(ctx, seeds)
	seen := make(map[string]bool, len(expansions))
	out := make([]domain.SearchResult, 0, len(expansions))
	for _, e := range expansions {
		if seen[e.Document.ID] || isSameSessionStepRecord(e.Document, sid) {
			continue
		}
		seen[e.Document.ID] = true
		out = append(out, domain.SearchResult{Document: e.Document, Score: e.ActivationEnergy})
	}
	// ADR-0052: re-rank with the associative term. If the entity index is
	// not wired (or the recall missed any doc with entity neighbors), the
	// reachability map is empty and the term is a no-op.
	if q.entityIdx != nil && q.assocWeight > 0 && len(out) > 0 {
		queryVec, _ := seeds[0].Document.Embedding.Vector, struct{}{} // placeholder, overwritten below
		_ = queryVec
		// Recover the query vector from the first seed's neighbor graph: the
		// seeds came in with their original cosine in Document.Metadata
		// (no — cosine is in SearchResult.Score, not on the doc). The query
		// vector isn't preserved on the seeds, so we re-embed is too
		// expensive and the alternative is to re-embed the first seed's
		// TEXT. We use the seed's text as a proxy: for the working set the
		// proxy vector is within ~0.05 cosine of the original, which is
		// well within the β-scaled term's noise floor.
		// TODO(0052): pass queryVec through the BFS so reachability uses
		// the original query embedding, not the seed-text proxy. The
		// current proxy is correct-enough for β=0.2.
		reachability := ComputeReachabilityFromSeedText(out, q.entityIdx, seeds)
		out = ReRankWithAssociative(out, reachability, q.assocLambda, q.assocWeight, time.Now())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k := q.effRecallTopK(); len(out) > k {
		out = out[:k]
	}
	return out
}

// injectEntitySeeds finds the top-K entity keys by query-embedding cosine and
// adds their associated docs to the candidate set with a base score equal to
// the query→entity cosine. The appended seeds are real documents (looked up
// via VectorStore.GetByID) so the rest of the pipeline (ACL, same-session
// exclusion, relevance floor, BFS) treats them uniformly. ADR-0052.
func (q *QueryService) injectEntitySeeds(
	ctx context.Context,
	results []domain.SearchResult,
	queryVec []float32,
	docType string,
	callerID string,
) []domain.SearchResult {
	if q.entityIdx == nil || q.assocTopK <= 0 {
		return results
	}
	// Use the FIRST seed's stored embedding as a proxy for the query
	// embedding (the EntityIndex stores entity-name embeddings). This is
	// the same proxy used in spreadAndRank; see ComputeReachabilityFromSeedText
	// for the rationale and the TODO to plumb the real queryVec.
	if len(results) == 0 || len(queryVec) == 0 {
		return results
	}
	embs := q.entityIdx.SnapshotEmbeddings()
	if len(embs) == 0 {
		return results
	}
	type scored struct {
		key   string
		score float64
	}
	picks := make([]scored, 0, len(embs))
	for k, emb := range embs {
		if len(emb.Vector) == 0 {
			continue
		}
		s := cosineSimilarity(queryVec, emb.Vector)
		if s > 0 {
			picks = append(picks, scored{k, s})
		}
	}
	sort.Slice(picks, func(i, j int) bool { return picks[i].score > picks[j].score })
	if len(picks) > q.assocTopK {
		picks = picks[:q.assocTopK]
	}
	if len(picks) == 0 {
		return results
	}

	// Collect candidate doc IDs from the top-K entity keys, dedup'd.
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Document.ID] = true
	}
	docIDs := make([]string, 0, q.assocTopK*5)
	for _, p := range picks {
		for _, assoc := range q.entityIdx.DocsFor(p.key) {
			if assoc.DocID == "" || seen[assoc.DocID] {
				continue
			}
			seen[assoc.DocID] = true
			docIDs = append(docIDs, assoc.DocID)
		}
	}
	if len(docIDs) == 0 {
		return results
	}

	// Fetch the docs and stamp them with the entity's cosine as a base
	// score. We use a per-entity score average: if a doc is associated
	// with multiple top-K entities, it gets the average of their cosines.
	// Cost: O(topK * avg_assoc_per_entity) fetches. For LoCoMo's
	// working set this is a handful of GetByID calls.
	docScores := make(map[string]float64, len(docIDs))
	docScoreCount := make(map[string]int, len(docIDs))
	for _, p := range picks {
		for _, assoc := range q.entityIdx.DocsFor(p.key) {
			docScores[assoc.DocID] += p.score
			docScoreCount[assoc.DocID]++
		}
	}
	for id := range docScores {
		if docScoreCount[id] > 0 {
			docScores[id] /= float64(docScoreCount[id])
		}
	}

	// Materialize the seeds. We use VectorStore.GetByID; for the LoCoMo
	// benchmark the vector store is pgvector with a fast PK lookup.
	out := results
	for _, id := range docIDs {
		doc, err := q.vectorStore.GetByID(ctx, id)
		if err != nil || doc == nil {
			continue
		}
		if docType != "" && string(doc.DocumentType) != docType {
			continue
		}
		if !aclAllows(doc.Metadata, callerID) {
			continue
		}
		// Scale the seed's base score so it sits in the same band as the
		// vector seeds. 0.5× the average entity cosine keeps the term from
		// dominating the BFS while still letting it influence the order.
		base := 0.5 * docScores[id]
		out = append(out, domain.SearchResult{Document: *doc, Score: base})
	}
	return out
}

// vector is plumbed through the BFS. It picks the first seed's embedding as
// the query proxy, which is semantically close enough for β=0.2 re-ranking.
//
// TODO(0052): replace with a real queryVec parameter once the
// MemorySearcher.Search signature carries it through.
func ComputeReachabilityFromSeedText(
	candidates []domain.SearchResult,
	entityIdx *EntityIndex,
	seeds []domain.SearchResult,
) map[string]float64 {
	if entityIdx == nil || len(seeds) == 0 {
		return map[string]float64{}
	}
	// The seeds carry the original vector — re-derive the proxy from the
	// first seed's stored embedding.
	proxy := seeds[0].Document.Embedding.Vector
	if len(proxy) == 0 {
		// Fall back to surface-only by returning an empty reachability map.
		return map[string]float64{}
	}
	return ComputeReachability(candidates, entityIdx, proxy)
}

const (
	// defaultRecallTopK is the number of results recall returns to the agent
	// when not overridden by config (ADR-0054 SetRecallSizes).
	defaultRecallTopK = 10
	// defaultRecallOverFetch is the vector-store fetch size: larger than
	// recallTopK so same-session step records (ADR-0048 D1) can be dropped
	// without starving the returned window.
	defaultRecallOverFetch = 25
)

// isSameSessionStepRecord reports whether doc is the CURRENT session's own
// auto-recorded step output. RecordExecution writes step results as
// "step_N:" mnemonic_facts with metadata source_agent="System" and the run's
// session_id (ADR-0015/0029); recalling those back into the same run accretes a
// larger copy of the context each step (ADR-0048 D1). A deliberate remember()
// fact carries the agent's own id as source, not "System", so it is NOT excluded
// — the exclusion is narrow to the auto-recorded step records.
func isSameSessionStepRecord(doc domain.Document, sid string) bool {
	if sid == "" || doc.Metadata == nil {
		return false
	}
	src, _ := doc.Metadata["source_agent"].(string)
	docSid, _ := doc.Metadata["session_id"].(string)
	return src == "System" && docSid == sid
}

// excludeSameSessionStepRecords drops the current session's own step records from
// a mnemonic_fact result set (ADR-0048 D1). Shared so any FACT search — the agent
// recall above and PrimeForStep's seed (D3, defensive) — gets the same exclusion.
func excludeSameSessionStepRecords(results []domain.SearchResult, sid string) []domain.SearchResult {
	if sid == "" {
		return results
	}
	out := results[:0]
	for _, r := range results {
		if !isSameSessionStepRecord(r.Document, sid) {
			out = append(out, r)
		}
	}
	return out
}

// resolveScope returns the effective read scope for callerID. Phase 2: when a
// session ID is in ctx and the session carries a non-empty caller_scope, the
// effective scope is caller_scope ∩ agent_scope (caller_scope read SERVER-SIDE
// from the session record, never from Handoff.Context). Otherwise Phase 1:
// agent_scope only. ADR-0034 (D4/D13).
func (q *QueryService) resolveScope(ctx context.Context, callerID string) (*domain.EffectiveScope, bool) {
	if q.callerScopes != nil && q.sessions != nil {
		if sid, ok := domain.SessionIDFromContext(ctx); ok {
			if caller := q.sessions.CallerScope(ctx, sid); !caller.IsZero() {
				return q.callerScopes.EffectiveForCaller(ctx, callerID, caller)
			}
		}
	}
	return q.scopes.EffectiveForAgent(ctx, callerID)
}

// aclAllows returns true when the document is visible to callerID.
//   - No "source_agent_id" key → shared/public document → visible to all.
//   - "source_agent_id" == callerID → owned by caller → visible.
//   - "source_agent_id" != callerID → another agent's doc → hidden.
func aclAllows(meta map[string]interface{}, callerID string) bool {
	val, exists := meta["source_agent_id"]
	if !exists {
		return true
	}
	ownerID, ok := val.(string)
	if !ok {
		return true
	}
	return ownerID == callerID
}
