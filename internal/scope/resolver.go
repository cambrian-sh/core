package scope

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// AgentScopeStore is the persistence port for agent scope profiles. It is the
// authoritative, replica-visible source of agent_scope (ADR-0034 D8/D9/R1).
// The Postgres implementation lives in internal/infrastructure/postgres; BBolt
// deliberately does NOT hold scope (that was the single-process fiction R1 fixed).
type AgentScopeStore interface {
	// LoadAll returns every agentID→ScopeConfig for boot-time cache warming.
	LoadAll(ctx context.Context) (map[string]domain.ScopeConfig, error)
	// Get returns one agent's scope. found==false means the agentID is unknown.
	Get(ctx context.Context, agentID string) (domain.ScopeConfig, bool, error)
	// Save validates-then-persists one agent's scope (caller validates first).
	Save(ctx context.Context, agentID string, cfg domain.ScopeConfig) error
}

// AgentWriteTagStore is the persistence port for per-agent DefaultWriteTags
// (ADR-0035 C2). Optional — a store that does not implement it leaves write
// classification empty (unclassified writes, visible only to unrestricted readers).
type AgentWriteTagStore interface {
	LoadAllWriteTags(ctx context.Context) (map[string][]string, error)
	GetWriteTags(ctx context.Context, agentID string) ([]string, bool, error)
	SaveWriteTags(ctx context.Context, agentID string, tags []string) error
}

// AgentExister disambiguates a scope-store miss: a registered agent with no scope
// row is "unprofiled" (unrestricted, found=true), whereas an agentID unknown to
// the registry is an unknown principal (fail-closed, found=false). ADR-0034 (D8).
type AgentExister interface {
	HasAgent(agentID string) bool
}

const defaultScopeTTL = 60 * time.Second

// errWriteStoreUnavailable is returned by SaveWriteTags when no DefaultWriteTags
// store is configured (ADR-0035 C2).
var errWriteStoreUnavailable = errors.New("scope: write-tag store not configured")

type cacheEntry struct {
	cfg       domain.ScopeConfig
	fetchedAt time.Time
}

// ScopeResolver resolves agentID→ScopeConfig with an O(1) in-memory cache (no
// hot-path DB query), warmed from the store at boot and invalidated across
// replicas on LISTEN/NOTIFY plus a safety TTL. Revocation is bounded by notify
// latency, not by process restart. ADR-0034 (D8/R1).
//
// Absence semantics (never conflated):
//   - found==true  + empty ScopeConfig → registered-but-unprofiled (unrestricted;
//     caller_scope still narrows). ADR-0034 (D9).
//   - found==false                     → unknown principal: FAIL-CLOSED. Write
//     paths reject; read paths treat as ScopeSystem-eligible only. Logged as a
//     security warning.
type ScopeResolver struct {
	store  AgentScopeStore
	ttl    time.Duration
	logger     *slog.Logger
	now        func() time.Time
	exister    AgentExister       // optional; disambiguates store-miss (D8)
	writeStore AgentWriteTagStore // optional; per-agent DefaultWriteTags (ADR-0035 C2)

	mu         sync.RWMutex
	cache      map[string]cacheEntry
	writeCache map[string][]string
}

// SetExister wires the registry existence check used to distinguish an unprofiled
// registered agent (unrestricted) from an unknown principal (fail-closed).
func (r *ScopeResolver) SetExister(e AgentExister) { r.exister = e }

// NewScopeResolver builds a resolver over store. ttl<=0 uses the 60s default; a
// nil logger uses slog.Default().
func NewScopeResolver(store AgentScopeStore, ttl time.Duration, logger *slog.Logger) *ScopeResolver {
	if ttl <= 0 {
		ttl = defaultScopeTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &ScopeResolver{
		store:      store,
		ttl:        ttl,
		logger:     logger,
		now:        time.Now,
		cache:      make(map[string]cacheEntry),
		writeCache: make(map[string][]string),
	}
	// Opt-in DefaultWriteTags support when the store provides it (ADR-0035 C2).
	if wts, ok := store.(AgentWriteTagStore); ok {
		r.writeStore = wts
	}
	return r
}

// Warm loads every scope profile into the cache. Call once at boot.
func (r *ScopeResolver) Warm(ctx context.Context) error {
	all, err := r.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	r.mu.Lock()
	for id, cfg := range all {
		r.cache[id] = cacheEntry{cfg: cfg, fetchedAt: now}
	}
	r.mu.Unlock()

	// ADR-0035 C2: warm DefaultWriteTags too, when supported.
	if r.writeStore != nil {
		wt, err := r.writeStore.LoadAllWriteTags(ctx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		for id, tags := range wt {
			r.writeCache[id] = tags
		}
		r.mu.Unlock()
	}
	return nil
}

// DefaultWriteTags returns the operator-configured write classification for an
// agent (ADR-0035 C2). Empty when the agent is unprofiled, unknown, or no write
// store is configured — an empty result means an UNCLASSIFIED write (visible only
// to unrestricted/ScopeSystem readers).
func (r *ScopeResolver) DefaultWriteTags(ctx context.Context, agentID string) []string {
	r.mu.RLock()
	tags, cached := r.writeCache[agentID]
	r.mu.RUnlock()
	if cached {
		return tags
	}
	if r.writeStore == nil {
		return nil
	}
	tags, found, err := r.writeStore.GetWriteTags(ctx, agentID)
	if err != nil || !found {
		return nil
	}
	r.mu.Lock()
	r.writeCache[agentID] = tags
	r.mu.Unlock()
	return tags
}

// SaveWriteTags persists an agent's DefaultWriteTags and refreshes the cache.
func (r *ScopeResolver) SaveWriteTags(ctx context.Context, agentID string, tags []string) error {
	if r.writeStore == nil {
		return errWriteStoreUnavailable
	}
	if err := r.writeStore.SaveWriteTags(ctx, agentID, tags); err != nil {
		return err
	}
	r.mu.Lock()
	r.writeCache[agentID] = tags
	r.mu.Unlock()
	return nil
}

// Get returns the agent's scope and whether the agentID is known. A fresh cache
// hit is O(1); a miss or TTL-stale entry re-reads the store. On a store error a
// stale entry is served (availability) if present, else not-found. ADR-0034 (D8).
func (r *ScopeResolver) Get(ctx context.Context, agentID string) (domain.ScopeConfig, bool) {
	r.mu.RLock()
	e, cached := r.cache[agentID]
	r.mu.RUnlock()
	if cached && r.now().Sub(e.fetchedAt) < r.ttl {
		return e.cfg, true
	}

	cfg, found, err := r.store.Get(ctx, agentID)
	if err != nil {
		if cached {
			return e.cfg, true // serve stale rather than fail open/closed on transient error
		}
		r.logger.WarnContext(ctx, "scope: resolver store read failed",
			slog.String("event", "scope_resolver_error"),
			slog.String("agent_id", agentID),
			slog.Any("err", err))
		return domain.ScopeConfig{}, false
	}
	if !found {
		// No scope row. If the agent is registered, it is unprofiled →
		// unrestricted (empty scope, found=true). Otherwise it is an unknown
		// principal → fail-closed. ADR-0034 (D8).
		if r.exister != nil && r.exister.HasAgent(agentID) {
			r.mu.Lock()
			r.cache[agentID] = cacheEntry{cfg: domain.ScopeConfig{}, fetchedAt: r.now()}
			r.mu.Unlock()
			return domain.ScopeConfig{}, true
		}
		r.mu.Lock()
		delete(r.cache, agentID)
		r.mu.Unlock()
		r.logger.WarnContext(ctx, "scope: unknown agent principal (fail-closed)",
			slog.String("event", "scope_unknown_principal"),
			slog.String("agent_id", agentID))
		return domain.ScopeConfig{}, false
	}

	r.mu.Lock()
	r.cache[agentID] = cacheEntry{cfg: cfg, fetchedAt: r.now()}
	r.mu.Unlock()
	return cfg, true
}

// SaveScope validates then persists an agent's scope and refreshes the local
// cache. Cross-replica propagation is the store's responsibility (NOTIFY); this
// replica updates immediately. Returns a validation error (the admin API maps it
// to 400) without persisting on conflict.
func (r *ScopeResolver) SaveScope(ctx context.Context, agentID string, cfg domain.ScopeConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := r.store.Save(ctx, agentID, cfg); err != nil {
		return err
	}
	r.mu.Lock()
	r.cache[agentID] = cacheEntry{cfg: cfg, fetchedAt: r.now()}
	r.mu.Unlock()
	return nil
}

// EffectiveForAgent returns the Phase-1 effective READ scope for an agent: the
// intersection of an EMPTY caller_scope (caller tags are advisory/ignored until
// Phase 2 — ADR-0034 R2/D13) with the agent's intrinsic agent_scope. The bool is
// false for an unknown principal, which the caller MUST treat as fail-closed
// (deny). A registered-but-unprofiled agent yields a non-nil, empty (unrestricted)
// effective scope.
func (r *ScopeResolver) EffectiveForAgent(ctx context.Context, agentID string) (*domain.EffectiveScope, bool) {
	cfg, found := r.Get(ctx, agentID)
	if !found {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, cfg)
	return &eff, true
}

// EffectiveForCaller returns the Phase-2 effective scope: the intersection of a
// caller_scope (re-derived SERVER-SIDE from the persisted Session.CallerScope —
// never from Handoff.Context) with the agent's intrinsic agent_scope. The bool is
// false for an unknown principal (fail-closed). Neither side can escalate the
// other. ADR-0034 (D13 Phase 2).
func (r *ScopeResolver) EffectiveForCaller(ctx context.Context, agentID string, caller domain.ScopeConfig) (*domain.EffectiveScope, bool) {
	cfg, found := r.Get(ctx, agentID)
	if !found {
		return nil, false
	}
	eff := domain.NewEffectiveScope(caller, cfg)
	return &eff, true
}

// Invalidate drops the cached entry for agentID; the next Get re-reads the store.
// Driven by LISTEN/NOTIFY for cross-replica revocation.
func (r *ScopeResolver) Invalidate(agentID string) {
	r.mu.Lock()
	delete(r.cache, agentID)
	delete(r.writeCache, agentID)
	r.mu.Unlock()
}

// WatchInvalidations consumes agentIDs from ch (fed by the store's LISTEN/NOTIFY
// subscription) and invalidates each, until ctx is cancelled or ch closes.
func (r *ScopeResolver) WatchInvalidations(ctx context.Context, ch <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			r.Invalidate(id)
		}
	}
}
