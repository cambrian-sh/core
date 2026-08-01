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
)

// MaxTraverseHops bounds traversal — high-fan-out boundedness is a §17
// contract row, and an unbounded graph walk is exactly the query shape that
// melts a store.
const MaxTraverseHops = 3

// MaxQueryLimit bounds every result set.
const MaxQueryLimit = 1000

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
}

// QueryPlane executes validated queries. Implementations MUST call Validate
// first and refuse on error — an invalid query never half-executes.
type QueryPlane interface {
	Query(ctx context.Context, q KnowledgeQuery) (QueryResult, error)
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
	default:
		return cannot("unknown query kind %q", q.Kind)
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
	default:
		return "exact over stored data; identity and coverage epistemically uncertain"
	}
}
