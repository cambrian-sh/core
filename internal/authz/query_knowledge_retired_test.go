package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type recordingPlane struct{ calls int }

func (p *recordingPlane) Query(context.Context, domain.KnowledgeQuery, *domain.TagPredicate) (domain.QueryResult, error) {
	p.calls++
	return domain.QueryResult{}, nil
}

// ENG-1045 (owner decision 2026-11-15): retired rows are readable on the
// operator plane only — the system principal it reads as. Every other principal
// is refused before scope resolution and the plane is never reached.
func TestQueryKnowledge_IncludeRetiredIsOperatorPlaneOnly(t *testing.T) {
	plane := &recordingPlane{}
	query := QueryKnowledgeFunc(nil, plane, nil)
	q := domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: "order/1", Predicate: "qty", IncludeRetired: true}
	for _, p := range []domain.PrincipalRef{
		domain.AgentPrincipal("agent-1"),
		{ID: "alice", Kind: domain.PrincipalUser},
		{ID: "d", Kind: domain.PrincipalDaemon},
	} {
		if _, err := query(context.Background(), p, q); !errors.Is(err, domain.ErrRetiredOperatorOnly) || !errors.Is(err, domain.ErrQueryDenied) {
			t.Errorf("%s principal: err = %v, want ErrRetiredOperatorOnly (a query denial)", p.Kind, err)
		}
	}
	if plane.calls != 0 {
		t.Fatalf("a refused retired read reached the plane %d times", plane.calls)
	}
	if _, err := query(context.Background(), domain.SystemPrincipal, q); err != nil {
		t.Fatalf("the operator plane's principal was refused: %v", err)
	}
	q.IncludeRetired = false
	if _, err := query(context.Background(), domain.AgentPrincipal("agent-1"), q); err != nil {
		t.Fatalf("an ordinary agent read was refused: %v", err)
	}
	if plane.calls != 2 {
		t.Fatalf("plane calls = %d, want 2", plane.calls)
	}
}
