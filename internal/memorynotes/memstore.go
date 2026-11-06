package memorynotes

import (
	"context"
	"sort"
	"sync"
)

// MemStore is the in-memory Store: tests, and a kernel booted without Postgres
// (notes then last for the process only, which the composition root logs).
type MemStore struct {
	mu      sync.Mutex
	docs    map[Key]Document
	budgets map[Key]int
	history []HistoryEntry
}

// NewMemStore builds an empty store.
func NewMemStore() *MemStore {
	return &MemStore{docs: make(map[Key]Document), budgets: make(map[Key]int)}
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, key Key) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[key]
	if !ok {
		return Document{Key: key}, nil
	}
	return d, nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, f ListFilter) ([]Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Document
	for k, d := range m.docs {
		if !d.Exists() || (f.Scope != "" && k.Scope != f.Scope) || (f.OwnerID != "" && k.OwnerID != f.OwnerID) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].OwnerID < out[j].OwnerID
	})
	return out, nil
}

func (m *MemStore) write(key Key, content string, expect int64, w WriteMeta, deleted bool) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.docs[key]
	if cur.Revision != expect {
		return Document{}, ErrRevisionConflict
	}
	d := Document{Key: key, Content: content, Revision: expect + 1, Deleted: deleted,
		UpdatedAt: w.At, UpdatedBy: w.Actor}
	hist := content
	if deleted {
		d.Content = ""
		hist = cur.Content
	}
	m.docs[key] = d
	m.history = append(m.history, HistoryEntry{Key: key, Revision: d.Revision, Op: w.Op,
		Actor: w.Actor, Reason: w.Reason, Content: hist, At: w.At})
	return d, nil
}

// Put implements Store.
func (m *MemStore) Put(_ context.Context, key Key, content string, expect int64, w WriteMeta) (Document, error) {
	return m.write(key, content, expect, w, false)
}

// Delete implements Store.
func (m *MemStore) Delete(_ context.Context, key Key, expect int64, w WriteMeta) (Document, error) {
	return m.write(key, "", expect, w, true)
}

// GetBudget implements Store.
func (m *MemStore) GetBudget(_ context.Context, scope Scope, owner string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.budgets[Key{Scope: scope, OwnerID: owner}]
	return b, ok, nil
}

// SetBudget implements Store.
func (m *MemStore) SetBudget(_ context.Context, scope Scope, owner string, budget int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgets[Key{Scope: scope, OwnerID: owner}] = budget
	return nil
}

// ClearBudget implements Store.
func (m *MemStore) ClearBudget(_ context.Context, scope Scope, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.budgets, Key{Scope: scope, OwnerID: owner})
	return nil
}

// History returns every write in order (tests and inspection).
func (m *MemStore) History() []HistoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]HistoryEntry(nil), m.history...)
}
