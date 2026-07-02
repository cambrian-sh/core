package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// IndexedDoc is a single doc-to-entity association tracked by the in-memory
// reverse index. We keep the weight (LLM confidence) so the query path can
// rank entity neighbors by relevance, not just surface recency.
type IndexedDoc struct {
	DocID    string
	Weight   float64
	LastSeen int64 // unix nano; used for stale eviction
}

// EntityIndex is the in-memory reverse index from entity canonical key
// (e.g. "named:caroline") to the set of docs that mentioned it. It is the
// recall path's "first hop" — given a query, the awareness layer finds the
// top-3 entity keys by embedding similarity and the index returns the
// associative neighbors for those entities.
//
// The index is rebuilt from `document_edges` on boot (see Load) and updated
// synchronously by the EdgeWriter after each IngestMemory call. It is NOT a
// source of truth — `document_edges` is. A rebuild from the table restores
// it after a crash.
type EntityIndex struct {
	mu          sync.RWMutex
	entityDocs  map[string][]IndexedDoc    // key → docs
	entityName  map[string]string           // key → raw name (for embedding)
	entityMeta  map[string]EntityMetaKind   // key → meta-kind
	entityEmbed map[string]domain.Embedding // key → name embedding
	capPerEnt   int                         // max docs per entity (LRU eviction)
	capTotal    int                         // max total keys
	loadedAt    int64                       // unix nano
}

// RLock / RUnlock expose the read lock to the recall path's reachability
// computation, which walks many keys under a single lock acquisition.
func (i *EntityIndex) RLock()   { i.mu.RLock() }
func (i *EntityIndex) RUnlock() { i.mu.RUnlock() }

// SnapshotEmbeddings returns a copy of every (key, name embedding) pair.
// Callers should pre-filter by query cosine before iterating the index the
// other way (see ComputeReachability). Cost: O(n) under the read lock.
func (i *EntityIndex) SnapshotEmbeddings() map[string]domain.Embedding {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]domain.Embedding, len(i.entityEmbed))
	for k, v := range i.entityEmbed {
		out[k] = v
	}
	return out
}

// EntityCount returns the number of distinct entity keys currently indexed.
// Used by operator-feed stats and by ComputeReachability to size pre-allocations.
func (i *EntityIndex) EntityCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.entityDocs)
}

// NewEntityIndex builds an empty index. capPerEnt and capTotal are safety
// nets against runaway growth on long-running agents; both default to generous
// values (500 and 10_000) but can be tightened via Setters.
func NewEntityIndex() *EntityIndex {
	return &EntityIndex{
		entityDocs:  make(map[string][]IndexedDoc),
		entityName:  make(map[string]string),
		entityMeta:  make(map[string]EntityMetaKind),
		entityEmbed: make(map[string]domain.Embedding),
		capPerEnt:   500,
		capTotal:    10_000,
	}
}

// SetCaps overrides the LRU caps.
func (i *EntityIndex) SetCaps(perEntity, total int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if perEntity > 0 {
		i.capPerEnt = perEntity
	}
	if total > 0 {
		i.capTotal = total
	}
}

// Add records that docID mentioned entityKey with the given weight + meta-kind.
// LastSeen is set to the kernel clock. Duplicate docIDs are upserted (the newer
// weight wins; if both are present the higher is kept).
func (i *EntityIndex) Add(entityKey string, docID string, weight float64, kind EntityMetaKind, now int64) {
	if entityKey == "" || docID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	docs := i.entityDocs[entityKey]
	for j, d := range docs {
		if d.DocID == docID {
			if weight > d.Weight {
				docs[j].Weight = weight
			}
			docs[j].LastSeen = now
			i.entityDocs[entityKey] = docs
			return
		}
	}
	docs = append(docs, IndexedDoc{DocID: docID, Weight: weight, LastSeen: now})
	if len(docs) > i.capPerEnt {
		// LRU eviction: drop the oldest.
		sort.Slice(docs, func(a, b int) bool { return docs[a].LastSeen < docs[b].LastSeen })
		docs = docs[len(docs)-i.capPerEnt:]
	}
	i.entityDocs[entityKey] = docs
	i.entityMeta[entityKey] = kind
	// Bump total cap check: count distinct keys cheaply via len.
	if _, exists := i.entityDocs[entityKey]; !exists {
		if len(i.entityDocs) > i.capTotal {
			i.evictOldestLocked()
		}
	}
}

// SetNameEmbedding stores the embedding of the entity's display name; used by
// the query path to find the top-3 entity keys most relevant to the query.
// If no embedding is available, the recall path falls back to surface match.
func (i *EntityIndex) SetNameEmbedding(entityKey, name string, emb domain.Embedding) {
	if entityKey == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entityName[entityKey] = name
	i.entityEmbed[entityKey] = emb
}

// DocsFor returns a copy of the docID list for an entity, sorted by weight desc.
// The copy prevents the caller from mutating internal state.
func (i *EntityIndex) DocsFor(entityKey string) []IndexedDoc {
	i.mu.RLock()
	defer i.mu.RUnlock()
	src := i.entityDocs[entityKey]
	if len(src) == 0 {
		return nil
	}
	out := make([]IndexedDoc, len(src))
	copy(out, src)
	sort.Slice(out, func(a, b int) bool { return out[a].Weight > out[b].Weight })
	return out
}

// AllKeys returns a snapshot of every entity key currently in the index.
func (i *EntityIndex) AllKeys() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.entityDocs))
	for k := range i.entityDocs {
		out = append(out, k)
	}
	return out
}

// LookupTopByEmbedding finds the topK entity keys whose stored name embedding
// has the highest cosine similarity to query. Returns the keys in score-desc
// order. Skips entities without a stored embedding.
func (i *EntityIndex) LookupTopByEmbedding(query domain.Embedding, topK int) []string {
	if topK <= 0 {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	type scored struct {
		key   string
		score float64
	}
	scoredAll := make([]scored, 0, len(i.entityEmbed))
	for k, emb := range i.entityEmbed {
		if len(emb.Vector) == 0 || len(query.Vector) == 0 {
			continue
		}
		scoredAll = append(scoredAll, scored{key: k, score: cosineSimilarity(emb.Vector, query.Vector)})
	}
	sort.Slice(scoredAll, func(a, b int) bool { return scoredAll[a].score > scoredAll[b].score })
	if len(scoredAll) > topK {
		scoredAll = scoredAll[:topK]
	}
	out := make([]string, len(scoredAll))
	for j, s := range scoredAll {
		out[j] = s.key
	}
	return out
}

// Stats returns (total entities, total associations, top-3 entity key counts).
// Used by the operator feed (D3) for a cheap "is the graph alive" signal.
func (i *EntityIndex) Stats() (totalEntities, totalAssocs int, top []EntityStat) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	totalEntities = len(i.entityDocs)
	for _, docs := range i.entityDocs {
		totalAssocs += len(docs)
	}
	type kv struct {
		key string
		n   int
	}
	all := make([]kv, 0, totalEntities)
	for k, docs := range i.entityDocs {
		all = append(all, kv{k, len(docs)})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].n > all[b].n })
	for j := 0; j < 3 && j < len(all); j++ {
		top = append(top, EntityStat{Key: all[j].key, Count: all[j].n})
	}
	return
}

// EntityStat is one row in the operator-feed top-N.
type EntityStat struct {
	Key   string
	Count int
}

// Load rebuilds the in-memory index by streaming every edge in the graph store.
// Called once at boot before IngestMemory accepts writes. The rebuild is
// idempotent; running it twice produces the same state.
func (i *EntityIndex) Load(ctx context.Context, gs domain.GraphStore, embedder domain.Embedder, now int64) error {
	if gs == nil {
		return fmt.Errorf("EntityIndex.Load: nil graph store")
	}
	// Walk all keys the store knows about. GraphStore has no global iterator
	// today; we rely on a `keys()` method if present, else we fail with a
	// clear error (the kernel boot path should provide one).
	iter, ok := gs.(entityIndexLoader)
	if !ok {
		return fmt.Errorf("EntityIndex.Load: graph store %T does not implement entity-index loader", gs)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entityDocs = make(map[string][]IndexedDoc)
	i.entityName = make(map[string]string)
	i.entityMeta = make(map[string]EntityMetaKind)
	i.entityEmbed = make(map[string]domain.Embedding)
	i.loadedAt = now
	return iter.LoadIntoEntityIndex(ctx, i, embedder, now)
}

// entityIndexLoader is the optional extension on domain.GraphStore that the
// boot path uses to stream existing edges into the index. Adapters that don't
// implement it are skipped — the index stays empty and the recall path falls
// back to surface-similarity-only.
type entityIndexLoader interface {
	LoadIntoEntityIndex(ctx context.Context, idx *EntityIndex, embedder domain.Embedder, now int64) error
}

// evictOldestLocked removes the entity with the lowest mean weight × docs (i.e.
// the least valuable entry). Caller holds the write lock.
func (i *EntityIndex) evictOldestLocked() {
	type scored struct {
		key   string
		score float64
	}
	all := make([]scored, 0, len(i.entityDocs))
	for k, docs := range i.entityDocs {
		mean := 0.0
		for _, d := range docs {
			mean += d.Weight
		}
		if len(docs) > 0 {
			mean /= float64(len(docs))
		}
		all = append(all, scored{k, mean})
	}
	if len(all) == 0 {
		return
	}
	sort.Slice(all, func(a, b int) bool { return all[a].score < all[b].score })
	victim := all[0].key
	delete(i.entityDocs, victim)
	delete(i.entityMeta, victim)
	delete(i.entityName, victim)
	delete(i.entityEmbed, victim)
}

// IsEntityKey reports whether a string looks like an entity canonical key.
// Used by the spreading engine and the recall path to distinguish entity
// targets from doc targets in the `document_edges` table.
func IsEntityKey(s string) bool {
	if i := strings.IndexByte(s, ':'); i > 0 {
		k := EntityMetaKind(s[:i])
		return ValidMetaKinds[k]
	}
	return false
}
