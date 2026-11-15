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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, kind := range []string{QueryPoint, QueryAsOf, QueryContradictions, QueryTraverse} {
		q := KnowledgeQuery{Kind: kind}
		if strings.TrimSpace(q.Guarantee()) == "" {
			t.Fatalf("kind %s has no guarantee label", kind)
		}
	}
}

// Policy and Actor select resolution rows (ADR-0111, owner amendment
// 2026-11-12): honoured by current/as_of/contradictions, refused everywhere
// else — and "*" is a row family, not an actor.
func TestValidate_BeliefSelectorsAreRefusedWhereTheyCannotApply(t *testing.T) {
	now := time.Now()
	for name, q := range map[string]KnowledgeQuery{
		"point actor":     {Kind: QueryPoint, EntityID: "e", Predicate: "p", Actor: "a"},
		"history policy":  {Kind: QueryHistory, EntityID: "e", Predicate: "p", From: now, To: now, Policy: "latest_assertion"},
		"aggregate actor": {Kind: QueryAggregate, EntityID: "e", Predicate: "p", Aggregate: "count", Actor: "a"},
		"entity policy":   {Kind: QueryEntity, EntityID: "e", Policy: "x"},
		"why actor":       {Kind: QueryWhy, EntityID: "e", Actor: "a"},
		"current star":    {Kind: QueryCurrent, ItemKind: "k", Actor: ResolutionActorAll},
	} {
		if err := q.Validate(); !errors.Is(err, ErrCannotExpress) {
			t.Errorf("%s: Validate = %v, want ErrCannotExpress", name, err)
		}
	}
	for name, q := range map[string]KnowledgeQuery{
		"current actor+policy":  {Kind: QueryCurrent, ItemKind: "k", Actor: "a", Policy: "latest_assertion"},
		"as_of actor":           {Kind: QueryAsOf, ItemKind: "k", AsOf: now, Actor: "a"},
		"contradictions policy": {Kind: QueryContradictions, ItemKind: "k", Policy: "designated_actor"},
	} {
		if err := q.Validate(); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
	if got := (KnowledgeQuery{}).PolicyOrDefault(); got != ResolutionPolicyLatestAssertion {
		t.Errorf("PolicyOrDefault() = %q", got)
	}
}

// ENG-1045: include_retired is expressible only on the shapes that read
// retirable typed rows.
func TestValidate_IncludeRetiredOnlyOnRetirableShapes(t *testing.T) {
	from, to := time.Unix(0, 0), time.Unix(100, 0)
	ok := []KnowledgeQuery{
		{Kind: QueryPoint, EntityID: "e/1", Predicate: "p", IncludeRetired: true},
		{Kind: QueryHistory, EntityID: "e/1", Predicate: "p", From: from, To: to, IncludeRetired: true},
		{Kind: QueryEvents, EntityID: "e/1", From: from, To: to, IncludeRetired: true},
	}
	for _, q := range ok {
		if err := q.Validate(); err != nil {
			t.Errorf("%s with include_retired refused: %v", q.Kind, err)
		}
	}
	bad := []KnowledgeQuery{
		{Kind: QueryAsOf, ItemKind: "k", AsOf: to, IncludeRetired: true},
		{Kind: QueryAggregate, EntityID: "e/1", Predicate: "p", Aggregate: "count", IncludeRetired: true},
		{Kind: QueryCurrent, ItemKind: "k", IncludeRetired: true},
	}
	for _, q := range bad {
		if err := q.Validate(); !errors.Is(err, ErrCannotExpress) {
			t.Errorf("%s with include_retired: err = %v, want a refusal", q.Kind, err)
		}
	}
}
