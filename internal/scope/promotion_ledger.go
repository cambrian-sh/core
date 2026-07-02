package scope

import "sync"

// InMemoryLedger is a process-local PromotionLedger. It tracks which cluster keys
// have been emitted (idempotency) and which key each source hash was last promoted
// under (supersede lookup). A BBolt-backed ledger (out-of-band bucket, raw docs
// untouched) is the production form. ADR-0034 (D11).
type InMemoryLedger struct {
	mu      sync.Mutex
	keys    map[string]struct{} // promoted cluster dedup keys
	hashKey map[string]string   // source_hash → latest key it was promoted under
}

// NewInMemoryLedger creates an empty ledger.
func NewInMemoryLedger() *InMemoryLedger {
	return &InMemoryLedger{keys: map[string]struct{}{}, hashKey: map[string]string{}}
}

// AlreadyPromoted reports whether this exact cluster (dedup key) was emitted.
func (l *InMemoryLedger) AlreadyPromoted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.keys[key]
	return ok
}

// Record marks the cluster key and its source hashes as promoted.
func (l *InMemoryLedger) Record(sourceHashes []string, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys[key] = struct{}{}
	for _, h := range sourceHashes {
		l.hashKey[h] = key
	}
	return nil
}

// PriorKeyFor returns the key of an earlier (narrower) insight that any of the
// cluster's source hashes was promoted under, excluding the cluster's own current
// key. Empty when none — i.e. nothing to supersede.
func (l *InMemoryLedger) PriorKeyFor(cluster ThemeCluster) string {
	current := dedupKey(cluster.SourceHashes())
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range cluster.SourceHashes() {
		if k, ok := l.hashKey[h]; ok && k != current {
			return k
		}
	}
	return ""
}
