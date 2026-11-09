package executer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// mapStepCache is a real (in-memory) domain.StepCache: Get returns what Put stored
// under the same key, so a test sees exactly what a second run would be served.
type mapStepCache struct {
	mu   sync.Mutex
	m    map[string]*domain.Handoff
	puts int
}

func newMapStepCache() *mapStepCache { return &mapStepCache{m: map[string]*domain.Handoff{}} }

func (c *mapStepCache) Get(_ context.Context, key string) (*domain.Handoff, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.m[key]
	if !ok {
		return nil, false, nil
	}
	cp := *h
	return &cp, true, nil
}

func (c *mapStepCache) Put(_ context.Context, key string, h *domain.Handoff, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *h
	c.m[key] = &cp
	c.puts++
	return nil
}

func testEffectVocabulary(t *testing.T) *domain.StepEffectVocabulary {
	t.Helper()
	v, err := domain.NewStepEffectVocabulary([]domain.StepEffectTerm{
		{Name: "read", Classes: []domain.ToolEffect{domain.EffectRead}},
		{Name: "write", Classes: []domain.ToolEffect{domain.EffectWrite}},
		{Name: "move", Refines: domain.EffectWrite, Tools: []string{"*move*"}},
		{Name: "egress", Classes: []domain.ToolEffect{domain.EffectEgress}},
		{Name: "send", Refines: domain.EffectEgress, Tools: []string{"*send*"}},
	})
	if err != nil {
		t.Fatalf("vocabulary: %v", err)
	}
	return v
}

// cacheTwoStepPlan: a root read step and a dependent step. The dependent step's cache
// key used to be subject + query + upstream output only.
func cacheTwoStepPlan(dependentEffects []string) *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Subject: "quarterly totals",
		Steps: []domain.Step{
			{Query: "list the shared invoice folder", RequiredEffects: []string{"read"}},
			{Query: "summarise the private notes on these invoices", DependsOn: []int{0}, RequiredEffects: dependentEffects},
		},
	}
}

// runAs executes plan for one principal in one session and returns the dependent
// step's result and how many times the dependent step actually ran.
func runAs(t *testing.T, cache domain.StepCache, vocab *domain.StepEffectVocabulary, principal string, session domain.SessionID, plan *domain.ExecutionPlan) (string, int) {
	t.Helper()
	ex := &DAGExecutor{StepCache: cache, EffectVocabulary: vocab, CurrentSessionID: session}
	ran := 0
	stepFn := func(_ context.Context, idx int, _ *domain.Handoff) (*domain.Handoff, error) {
		if idx == 0 {
			// Same shared data for every principal: identical upstream output.
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("inv-1, inv-2")}}, nil
		}
		ran++
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("notes visible to " + principal)}}, nil
	}
	ctx := domain.WithPrincipal(t.Context(), domain.UserPrincipal(principal))
	ctx = domain.WithRunPrincipal(ctx, principal)
	out, err := ex.Execute(ctx, plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute as %s: %v", principal, err)
	}
	return out["step_1_result"], ran
}

// T-20261108-step-cache-key-omits-principal-scope-and (1): principal B must never be
// served the dependent step result computed for principal A.
func TestStepCache_DoesNotCrossPrincipals(t *testing.T) {
	cache := newMapStepCache()
	vocab := testEffectVocabulary(t)
	plan := cacheTwoStepPlan([]string{"read"})

	a, _ := runAs(t, cache, vocab, "alice", "ses-alice", plan)
	if a != "notes visible to alice" {
		t.Fatalf("alice run: got %q", a)
	}
	b, ranB := runAs(t, cache, vocab, "bob", "ses-bob", cacheTwoStepPlan([]string{"read"}))
	if b != "notes visible to bob" || ranB != 1 {
		t.Fatalf("bob was served %q (dependent step ran %d times): the step cache crossed principals", b, ranB)
	}
}

// Same principal, same session: a read-only dependent step is still reused.
func TestStepCache_ReusesWithinSamePrincipalAndSession(t *testing.T) {
	cache := newMapStepCache()
	vocab := testEffectVocabulary(t)

	runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan([]string{"read"}))
	got, ran := runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan([]string{"read"}))
	if ran != 0 || got != "notes visible to alice" {
		t.Fatalf("read-only dependent step not reused within the same principal+session: ran=%d got=%q", ran, got)
	}
}

// A different read scope on the same principal and session is a different key.
func TestStepCache_ScopeIsPartOfTheKey(t *testing.T) {
	cache := newMapStepCache()
	vocab := testEffectVocabulary(t)
	run := func(scope *domain.TagPredicate) int {
		ex := &DAGExecutor{StepCache: cache, EffectVocabulary: vocab, CurrentSessionID: "ses-1"}
		ran := 0
		stepFn := func(_ context.Context, idx int, _ *domain.Handoff) (*domain.Handoff, error) {
			if idx == 1 {
				ran++
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}, nil
		}
		ctx := domain.WithScope(domain.WithPrincipal(t.Context(), domain.UserPrincipal("alice")), scope)
		if _, err := ex.Execute(ctx, cacheTwoStepPlan([]string{"read"}), nil, stepFn); err != nil {
			t.Fatal(err)
		}
		return ran
	}
	run(&domain.TagPredicate{RequiredTags: []string{"finance"}})
	if ran := run(&domain.TagPredicate{RequiredTags: []string{"sales"}}); ran != 1 {
		t.Fatalf("a narrower/different scope was served the other scope's cached result (ran=%d)", ran)
	}
}

// T-20261108-step-cache-key-omits-principal-scope-and (2): a side-effecting step is
// never served from cache — its tools must run every time — and never stored.
func TestStepCache_SideEffectingStepsAlwaysRun(t *testing.T) {
	vocab := testEffectVocabulary(t)
	for _, effects := range [][]string{{"write"}, {"move"}, {"send"}, {"egress"}, {"read", "write"}, {"unknown-effect"}} {
		cache := newMapStepCache()
		runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan(effects))
		_, ran := runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan(effects))
		if ran != 1 {
			t.Errorf("effects %v: dependent step served from cache, its tools did not run", effects)
		}
		if cache.puts != 2 { // only the two read-only root steps
			t.Errorf("effects %v: %d puts, want 2 (the side-effecting step must not be stored)", effects, cache.puts)
		}
	}
}

// A step that declares no effects (legacy plan, nil) may act: fail closed, never cached.
func TestStepCache_UndeclaredEffectsAreNotCached(t *testing.T) {
	cache := newMapStepCache()
	vocab := testEffectVocabulary(t)
	runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan(nil))
	if _, ran := runAs(t, cache, vocab, "alice", "ses-alice", cacheTwoStepPlan(nil)); ran != 1 {
		t.Fatal("a step with undeclared effects was served from cache")
	}
}

// No session and no principal on the context: nothing identifies whose result it
// is, so nothing is cached.
func TestStepCache_NoIdentityNoCache(t *testing.T) {
	cache := newMapStepCache()
	ex := &DAGExecutor{StepCache: cache, EffectVocabulary: testEffectVocabulary(t)}
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}, nil
	}
	if _, err := ex.Execute(t.Context(), cacheTwoStepPlan([]string{"read"}), nil, stepFn); err != nil {
		t.Fatal(err)
	}
	if cache.puts != 0 {
		t.Fatalf("cached %d results with no principal or session on the run", cache.puts)
	}
}
