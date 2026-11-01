package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// listingStore answers Search with a fixed row set and records what it was asked.
type listingStore struct {
	stubVectorStore
	rows     []domain.SearchResult
	lastOpts domain.SearchOptions
	ctxScope bool
}

func (l *listingStore) Search(ctx context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	l.lastOpts = opts
	_, l.ctxScope = domain.ScopeFromContext(ctx)
	return l.rows, nil
}

func profileRow(t *testing.T, agentID, hash string, at time.Time, success float64) domain.SearchResult {
	t.Helper()
	b, err := json.Marshal(domain.AgentProfile{AgentID: agentID, SuccessRate: success})
	if err != nil {
		t.Fatal(err)
	}
	return domain.SearchResult{Document: domain.Document{
		ID:           "profile:" + agentID + ":" + hash,
		DocumentType: domain.DocTypeAgentProfile,
		Text:         string(b),
		CreatedAt:    at,
		Metadata:     map[string]interface{}{"agent_id": agentID, "source_hash": hash},
	}}
}

// T-20261101-interview-reinterview-never-finds-the-previous: the newest profile
// under ANOTHER hash is returned; the current hash and other agents' rows are
// never; scope travels on both SearchOptions and ctx (AGENTS.md §2.4); the
// agent id is escaped into the raw Filter.
func TestGetPreviousProfile_NewestOtherVersion(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1750000000, 0)
	ls := &listingStore{rows: []domain.SearchResult{
		profileRow(t, "a", "h1", t0, 0.1),
		profileRow(t, "a", "h2", t0.Add(time.Hour), 0.2),
		profileRow(t, "a", "h3", t0.Add(2*time.Hour), 0.3), // current
		profileRow(t, "b", "hx", t0.Add(3*time.Hour), 0.9), // other agent (Filter ignored)
	}}
	got, err := NewProfileStore(ls).GetPreviousProfile(context.Background(), "a", "h3")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SourceHash != "h2" || got.SuccessRate != 0.2 {
		t.Fatalf("got %+v, want the h2 profile (newest non-current, this agent)", got)
	}
	if ls.lastOpts.DocumentType != domain.DocTypeAgentProfile {
		t.Errorf("did not target agent_profiles: %q", ls.lastOpts.DocumentType)
	}
	if ls.lastOpts.Scope == nil || !ls.ctxScope {
		t.Errorf("scope not on both channels: opts=%v ctx=%v", ls.lastOpts.Scope, ls.ctxScope)
	}
}

func TestGetPreviousProfile_NoOtherVersionIsNil(t *testing.T) {
	t.Parallel()
	ls := &listingStore{rows: []domain.SearchResult{profileRow(t, "a", "h1", time.Unix(1, 0), 0.5)}}
	got, err := NewProfileStore(ls).GetPreviousProfile(context.Background(), "a", "h1")
	if err != nil || got != nil {
		t.Fatalf("got %+v err=%v, want nil, nil", got, err)
	}
}

func TestGetPreviousProfile_EscapesAgentID(t *testing.T) {
	t.Parallel()
	ls := &listingStore{}
	if _, err := NewProfileStore(ls).GetPreviousProfile(context.Background(), "x' OR '1'='1", "h"); err != nil {
		t.Fatal(err)
	}
	if f := ls.lastOpts.Filter; !strings.Contains(f, "x'' OR ''1''=''1") || strings.Count(f, "'")%2 != 0 {
		t.Fatalf("agent id not escaped: %q", f)
	}
}
