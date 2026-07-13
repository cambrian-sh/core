package scope_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"
)

// memScopeStore is an in-memory AgentScopeStore shared by resolvers in a test.
type memScopeStore struct {
	mu   sync.Mutex
	data map[string]domain.ScopeConfig
}

func newMemStore() *memScopeStore { return &memScopeStore{data: map[string]domain.ScopeConfig{}} }

func (m *memScopeStore) LoadAll(context.Context) (map[string]domain.ScopeConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]domain.ScopeConfig, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out, nil
}

func (m *memScopeStore) Get(_ context.Context, id string) (domain.ScopeConfig, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.data[id]
	return cfg, ok, nil
}

func (m *memScopeStore) Save(_ context.Context, id string, cfg domain.ScopeConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = cfg
	return nil
}

func TestResolver_UnknownIsFailClosed(t *testing.T) {
	r := scope.NewScopeResolver(newMemStore(), time.Minute, nil)
	_, found := r.Get(context.Background(), "ghost")
	if found {
		t.Fatalf("unknown agent must return found=false (fail-closed)")
	}
}

func TestResolver_UnprofiledIsUnrestricted(t *testing.T) {
	store := newMemStore()
	_ = store.Save(context.Background(), "support", domain.ScopeConfig{}) // registered, empty
	r := scope.NewScopeResolver(store, time.Minute, nil)
	cfg, found := r.Get(context.Background(), "support")
	if !found {
		t.Fatalf("registered agent must be found")
	}
	if !cfg.IsZero() {
		t.Errorf("empty profile must be unrestricted, got %+v", cfg)
	}
}

func TestResolver_SaveValidatesAndRejectsUnsatisfiable(t *testing.T) {
	r := scope.NewScopeResolver(newMemStore(), time.Minute, nil)
	bad := domain.ScopeConfig{RequiredTags: []string{"secrets"}, ForbiddenTags: []string{"secrets"}}
	if err := r.SaveScope(context.Background(), "zombie", bad); err == nil {
		t.Fatalf("SaveScope must reject an unsatisfiable profile")
	}
	if _, found := r.Get(context.Background(), "zombie"); found {
		t.Errorf("a rejected profile must not be persisted")
	}
}

// Two resolvers over one shared store + a fake NOTIFY channel: a scope updated by
// replica A is reflected in replica B after the invalidation signal arrives.
func TestResolver_CrossReplicaInvalidation(t *testing.T) {
	store := newMemStore()
	_ = store.Save(context.Background(), "agentX", domain.ScopeConfig{ForbiddenTags: []string{"secrets"}})

	replicaA := scope.NewScopeResolver(store, time.Hour, nil)
	replicaB := scope.NewScopeResolver(store, time.Hour, nil) // long TTL: only NOTIFY can refresh
	if err := replicaB.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}

	// B has the old value cached.
	if cfg, _ := replicaB.Get(context.Background(), "agentX"); !cfg.Forbids("secrets") {
		t.Fatalf("precondition: B should see the original scope")
	}

	// A updates the scope (persists to the shared store).
	newCfg := domain.ScopeConfig{ForbiddenTags: []string{"secrets", "internal_only"}}
	if err := replicaA.SaveScope(context.Background(), "agentX", newCfg); err != nil {
		t.Fatal(err)
	}

	// Without a NOTIFY, B still serves the stale (long-TTL) value.
	if cfg, _ := replicaB.Get(context.Background(), "agentX"); cfg.Forbids("internal_only") {
		t.Fatalf("B must still be stale before NOTIFY delivery")
	}

	// Deliver the NOTIFY to B's invalidation watcher.
	ch := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go replicaB.WatchInvalidations(ctx, ch)
	ch <- "agentX"

	// Eventually B re-reads the store and sees the new scope.
	deadline := time.After(time.Second)
	for {
		if cfg, _ := replicaB.Get(context.Background(), "agentX"); cfg.Forbids("internal_only") {
			return // success
		}
		select {
		case <-deadline:
			t.Fatalf("B never observed the updated scope after NOTIFY")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestResolver_ServesStaleOnStoreError(t *testing.T) {
	store := &erroringStore{inner: newMemStore()}
	_ = store.inner.Save(context.Background(), "a", domain.ScopeConfig{ForbiddenTags: []string{"x"}})
	r := scope.NewScopeResolver(store, time.Nanosecond, nil) // TTL ~0 → always re-read
	if err := r.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	time.Sleep(time.Millisecond)
	cfg, found := r.Get(context.Background(), "a")
	if !found || !cfg.Forbids("x") {
		t.Errorf("expected stale value served on store error, got found=%v cfg=%+v", found, cfg)
	}
}

type erroringStore struct {
	inner *memScopeStore
	fail  bool
}

func (e *erroringStore) LoadAll(ctx context.Context) (map[string]domain.ScopeConfig, error) {
	return e.inner.LoadAll(ctx)
}
func (e *erroringStore) Get(ctx context.Context, id string) (domain.ScopeConfig, bool, error) {
	if e.fail {
		return domain.ScopeConfig{}, false, errors.New("boom")
	}
	return e.inner.Get(ctx, id)
}
func (e *erroringStore) Save(ctx context.Context, id string, cfg domain.ScopeConfig) error {
	return e.inner.Save(ctx, id, cfg)
}
