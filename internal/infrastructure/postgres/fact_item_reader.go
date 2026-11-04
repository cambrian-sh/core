package postgres

import (
	"context"
	"fmt"
	"sort"

	"github.com/pgvector/pgvector-go"

	"github.com/cambrian-sh/core/domain"
)

// PgFactItemReader is the substrate-backed read side of the ADR-0136 fact layer (A1.5):
// a top-k search over the item embedding projection (knowledge_item_embeddings, 0022)
// filtered IN SQL on knowledge_items.classification with the ADR-0118 D2 TEXT[]
// operators, then the derived chain of each matched key (the valid_time_supersession
// authority over items + stored assessments + predicate profile) so every hit says
// whether it is current and what superseded it.
//
// Filtering in SQL matters for an HNSW top-k: a Go post-filter (the query plane's
// `current` shape does one) runs after the LIMIT and returns too few rows for a reader
// whose scope admits a minority of the items.
//
// The kernel reaches it only through authz.EnforcingFactItemReader (fail closed when no
// predicate exists). A nil predicate here adds no SQL term, by design: the decision
// belongs to the decorator.
type PgFactItemReader struct {
	store     *PgKnowledgeStore
	namespace string
	modelID   string
	dim       int
}

var _ domain.FactItemReader = (*PgFactItemReader)(nil)

// NewPgFactItemReader reads memory_fact items of namespace through store's pool. modelID
// and dim select the projection rows and the per-dimension HNSW index
// (migrate.ItemEmbeddingIndexName — named, not imported: the adapter must not depend on
// the runner) the search is served by.
func NewPgFactItemReader(store *PgKnowledgeStore, namespace, modelID string, dim int) *PgFactItemReader {
	if namespace == "" {
		namespace = "default"
	}
	return &PgFactItemReader{store: store, namespace: namespace, modelID: modelID, dim: dim}
}

// vecExpr is the indexed expression. The literal dimension (an int from config, never
// caller text) and the literal `dims = <dim>` predicate are what let the planner match
// the partial expression index; a bound parameter would not imply its predicate.
func (r *PgFactItemReader) vecExpr() string { return fmt.Sprintf("e.embedding::vector(%d)", r.dim) }

func (r *PgFactItemReader) authority() (domain.InputResolutionAuthority, error) {
	_, a, err := r.store.policyFor(domain.KindMemoryFact)
	if err != nil {
		return nil, err
	}
	ia, ok := a.(domain.InputResolutionAuthority)
	if !ok {
		return nil, fmt.Errorf("fact reader: kind %q is not derived by an input-taking authority (is the memory_fact kind declared?)", domain.KindMemoryFact)
	}
	return ia, nil
}

// SearchFacts implements domain.FactItemReader.
func (r *PgFactItemReader) SearchFacts(ctx context.Context, vec []float32, opts domain.FactSearchOptions) ([]domain.FactHit, error) {
	if len(vec) == 0 || r.dim <= 0 || len(vec) != r.dim {
		return nil, nil
	}
	k := opts.TopK
	if k <= 0 {
		k = 20
	}
	ia, err := r.authority()
	if err != nil {
		return nil, err
	}
	var q string
	var args []any
	if len(opts.SourceDocumentIDs) > 0 {
		q, args = r.scopedSearchSQL(vec, k, opts)
	} else {
		scopeSQL, scopeArgs := itemScopeSQL(opts.Scope, "ki", 5)
		q = fmt.Sprintf(`SELECT ki.id, 1 - (%[1]s <=> $1::vector(%[2]d)) AS score
		FROM knowledge_item_embeddings e
		JOIN knowledge_items ki ON ki.id = e.item_id
		WHERE e.dims = %[2]d AND e.model_id = $2 AND ki.kind = $3 AND ki.namespace_id = $4%[3]s
		ORDER BY %[1]s <=> $1::vector(%[2]d)
		LIMIT $5`, r.vecExpr(), r.dim, scopeSQL)
		args = append([]any{pgvector.NewVector(vec), r.modelID, domain.KindMemoryFact, r.namespace, k}, scopeArgs...)
	}
	rows, err := r.store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fact search: %w", err)
	}
	type hit struct {
		id    domain.KnowledgeItemID
		score float64
	}
	var hits []hit
	for rows.Next() {
		var h hit
		var id string
		if err := rows.Scan(&id, &h.score); err != nil {
			rows.Close()
			return nil, err
		}
		h.id = domain.KnowledgeItemID(id)
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = string(h.id)
	}
	items, err := loadItems(ctx, r.store.pool, `WHERE ki.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := map[domain.KnowledgeItemID]domain.KnowledgeItem{}
	var keys []resolutionKey
	seen := map[resolutionKey]bool{}
	for _, it := range items {
		byID[it.ID] = it
		k := resolutionKey{it.EntityID, it.AssertedBy}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	derived, err := r.store.deriveKeys(ctx, r.store.pool, ia, r.namespace, domain.KindMemoryFact, keys, opts.Scope)
	if err != nil {
		return nil, err
	}
	views := mergeViews(derived)
	out := make([]domain.FactHit, 0, len(hits))
	for _, h := range hits {
		f, ok := views[h.id]
		if !ok {
			it, present := byID[h.id]
			if !present {
				continue
			}
			f = domain.FactFromItem(it)
		}
		fh := domain.FactHit{Fact: f, Score: h.score}
		if f.SupersededBy != "" {
			if s, ok := views[domain.KnowledgeItemID(f.SupersededBy)]; ok {
				fh.SupersededByValue = s.Value
			}
			fh.SupersededAt = f.ValidTo
		}
		out = append(out, fh)
	}
	return out, nil
}

// scopedSearchSQL is the ADR-0136 A4 document-scoped search: the top k facts whose
// source document (the FactPredSourceDocument value recorded at extraction) is one of
// opts.SourceDocumentIDs, under the SAME classification predicate as the global search.
//
// It is an EXACT cosine ranking, deliberately not served by the HNSW index: the set is
// the facts of a handful of documents, and an HNSW top-k post-filtered to them would
// return too few rows (the A1.5 reasoning for filtering in SQL at all). The candidates
// are found through idx_statement_values_source_document (migrate.go, a concurrent
// companion index), so the scan is over those documents' facts, not the namespace. The
// predicate is a literal (a kernel constant, never caller text) so that partial index's
// `WHERE predicate = 'source_document'` is implied. The ORDER BY is on the score, not on
// the `<=>` operator, so the planner cannot pick the approximate index. Ties break on the item id, so the result is deterministic.
func (r *PgFactItemReader) scopedSearchSQL(vec []float32, k int, opts domain.FactSearchOptions) (string, []any) {
	scopeSQL, scopeArgs := itemScopeSQL(opts.Scope, "ki", 6)
	q := fmt.Sprintf(`SELECT ki.id, 1 - (%[1]s <=> $1::vector(%[2]d)) AS score
		FROM statement_values sv
		JOIN knowledge_items ki ON ki.id = sv.item_id
		JOIN knowledge_item_embeddings e ON e.item_id = ki.id
		WHERE sv.predicate = '%[4]s' AND sv.value_text = ANY($6::text[])
		  AND e.dims = %[2]d AND e.model_id = $2 AND ki.kind = $3 AND ki.namespace_id = $4%[3]s
		ORDER BY score DESC, ki.id
		LIMIT $5`, r.vecExpr(), r.dim, scopeSQL, domain.FactPredSourceDocument)
	args := append([]any{pgvector.NewVector(vec), r.modelID, domain.KindMemoryFact, r.namespace, k, opts.SourceDocumentIDs}, scopeArgs...)
	return q, args
}

// chainViews renders a derivation as fact views keyed by item id: the key's own items
// with their derived intervals and the key's cardinality, plus the related items (the
// cross-key successors) as stated, so a superseded-by value can be named.
func chainViews(d derivedKey) map[domain.KnowledgeItemID]domain.Fact {
	out := map[domain.KnowledgeItemID]domain.Fact{}
	byID := map[domain.KnowledgeItemID]domain.KnowledgeItem{}
	for _, it := range d.items {
		byID[it.ID] = it
	}
	for _, r := range d.inputs.Related {
		out[r.ID] = domain.FactFromItem(r)
	}
	card := domain.FactCardinality(d.inputs.Cardinality)
	for _, iv := range d.outcome.Chain {
		it, ok := byID[iv.ItemID]
		if !ok || domain.IsFactErasedSentinel(it) {
			continue
		}
		f := domain.FactFromItem(it).ApplyInterval(iv)
		f.Cardinality = card
		out[iv.ItemID] = f
	}
	return out
}

// mergeViews merges several derivations' views. An item's view from its OWN key's
// derivation (with its derived interval) always wins over the stated copy another key
// carries as a related item, whatever the map order.
func mergeViews(derived map[resolutionKey]derivedKey) map[domain.KnowledgeItemID]domain.Fact {
	views := map[domain.KnowledgeItemID]domain.Fact{}
	own := map[domain.KnowledgeItemID]bool{}
	for _, d := range derived {
		for _, iv := range d.outcome.Chain {
			own[iv.ItemID] = true
		}
	}
	for _, d := range derived {
		for id, f := range chainViews(d) {
			if _, seen := views[id]; seen && !own[id] {
				continue
			}
			if own[id] && !isOwnView(d, id) {
				continue
			}
			views[id] = f
		}
	}
	return views
}

func isOwnView(d derivedKey, id domain.KnowledgeItemID) bool {
	for _, iv := range d.outcome.Chain {
		if iv.ItemID == id {
			return true
		}
	}
	return false
}

// orderedChain returns the key's facts in chain order (sentinels omitted).
func orderedChain(d derivedKey) []domain.Fact {
	views := chainViews(d)
	out := make([]domain.Fact, 0, len(d.outcome.Chain))
	for _, iv := range d.outcome.Chain {
		if f, ok := views[iv.ItemID]; ok {
			out = append(out, f)
		}
	}
	return out
}

// FactChain implements domain.FactItemReader.
func (r *PgFactItemReader) FactChain(ctx context.Context, f domain.Fact, scope *domain.TagPredicate) ([]domain.Fact, error) {
	if f.EntityID == "" {
		return nil, nil
	}
	ia, err := r.authority()
	if err != nil {
		return nil, err
	}
	k := resolutionKey{f.EntityID, f.Actor}
	derived, err := r.store.deriveKeys(ctx, r.store.pool, ia, r.namespace, domain.KindMemoryFact, []resolutionKey{k}, scope)
	if err != nil {
		return nil, err
	}
	d, ok := derived[k]
	if !ok {
		return nil, nil
	}
	return orderedChain(d), nil
}

// SimilarFacts implements domain.FactItemReader: same subject reference (scope key +
// subject key — so never another classification), another entity id (predicate),
// positive, nearest first. Read through the entity-typed `subject` value and its index
// (idx_statement_values_entity, 0022).
//
// Ties in distance are broken by item id (identical fact text embeds identically, so
// ties are ordinary), which makes the candidate set — and so the keyed assessment id —
// deterministic; the subject filter is selective, so the order is computed over a few
// rows.
func (r *PgFactItemReader) SimilarFacts(ctx context.Context, f domain.Fact, vec []float32, k int, scope *domain.TagPredicate) ([]domain.Fact, error) {
	return r.SimilarFactsHiding(ctx, f, vec, k, scope, nil)
}

var _ domain.FactStagedReader = (*PgFactItemReader)(nil)

// SimilarFactsHiding implements domain.FactStagedReader: SimilarFacts with the hidden
// items excluded from the ranking (SQL) and from every derivation (deriveKeysHiding),
// so the result is what SimilarFacts returned before those items were written.
func (r *PgFactItemReader) SimilarFactsHiding(ctx context.Context, f domain.Fact, vec []float32, k int, scope *domain.TagPredicate, hidden []domain.KnowledgeItemID) ([]domain.Fact, error) {
	if k <= 0 || len(vec) == 0 || len(vec) != r.dim || f.EntityID == "" {
		return nil, nil
	}
	ia, err := r.authority()
	if err != nil {
		return nil, err
	}
	hide := make(map[domain.KnowledgeItemID]bool, len(hidden))
	for _, id := range hidden {
		hide[id] = true
	}
	// k is an int, rendered as a literal so the scope placeholders can follow $7.
	scopeSQL, scopeArgs := itemScopeSQL(scope, "ki", 7)
	q := fmt.Sprintf(`SELECT ki.id FROM knowledge_items ki
		JOIN statement_values sv ON sv.item_id = ki.id AND sv.predicate = '%[4]s' AND sv.value_type = 'entity'
		JOIN knowledge_item_embeddings e ON e.item_id = ki.id AND e.dims = %[2]d AND e.model_id = $2
		WHERE sv.value_entity_id = $3 AND ki.kind = $4 AND ki.namespace_id = $5 AND ki.entity_id <> $6
		  AND NOT ki.negation AND NOT (ki.id = ANY($7::text[]))%[3]s
		ORDER BY %[1]s <=> $1::vector(%[2]d), ki.id
		LIMIT %[5]d`, r.vecExpr(), r.dim, scopeSQL, domain.FactPredSubject, k)
	args := append([]any{pgvector.NewVector(vec), r.modelID, domain.FactSubjectRef(f.ScopeKey, f.SubjectKey),
		domain.KindMemoryFact, r.namespace, f.EntityID, idStrings(hidden)}, scopeArgs...)
	rows, err := r.store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fact similar: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return nil, err
	}
	items, err := loadItems(ctx, r.store.pool, `WHERE ki.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var keys []resolutionKey
	seen := map[resolutionKey]bool{}
	for _, it := range items {
		k := resolutionKey{it.EntityID, it.AssertedBy}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	derived, err := r.store.deriveKeysHiding(ctx, r.store.pool, ia, r.namespace, domain.KindMemoryFact, keys, scope, hide)
	if err != nil {
		return nil, err
	}
	views := mergeViews(derived)
	rank := map[string]int{}
	for i, id := range ids {
		rank[id] = i
	}
	out := make([]domain.Fact, 0, len(ids))
	for _, id := range ids {
		if v, ok := views[domain.KnowledgeItemID(id)]; ok && !v.ValidEmpty {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].ID] < rank[out[j].ID] })
	return out, nil
}

// FactsByID implements domain.FactStagedReader: the items as stored (no derivation),
// under scope IN SQL, each with its projection embedding of this reader's model when
// one is stored. The two-phase backfill reloads a journaled window with it.
func (r *PgFactItemReader) FactsByID(ctx context.Context, ids []domain.KnowledgeItemID, scope *domain.TagPredicate) ([]domain.Fact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	scopeSQL, scopeArgs := itemScopeSQL(scope, "ki", 3)
	items, err := loadItems(ctx, r.store.pool, `WHERE ki.id = ANY($1) AND ki.kind = $2 AND ki.namespace_id = $3`+scopeSQL,
		append([]any{idStrings(ids), domain.KindMemoryFact, r.namespace}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("facts by id: %w", err)
	}
	out := make([]domain.Fact, 0, len(items))
	byID := map[string]int{}
	for _, it := range items {
		byID[string(it.ID)] = len(out)
		out = append(out, domain.FactFromItem(it))
	}
	if len(out) == 0 || r.dim <= 0 {
		return out, nil
	}
	rows, err := r.store.pool.Query(ctx, fmt.Sprintf(`SELECT item_id, %s FROM knowledge_item_embeddings e
		WHERE e.item_id = ANY($1) AND e.model_id = $2 AND e.dims = %d`, r.vecExpr(), r.dim), idStrings(ids), r.modelID)
	if err != nil {
		return nil, fmt.Errorf("facts by id: embeddings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v pgvector.Vector
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			out[i].Embedding = v.Slice()
		}
	}
	return out, rows.Err()
}
