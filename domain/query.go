package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCannotExpress is the typed query plane's ONLY failure mode for requests
// outside the closed AST (ADR-0111; memo §14): "a model may form the AST, but
// an unexpressible request returns 'cannot express safely', never invented
// SQL." Every wrap names the specific reason, because an unexplained refusal
// is unappealable.
var ErrCannotExpress = errors.New("cannot express safely")

// Query kinds — the closed set. Open-ended questions are DELIBERATELY absent:
// they belong to the corpus/memory lane, and the two-tier split is the design.
const (
	QueryPoint          = "point"          // latest stored observation for entity+predicate
	QueryHistory        = "history"        // observations in [From, To)
	QueryAsOf           = "as_of"          // resolutions as our records held them at AsOf
	QueryCurrent        = "current"        // current resolutions for an item kind
	QueryContradictions = "contradictions" // entities whose current resolutions disagree across actors
	QueryAggregate      = "aggregate"      // count|avg|min|max over numeric observations
	QueryEvents         = "events"         // events an entity participated in
	QueryTraverse       = "traverse"       // bounded co-participation traversal
	QueryEvidence       = "evidence"       // one evidence row, provenance fields

	// The identity plane's two shapes (five-planes step 2; FIVE-PLANES-BUILD.md).
	// They read LINKS rather than observations, and that is why they are separate
	// kinds rather than flags on the existing ones: an answer assembled from
	// assertions ABOUT an entity carries a different warranty from one assembled
	// from what was observed of it, and §14 exists so the caller reads that
	// warranty instead of inferring it.
	QueryEntity = "entity" // one entity handle, its alias closure, and its links
	QueryWhy    = "why"    // bounded backward walk over lineage, hop by hop
)

// MaxTraverseHops bounds traversal — high-fan-out boundedness is a §17
// contract row, and an unbounded graph walk is exactly the query shape that
// melts a store.
const MaxTraverseHops = 3

// MaxQueryLimit bounds every result set.
const MaxQueryLimit = 1000

// Closure guards (five-planes step 2; FIVE-PLANES-BUILD.md "Closure guard
// constants"). Alias expansion walks CONFIRMED identity links, and a link is an
// assertion somebody made — a bad crosswalk, a scored producer promoted by a
// careless reviewer, or one genuine merge too many all end the same way: a
// question about one customer silently answered over a hundred. These four
// numbers are where that stops.
//
// They are deliberately small. The point of the plane is that identity is
// REVIEWED, and a closure that needs more than a handful of hops or members is
// not a reviewed identity — it is a merge nobody has looked at.
const (
	// ClosureDefaultDepth is how far an expansion walks when the caller says
	// nothing. Two hops covers "A is B, and B is C" — the transitive case a
	// crosswalk actually produces — without becoming a graph walk.
	ClosureDefaultDepth = 2
	// ClosureMaxDepth is the hard ceiling, aligned with MaxTraverseHops on
	// purpose: two bounded walks over the same store with two different limits is
	// a pair somebody will eventually have to reconcile.
	ClosureMaxDepth = MaxTraverseHops
	// ClosureMaxEntities caps the SET. Exceeding it REFUSES the query loudly
	// rather than truncating: a silently trimmed closure answers a different
	// question from the one asked, and answers it without saying so.
	ClosureMaxEntities = 8
	// ClosureMaxLinksPerEntityPerMechanism caps fan-out from ONE entity under ONE
	// mechanism. Exceeding it FLAGS that entity and excludes it from expansion —
	// a single id claimed same_as sixteen others by one producer is a producer
	// fault, and following it would spend the set cap on one bad row.
	ClosureMaxLinksPerEntityPerMechanism = 16
)

// KnowledgeQuery is the closed AST. One struct, one Validate, no dialects.
type KnowledgeQuery struct {
	Kind       string
	EntityID   string
	Predicate  string
	ItemKind   string
	Policy     string
	Actor      string
	AsOf       time.Time
	From, To   time.Time
	Aggregate  string // count | avg | min | max
	Hops       int
	Limit      int
	EvidenceID string
	// ExpandAliases widens the SUBJECT of an entity-scoped op across the
	// entity's confirmed identity closure (D-W2-3; closure verbs come from the
	// RelationRegistry, never from a name in this package).
	//
	// It widens the subject and NOTHING else. Every row the widened query
	// returns is still authorized against that row's OWN classification and
	// parties, so a confirmed link can add rows a caller may read — it can never
	// add rows a caller may not. See the executor's SCOPE RULE comment.
	ExpandAliases bool
}

// SupportsAliasExpansion reports whether a kind is entity-scoped, i.e. whether
// ExpandAliases means anything for it. Exported because the executor asks the
// same question the validator does, and two copies of this list would drift into
// a flag that validates on one side and is ignored on the other.
func (q KnowledgeQuery) SupportsAliasExpansion() bool {
	switch q.Kind {
	case QueryPoint, QueryHistory, QueryAggregate, QueryEvents, QueryTraverse, QueryEntity:
		return true
	}
	return false
}

// QueryRow is one result row. Keys are shape-specific and documented by the
// executor; the closed INPUT is the safety property.
type QueryRow map[string]any

// QueryResult carries rows plus the §14 guarantee label for the kind, so no
// caller can quote a point lookup as world truth without deleting words the
// API handed them.
type QueryResult struct {
	Guarantee string
	Rows      []QueryRow
	// ClosureSize is how many ALIASES beyond the subject the answer was drawn
	// over — 0 for every unexpanded query. It is reported rather than left
	// implicit because "we also counted four other ids we believe are this one"
	// changes what the number means, and the Guarantee string says so in words
	// (see ClosureNote) only because this field says so in a number.
	ClosureSize int
}

// ClosureNote is the suffix a widened answer appends to its guarantee. Empty for
// an unwidened one, so the ordinary label is untouched.
func ClosureNote(aliases int) string {
	if aliases <= 0 {
		return ""
	}
	return fmt.Sprintf("; across %d confirmed aliases", aliases)
}

// ClosureSetRefusal is the typed refusal for the set cap. It names the guard and
// the number reached, because "too wide" with no figure gives an operator
// nothing to act on — and because the fix (retract a bad link) is a different
// act from the fix for depth (ask for fewer hops).
func ClosureSetRefusal(reached int) error {
	return cannot("alias closure reached %d entities, above the %d-entity cap — refused rather "+
		"than truncated, because a trimmed closure answers a different question without saying so",
		reached, ClosureMaxEntities)
}

// ClosureDepthRefusal is the typed refusal for the depth guard.
func ClosureDepthRefusal(asked int) error {
	return cannot("alias closure depth %d outside [1, %d] — an unbounded identity walk is not expressible",
		asked, ClosureMaxDepth)
}

// ErrQueryScopeMissing is returned when a query reaches the plane with a nil
// scope predicate. Scope is a REQUIRED positional parameter (ADR-0118 D1, the
// records-lane discipline): the 2026-07-27 records audit found unscoped call
// sites only because making scope required broke them at compile time. Nil
// DENIES; unrestricted is said explicitly with a bypass predicate.
var ErrQueryScopeMissing = errors.New("knowledge query scope missing: nil predicate denies")

// ErrQueryDenied is returned by the principal-resolving seam when the
// authorizer grants the caller no read predicate at all (fail closed).
var ErrQueryDenied = errors.New("knowledge query not authorized for principal")

// QueryPlane executes validated queries. Implementations MUST call Validate
// first and refuse on error — an invalid query never half-executes — and MUST
// enforce the scope predicate on every row (ADR-0118 D2): nil scope refuses
// with ErrQueryScopeMissing, a bypass predicate reads unrestricted, anything
// else filters by the classification the row's provenance carries.
type QueryPlane interface {
	Query(ctx context.Context, q KnowledgeQuery, scope *TagPredicate) (QueryResult, error)
}

func cannot(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ErrCannotExpress)...)
}

// Validate refuses everything outside the closed set, naming the reason.
func (q KnowledgeQuery) Validate() error {
	if q.Limit < 0 || q.Limit > MaxQueryLimit {
		return cannot("limit %d outside [0, %d]", q.Limit, MaxQueryLimit)
	}
	switch q.Kind {
	case QueryPoint:
		if q.EntityID == "" || q.Predicate == "" {
			return cannot("point lookup needs entity_id and predicate")
		}
	case QueryHistory:
		if q.EntityID == "" || q.Predicate == "" || q.From.IsZero() || q.To.IsZero() {
			return cannot("history needs entity_id, predicate, from and to")
		}
	case QueryAsOf:
		if q.ItemKind == "" || q.AsOf.IsZero() {
			return cannot("as_of needs item_kind and as_of")
		}
	case QueryCurrent:
		if q.ItemKind == "" {
			return cannot("current needs item_kind")
		}
	case QueryContradictions:
		if q.ItemKind == "" {
			return cannot("contradictions needs item_kind")
		}
	case QueryAggregate:
		if q.EntityID == "" || q.Predicate == "" {
			return cannot("aggregate needs entity_id and predicate")
		}
		switch q.Aggregate {
		case "count", "avg", "min", "max":
		default:
			return cannot("unknown aggregate %q (count|avg|min|max)", q.Aggregate)
		}
	case QueryEvents:
		if q.EntityID == "" || q.From.IsZero() || q.To.IsZero() {
			return cannot("events needs entity_id, from and to")
		}
	case QueryTraverse:
		if q.EntityID == "" {
			return cannot("traverse needs entity_id")
		}
		if q.Hops < 1 || q.Hops > MaxTraverseHops {
			return cannot("hops %d outside [1, %d] — unbounded traversal is not expressible", q.Hops, MaxTraverseHops)
		}
		if q.From.IsZero() || q.To.IsZero() {
			return cannot("traverse needs from and to")
		}
	case QueryEvidence:
		if q.EvidenceID == "" {
			return cannot("evidence inspection needs evidence_id")
		}
	case QueryEntity:
		if q.EntityID == "" {
			return cannot("entity lookup needs entity_id")
		}
		// Hops is the CLOSURE depth for this shape (0 = ClosureDefaultDepth). An
		// over-cap ask is refused rather than silently capped: a caller who asked
		// for five hops and got three would read a partial closure as the whole
		// of it, which is the same failure the set cap refuses to commit.
		if q.Hops < 0 || q.Hops > ClosureMaxDepth {
			return ClosureDepthRefusal(q.Hops)
		}
	case QueryWhy:
		// A typed ref ("event:e-1", "entity:customer/C-1042") or a bare entity
		// id, which the executor promotes to an entity ref. Both are accepted
		// because the two callers differ: a console walks back from an event it
		// is looking at, an agent walks back from the entity it was asked about.
		if q.EntityID == "" {
			return cannot("why needs entity_id as a typed ref (entity:|event:|decision:|evidence:) or a bare entity id")
		}
		// Hops reuses the traversal bound rather than inventing a second one;
		// 0 means "the default depth", which is how every other optional bound
		// in this AST reads.
		if q.Hops < 0 || q.Hops > MaxTraverseHops {
			return cannot("hops %d outside [0, %d] — an unbounded lineage walk is not expressible", q.Hops, MaxTraverseHops)
		}
	default:
		return cannot("unknown query kind %q", q.Kind)
	}
	// A flag that is silently ignored is worse than one that is refused: the
	// caller believes it asked a wider question and reads a narrower answer as
	// the whole of it. Entity-scoped kinds honour ExpandAliases; the rest say so.
	if q.ExpandAliases && !q.SupportsAliasExpansion() {
		return cannot("expand_aliases is not expressible for kind %q — it widens an entity SUBJECT, "+
			"and this shape has none", q.Kind)
	}
	return nil
}

// Guarantee returns the §14 label for a kind (memo: deterministic ≠ exact ≠
// true — the answer states which it is).
func (q KnowledgeQuery) Guarantee() string {
	switch q.Kind {
	case QueryAsOf:
		return "exact and complete — a question about our own records"
	case QueryContradictions:
		return "deterministic over stored interpretations; extraction uncertainty applies"
	case QueryTraverse:
		return "exact over stored edges; entity links epistemically uncertain"
	case QueryEntity:
		// "as asserted" is the whole warranty: the store is exact about WHICH
		// assertions exist and says nothing about whether the equivalence holds.
		return "exact over stored links; identity as asserted, see links"
	case QueryWhy:
		// The second clause is load-bearing. A correlation hop says two things
		// co-occurred and share a participant; read as causation it is exactly
		// the mistake the mechanism vocabulary exists to prevent.
		return "each hop labelled by mechanism; correlation hops are not causal"
	default:
		return "exact over stored data; identity and coverage epistemically uncertain"
	}
}
