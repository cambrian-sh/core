package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeCallerProvider implements both ScopeProvider and CallerScopeProvider.
type fakeCallerProvider struct {
	agent map[string]domain.ScopeConfig
	known map[string]bool
}

func (p *fakeCallerProvider) EffectiveForAgent(_ context.Context, id string) (*domain.EffectiveScope, bool) {
	if !p.known[id] {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, p.agent[id])
	return &eff, true
}

func (p *fakeCallerProvider) EffectiveForCaller(_ context.Context, id string, caller domain.ScopeConfig) (*domain.EffectiveScope, bool) {
	if !p.known[id] {
		return nil, false
	}
	eff := domain.NewEffectiveScope(caller, p.agent[id])
	return &eff, true
}

// fakeSessions returns a fixed caller_scope per session ID.
type fakeSessions struct{ byID map[string]domain.ScopeConfig }

func (s fakeSessions) CallerScope(_ context.Context, sid string) domain.ScopeConfig {
	return s.byID[sid]
}

// Phase 2: a session-carried caller_scope narrows the effective scope (caller ∩
// agent), and the value is sourced from the session record (via ctx session ID),
// never from any caller-supplied payload.
func TestQueryService_Phase2_SessionCallerScopeNarrows(t *testing.T) {
	store := corpus() // docs: kb(public_kb), secret(secrets)
	q := NewQueryService(&fakeEmbedder{}, store)

	prov := &fakeCallerProvider{
		known: map[string]bool{"analyst": true},
		agent: map[string]domain.ScopeConfig{"analyst": {}}, // agent unrestricted
	}
	q.EnableScoping(prov, store)
	// Session caller_scope forbids secrets — even though the agent is unrestricted.
	q.EnablePhase2(prov, fakeSessions{byID: map[string]domain.ScopeConfig{
		"sess-1": {ForbiddenTags: []string{"secrets"}},
	}})

	// With session ID in ctx → caller_scope applies → secrets excluded.
	ctx := domain.WithSessionID(context.Background(), "sess-1")
	res, _ := q.Search(ctx, "anything", "analyst")
	if collect(res)["secret"] {
		t.Errorf("Phase-2 caller_scope must exclude secrets, got %v", collect(res))
	}
	if !collect(res)["kb"] {
		t.Errorf("public doc should still be visible")
	}

	// Without session ID → Phase-1 agent_scope only (agent unrestricted) → all docs.
	res2, _ := q.Search(context.Background(), "anything", "analyst")
	if !collect(res2)["secret"] {
		t.Errorf("without a session caller_scope, the unrestricted agent sees all docs")
	}
}

// The caller_scope used is the one from the session record, not any tag a caller
// might try to inject — the Search signature exposes no caller-tag parameter.
func TestQueryService_Phase2_UnknownSessionFallsBackToAgent(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store)
	prov := &fakeCallerProvider{
		known: map[string]bool{"support": true},
		agent: map[string]domain.ScopeConfig{"support": {ForbiddenTags: []string{"secrets"}}},
	}
	q.EnableScoping(prov, store)
	q.EnablePhase2(prov, fakeSessions{byID: map[string]domain.ScopeConfig{}}) // no session scopes

	ctx := domain.WithSessionID(context.Background(), "ghost-session")
	res, _ := q.Search(ctx, "anything", "support")
	// Unknown session → empty caller_scope → Phase-1 agent_scope still forbids secrets.
	if collect(res)["secret"] {
		t.Errorf("agent_scope must still apply when session caller_scope is absent")
	}
}
