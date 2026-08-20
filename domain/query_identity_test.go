package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The identity plane's two shapes and the alias flag (five-planes step 2;
// FIVE-PLANES-BUILD.md). Pure AST properties, proved without a database — the
// executor's behaviour is a separate file's problem.

func TestKnowledgeQuery_IdentityShapesValidate(t *testing.T) {
	t0 := time.Now()
	valid := []KnowledgeQuery{
		{Kind: QueryEntity, EntityID: "customer/C-1042"},
		{Kind: QueryEntity, EntityID: "customer/C-1042", ExpandAliases: true},
		{Kind: QueryEntity, EntityID: "customer/C-1042", Hops: MaxTraverseHops},
		{Kind: QueryWhy, EntityID: "event:ev-1"},
		{Kind: QueryWhy, EntityID: "customer/C-1042"},
		{Kind: QueryWhy, EntityID: "decision:d-1", Hops: 3},
		{Kind: QueryPoint, EntityID: "e", Predicate: "p", ExpandAliases: true},
		{Kind: QueryTraverse, EntityID: "e", Hops: 1, From: t0, To: t0.Add(time.Hour), ExpandAliases: true},
	}
	for _, q := range valid {
		if err := q.Validate(); err != nil {
			t.Fatalf("valid %s refused: %v", q.Kind, err)
		}
	}
}

func TestKnowledgeQuery_IdentityRefusalsAreTypedAndNamed(t *testing.T) {
	t0 := time.Now()
	cases := map[string]KnowledgeQuery{
		"entity without id":  {Kind: QueryEntity},
		"why without ref":    {Kind: QueryWhy},
		"why over hop cap":   {Kind: QueryWhy, EntityID: "event:ev-1", Hops: MaxTraverseHops + 1},
		"why negative hops":  {Kind: QueryWhy, EntityID: "event:ev-1", Hops: -1},
		"entity over depth":  {Kind: QueryEntity, EntityID: "customer/C-1", Hops: ClosureMaxDepth + 1},
		"expand on as_of":    {Kind: QueryAsOf, ItemKind: "commitment", AsOf: t0, ExpandAliases: true},
		"expand on current":  {Kind: QueryCurrent, ItemKind: "policy_statement", ExpandAliases: true},
		"expand on evidence": {Kind: QueryEvidence, EvidenceID: "ev_x", ExpandAliases: true},
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

// A flag that is silently ignored is worse than one that is refused: the caller
// believes it asked a wider question and reads the narrower answer as the whole
// of it. Every kind therefore either honours ExpandAliases or refuses it, and
// this test is what stops a tenth kind arriving that does neither.
func TestKnowledgeQuery_ExpandAliasesIsHonouredOrRefused(t *testing.T) {
	all := []string{
		QueryPoint, QueryHistory, QueryAsOf, QueryCurrent, QueryContradictions,
		QueryAggregate, QueryEvents, QueryTraverse, QueryEvidence, QueryEntity, QueryWhy,
	}
	t0 := time.Now()
	// Every kind is asked with the flag set; the ones that cannot honour it must
	// REFUSE, typed. Filled to be otherwise-valid so a refusal can only be about
	// the flag.
	filled := map[string]KnowledgeQuery{
		QueryPoint:          {EntityID: "e", Predicate: "p"},
		QueryHistory:        {EntityID: "e", Predicate: "p", From: t0, To: t0.Add(time.Hour)},
		QueryAsOf:           {ItemKind: "commitment", AsOf: t0},
		QueryCurrent:        {ItemKind: "policy_statement"},
		QueryContradictions: {ItemKind: "commitment"},
		QueryAggregate:      {EntityID: "e", Predicate: "p", Aggregate: "count"},
		QueryEvents:         {EntityID: "e", From: t0, To: t0.Add(time.Hour)},
		QueryTraverse:       {EntityID: "e", Hops: 1, From: t0, To: t0.Add(time.Hour)},
		QueryEvidence:       {EvidenceID: "ev_x"},
		QueryEntity:         {EntityID: "customer/C-1"},
		QueryWhy:            {EntityID: "event:ev-1"},
	}
	for _, kind := range all {
		q := filled[kind]
		q.Kind = kind
		q.ExpandAliases = true
		err := q.Validate()
		if q.SupportsAliasExpansion() {
			if err != nil {
				t.Fatalf("kind %s honours expand_aliases but refused it: %v", kind, err)
			}
			continue
		}
		if !errors.Is(err, ErrCannotExpress) {
			t.Fatalf("kind %s cannot honour expand_aliases and did not refuse it (got %v)", kind, err)
		}
	}
	// And the two lists agree: `why` is a lineage walk from a REF, not a query
	// about an entity's properties, so it does not take the flag.
	if (KnowledgeQuery{Kind: QueryWhy}).SupportsAliasExpansion() {
		t.Fatal("why must not claim to honour expand_aliases")
	}
	if !(KnowledgeQuery{Kind: QueryEntity}).SupportsAliasExpansion() {
		t.Fatal("entity must honour expand_aliases")
	}
}

func TestKnowledgeQuery_IdentityGuaranteesNamed(t *testing.T) {
	for _, kind := range []string{QueryEntity, QueryWhy} {
		q := KnowledgeQuery{Kind: kind}
		if strings.TrimSpace(q.Guarantee()) == "" {
			t.Fatalf("kind %s has no guarantee label", kind)
		}
	}
	// The `why` label must keep saying what a correlation hop is NOT. Reading
	// co-occurrence as cause is the single mistake the mechanism vocabulary
	// exists to prevent, and the guarantee is where a caller is told.
	if !strings.Contains((KnowledgeQuery{Kind: QueryWhy}).Guarantee(), "not causal") {
		t.Fatal("why guarantee no longer disclaims causation")
	}
}

func TestClosureNote_OnlyWidensWhenItWidened(t *testing.T) {
	if ClosureNote(0) != "" || ClosureNote(-1) != "" {
		t.Fatal("an unwidened answer must not claim aliases")
	}
	if !strings.Contains(ClosureNote(3), "3 confirmed aliases") {
		t.Fatalf("closure note must state the count, got %q", ClosureNote(3))
	}
}

// The guards refuse rather than truncate, and they say which guard bit. An
// operator handed "too wide" with no figure has nothing to act on, and the fix
// for a set breach (retract a bad link) is a different act from the fix for a
// depth breach (ask for fewer hops).
func TestClosureRefusalsNameTheirGuard(t *testing.T) {
	set := ClosureSetRefusal(ClosureMaxEntities + 1)
	if !errors.Is(set, ErrCannotExpress) {
		t.Fatalf("set refusal must be typed, got %v", set)
	}
	if !strings.Contains(set.Error(), "9") || !strings.Contains(set.Error(), "8") {
		t.Fatalf("set refusal must name the reach and the cap, got %q", set)
	}
	depth := ClosureDepthRefusal(ClosureMaxDepth + 1)
	if !errors.Is(depth, ErrCannotExpress) {
		t.Fatalf("depth refusal must be typed, got %v", depth)
	}
	if !strings.Contains(depth.Error(), "depth") {
		t.Fatalf("depth refusal must name the guard, got %q", depth)
	}
}

// The hard depth cap is ALIGNED with traversal's, not merely similar: two
// bounded walks over the same store with two different limits is a pair somebody
// eventually has to reconcile.
func TestClosureDepthAlignsWithTraversal(t *testing.T) {
	if ClosureMaxDepth != MaxTraverseHops {
		t.Fatalf("closure depth cap %d drifted from MaxTraverseHops %d", ClosureMaxDepth, MaxTraverseHops)
	}
	if ClosureDefaultDepth > ClosureMaxDepth {
		t.Fatal("the default depth cannot exceed the hard cap")
	}
}
