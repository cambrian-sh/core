package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The gate's failure-mode half: everything outside the closed AST refuses
// with ErrCannotExpress and NAMES the reason — never a guess, never SQL.
func TestKnowledgeQuery_RefusalsAreTypedAndNamed(t *testing.T) {
	now := time.Now()
	cases := map[string]KnowledgeQuery{
		"unknown kind":        {Kind: "select_star"},
		"point sans entity":   {Kind: QueryPoint, Predicate: "seen_at"},
		"history sans range":  {Kind: QueryHistory, EntityID: "e", Predicate: "p"},
		"as_of sans time":     {Kind: QueryAsOf, ItemKind: "commitment"},
		"unknown aggregate":   {Kind: QueryAggregate, EntityID: "e", Predicate: "p", Aggregate: "median"},
		"unbounded traversal": {Kind: QueryTraverse, EntityID: "e", Hops: 9, From: now, To: now},
		"zero-hop traversal":  {Kind: QueryTraverse, EntityID: "e", Hops: 0, From: now, To: now},
		"oversized limit":     {Kind: QueryPoint, EntityID: "e", Predicate: "p", Limit: 100000},
		"evidence sans id":    {Kind: QueryEvidence},
	}
	for name, q := range cases {
		err := q.Validate()
		if !errors.Is(err, ErrCannotExpress) {
			t.Fatalf("%s: want ErrCannotExpress, got %v", name, err)
		}
		if len(err.Error()) < len(ErrCannotExpress.Error())+5 {
			t.Fatalf("%s: refusal must NAME the reason, got %q", name, err)
		}
	}
}

func TestKnowledgeQuery_ValidShapesPass(t *testing.T) {
	now := time.Now()
	valid := []KnowledgeQuery{
		{Kind: QueryPoint, EntityID: "e", Predicate: "p"},
		{Kind: QueryHistory, EntityID: "e", Predicate: "p", From: now.Add(-time.Hour), To: now},
		{Kind: QueryAsOf, ItemKind: "commitment", AsOf: now},
		{Kind: QueryCurrent, ItemKind: "policy_statement"},
		{Kind: QueryContradictions, ItemKind: "commitment"},
		{Kind: QueryAggregate, EntityID: "e", Predicate: "p", Aggregate: "avg"},
		{Kind: QueryEvents, EntityID: "e", From: now.Add(-time.Hour), To: now},
		{Kind: QueryTraverse, EntityID: "e", Hops: 2, From: now.Add(-time.Hour), To: now},
		{Kind: QueryEvidence, EvidenceID: "ev_x"},
	}
	for _, q := range valid {
		if err := q.Validate(); err != nil {
			t.Fatalf("valid %s refused: %v", q.Kind, err)
		}
	}
}

// Every kind states its §14 guarantee — an answer that cannot say what it
// guarantees invites being quoted as truth.
func TestKnowledgeQuery_GuaranteesNamed(t *testing.T) {
	for _, kind := range []string{QueryPoint, QueryAsOf, QueryContradictions, QueryTraverse} {
		q := KnowledgeQuery{Kind: kind}
		if strings.TrimSpace(q.Guarantee()) == "" {
			t.Fatalf("kind %s has no guarantee label", kind)
		}
	}
}
