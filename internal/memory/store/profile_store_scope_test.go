package store

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// recordingStore captures what reached the underlying store, and can refuse
// by-id reads the way the raw adapter never would.
type recordingStore struct {
	stubVectorStore
	lastOpts   domain.SearchOptions
	searchSeen bool
	getSeen    bool
}

func (r *recordingStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	r.searchSeen = true
	r.lastOpts = opts
	return nil, nil
}

func (r *recordingStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	r.getSeen = true
	return nil, nil
}

// A judicial-record lookup must not let an agent id carrying a quote break out of
// the SQL string literal it is interpolated into. SearchOptions.Filter is applied
// by the adapter as a RAW predicate (goqu.L), so this is the only thing standing
// between a registered agent's self-chosen id and arbitrary SQL.
func TestGetJudicialRecords_EscapesQuotesInFilter(t *testing.T) {
	rec := &recordingStore{}
	store := NewProfileStore(rec)

	// The classic payload: close the literal, OR a tautology, comment out the rest.
	malicious := "x' OR '1'='1"
	if _, err := store.GetJudicialRecords(context.Background(), malicious, "hash", 5); err != nil {
		t.Fatalf("GetJudicialRecords: %v", err)
	}
	if !rec.searchSeen {
		t.Fatal("expected the lookup to reach the store")
	}

	got := rec.lastOpts.Filter
	// Escaped, the payload's quotes are doubled and it stays one literal.
	if !strings.Contains(got, "x'' OR ''1''=''1") {
		t.Errorf("agent id was not escaped into the filter; got %q", got)
	}
	// Unescaped, the filter would contain an odd number of quotes and the
	// tautology would be live SQL. Counting quotes is the property that matters:
	// every quote must be part of a balanced, doubled pair.
	if strings.Count(got, "'")%2 != 0 {
		t.Errorf("filter has unbalanced quotes, so the literal is escapable: %q", got)
	}
}

func TestSQLQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"plain":     "plain",
		"o'brien":   "o''brien",
		"''":        "''''",
		"":          "",
		"a'b'c":     "a''b''c",
		"no-quotes": "no-quotes",
	}
	for in, want := range cases {
		if got := sqlQuoteLiteral(in); got != want {
			t.Errorf("sqlQuoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

// The profile store is wired to the enforcing chokepoint, whose by-id reads are
// fail-closed: a context with no predicate is refused rather than served
// unfiltered. A profile read legitimately has no principal, so the store must
// seed the explicit kernel bypass itself — otherwise every Gatekeeper profile
// lookup would fail the moment the store stopped holding the raw adapter.
func TestGetProfile_SeedsKernelScopeThroughEnforcingStore(t *testing.T) {
	enforcing := authz.NewEnforcingVectorStore(&recordingStore{}, nil)
	store := NewProfileStore(enforcing)

	// context.Background() carries no predicate at all — the exact condition
	// EnforcingVectorStore refuses.
	if _, err := store.GetProfile(context.Background(), "agent-a", "hash-a"); err != nil {
		t.Fatalf("GetProfile through the enforcing store: %v", err)
	}
	if _, err := store.HasInterviewVector(context.Background(), "agent-a", "hash-a"); err != nil {
		t.Fatalf("HasInterviewVector through the enforcing store: %v", err)
	}
}

// kernelRead fills in the absence of a predicate; it never overrides one the
// caller already resolved.
func TestKernelRead_DoesNotOverrideAnExistingScope(t *testing.T) {
	caller := &domain.TagPredicate{ForbiddenTags: []string{"secrets"}}
	ctx := domain.WithScope(context.Background(), caller)

	got, ok := domain.ScopeFromContext(kernelRead(ctx))
	if !ok {
		t.Fatal("expected a predicate on the returned context")
	}
	if got != caller {
		t.Errorf("kernelRead replaced the caller's predicate: got %+v, want %+v", got, caller)
	}

	// With no predicate, it supplies the system bypass.
	seeded, ok := domain.ScopeFromContext(kernelRead(context.Background()))
	if !ok || seeded == nil || !seeded.Bypass {
		t.Errorf("expected kernelRead to seed a bypass predicate, got %+v (ok=%v)", seeded, ok)
	}
}

// A judicial record's id is derived from its content, so writing the same critique
// twice is idempotent. The previous clock-based id made a retry produce a second
// row saying the same thing.
func TestJudicialRecordID_IsDeterministicAndContentAddressed(t *testing.T) {
	md := map[string]interface{}{"agent_id": "agent-a", "source_hash": "hash-1"}

	a := judicialRecordID("the plan skipped verification", md)
	b := judicialRecordID("the plan skipped verification", md)
	if a != b {
		t.Errorf("same critique produced different ids: %q vs %q", a, b)
	}

	// Different critique text ⇒ different row.
	if c := judicialRecordID("a different critique", md); c == a {
		t.Error("different critique text must not collide")
	}
	// Same text about a different agent ⇒ different row.
	other := map[string]interface{}{"agent_id": "agent-b", "source_hash": "hash-1"}
	if c := judicialRecordID("the plan skipped verification", other); c == a {
		t.Error("same critique about a different agent must not collide")
	}
	// Field-boundary collision: ("ab","c") must not equal ("a","bc").
	x := judicialRecordID("t", map[string]interface{}{"agent_id": "ab", "source_hash": "c"})
	y := judicialRecordID("t", map[string]interface{}{"agent_id": "a", "source_hash": "bc"})
	if x == y {
		t.Error("field boundaries are ambiguous; separator is not doing its job")
	}
	// Missing metadata must not panic.
	if got := judicialRecordID("t", nil); got == "" {
		t.Error("nil metadata should still yield an id")
	}
}

// Two saves of the same critique must reuse one id, so the store overwrites
// rather than accumulating duplicates.
func TestSaveJudicialRecord_RetryIsIdempotent(t *testing.T) {
	rec := &idCapturingStore{}
	store := NewProfileStore(rec)
	md := map[string]interface{}{"agent_id": "agent-a", "source_hash": "hash-1"}

	for range 2 {
		if err := store.Save(context.Background(), "same critique", []float32{0.1}, md); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	if len(rec.ids) != 2 {
		t.Fatalf("expected 2 Save calls, got %d", len(rec.ids))
	}
	if rec.ids[0] != rec.ids[1] {
		t.Errorf("a retry wrote a second row: %q then %q", rec.ids[0], rec.ids[1])
	}
}

type idCapturingStore struct {
	stubVectorStore
	ids []string
}

func (s *idCapturingStore) Save(_ context.Context, doc *domain.Document) error {
	s.ids = append(s.ids, doc.ID)
	return nil
}
