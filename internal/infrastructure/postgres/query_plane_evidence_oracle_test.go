package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// oracleEvidenceStore answers Get the way PgEvidenceStore does: a wrapped
// pgx.ErrNoRows for an unknown id, the row otherwise.
type oracleEvidenceStore struct {
	domain.EvidenceStore
	rows map[domain.EvidenceID]*domain.Evidence
}

func (s oracleEvidenceStore) Get(_ context.Context, id domain.EvidenceID) (*domain.Evidence, error) {
	if ev, ok := s.rows[id]; ok {
		return ev, nil
	}
	return nil, fmt.Errorf("evidence get %s: %w", id, pgx.ErrNoRows)
}

// T-20261112-substrate-read-path-leaks-and-gaps-evidence item 1: the evidence shape
// must answer an absent id exactly like a hidden one — no rows, no error — or a scoped
// caller can tell "does not exist" from "exists but hidden from you".
func TestEvidenceRow_AbsentAndHiddenAreIndistinguishable(t *testing.T) {
	p := &PgQueryPlane{evidence: oracleEvidenceStore{rows: map[domain.EvidenceID]*domain.Evidence{
		"ev-secret": {ID: "ev-secret", Classification: []string{"finance"}},
		"ev-open":   {ID: "ev-open", Classification: []string{"ops"}},
	}}}
	scope := &domain.TagPredicate{RequiredTags: []string{"ops"}}
	ctx := context.Background()

	hidden, hErr := p.evidenceRow(ctx, domain.KnowledgeQuery{EvidenceID: "ev-secret"}, scope)
	absent, aErr := p.evidenceRow(ctx, domain.KnowledgeQuery{EvidenceID: "ev-missing"}, scope)
	if hErr != nil || aErr != nil {
		t.Fatalf("errors must not distinguish absent from hidden: hidden=%v absent=%v", hErr, aErr)
	}
	if len(hidden) != 0 || len(absent) != 0 {
		t.Fatalf("both must be empty: hidden=%v absent=%v", hidden, absent)
	}
	visible, err := p.evidenceRow(ctx, domain.KnowledgeQuery{EvidenceID: "ev-open"}, scope)
	if err != nil || len(visible) != 1 || visible[0]["id"] != "ev-open" {
		t.Fatalf("a visible row must still be returned: %v %v", visible, err)
	}
}

// Any other store failure is still an error — only "no such row" is folded.
func TestEvidenceRow_OtherErrorsStillSurface(t *testing.T) {
	p := &PgQueryPlane{evidence: failingEvidenceStore{}}
	if _, err := p.evidenceRow(context.Background(), domain.KnowledgeQuery{EvidenceID: "x"}, &domain.TagPredicate{Bypass: true}); err == nil {
		t.Fatal("a store failure was swallowed")
	}
}

type failingEvidenceStore struct{ domain.EvidenceStore }

func (failingEvidenceStore) Get(context.Context, domain.EvidenceID) (*domain.Evidence, error) {
	return nil, fmt.Errorf("evidence get: connection reset")
}
