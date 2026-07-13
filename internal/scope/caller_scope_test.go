package scope_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"
)

// Phase 2: effective scope re-derived from the persisted caller_scope ∩ agent_scope.
// The caller value comes from the SESSION record, never from Handoff.Context.
func TestPhase2_EffectiveForCaller_Intersection(t *testing.T) {
	store := newMemStore()
	// Agent forbids internal_only; caller (from session) requires customer_789.
	_ = store.Save(context.Background(), "support", domain.ScopeConfig{ForbiddenTags: []string{"internal_only"}})
	r := scope.NewScopeResolver(store, time.Minute, nil)

	caller := domain.ScopeConfig{RequiredTags: []string{"customer_789"}, ForbiddenTags: []string{"secrets"}}
	eff, ok := r.EffectiveForCaller(context.Background(), "support", caller)
	if !ok {
		t.Fatal("expected known principal")
	}
	// Neither side can escalate the other: both Forbidden tags present (union),
	// caller Required present.
	if !eff.Forbids("secrets") || !eff.Forbids("internal_only") {
		t.Errorf("effective must union both ForbiddenTags, got %+v", eff.ForbiddenTags)
	}
	// A doc tagged customer_789/public passes; secrets/internal_only excluded.
	if !eff.Allows([]string{"customer_789"}) {
		t.Errorf("doc satisfying required+no-forbidden should pass")
	}
	if eff.Allows([]string{"customer_789", "secrets"}) {
		t.Errorf("caller ForbiddenTags must still apply")
	}
}

func TestPhase2_EffectiveForCaller_UnknownPrincipalFailsClosed(t *testing.T) {
	r := scope.NewScopeResolver(newMemStore(), time.Minute, nil)
	if _, ok := r.EffectiveForCaller(context.Background(), "ghost", domain.ScopeConfig{}); ok {
		t.Errorf("unknown principal must be fail-closed even with a caller scope")
	}
}

// Forgeability regression: the caller scope used for enforcement is the one
// PASSED to EffectiveForCaller (sourced server-side from the session). A different
// value forged in Handoff.Context cannot be the input because the Substrate never
// reads it here. We assert that two different caller scopes produce two different
// effective scopes — proving the caller value is load-bearing and must therefore
// come from a non-forgeable source.
func TestPhase2_CallerValueIsLoadBearing(t *testing.T) {
	store := newMemStore()
	_ = store.Save(context.Background(), "agent", domain.ScopeConfig{})
	r := scope.NewScopeResolver(store, time.Minute, nil)

	narrow, _ := r.EffectiveForCaller(context.Background(), "agent", domain.ScopeConfig{ForbiddenTags: []string{"secrets"}})
	wide, _ := r.EffectiveForCaller(context.Background(), "agent", domain.ScopeConfig{})

	if narrow.Allows([]string{"secrets"}) {
		t.Errorf("narrow caller must forbid secrets")
	}
	if !wide.Allows([]string{"secrets"}) {
		t.Errorf("wide caller (unrestricted) should allow secrets")
	}
	// If an attacker could substitute the wide caller for the narrow one, they'd
	// widen scope — which is exactly why the caller scope must be read from the
	// server-side session record, not the agent-held Handoff.Context.
}
