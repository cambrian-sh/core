package domain

import (
	"context"
	"errors"
	"time"
)

// The objects query and GetObject (ADR-0149 D6, D7): the read side of the
// per-kind object tables (objects_<kind>, ADR-0149 D3). One closed request type,
// one result type, one port; internal/objects is the only implementation.
//
// Every value in a request is typed (StatementValue) and every name is checked
// against the live declarations before anything runs: an unknown kind,
// property, link or operator, a literal of the wrong type, or a request over a
// bound is refused with ErrObjectsInvalid. Caller strings never reach an SQL
// identifier position.
//
// Access is per PROPERTY (ADR-0149 D3): for a caller, a property value whose
// visibility companions its predicate cannot read is exactly an absent value —
// it is not returned, matches no filter except is_null, sorts last, groups
// under the empty key and feeds no metric. An object the caller may not know
// exists (ADR-0128 D12) is not listed and reads as not found.

// ErrObjectsInvalid refuses a request the declarations or the bounds do not
// admit (gRPC INVALID_ARGUMENT, Google AIP-160). Nothing ran.
var ErrObjectsInvalid = errors.New("invalid objects request")

// ErrObjectNotFound is GetObject's answer for an id that is not an object, an
// object the caller may not know exists, and an id that never existed — the
// same answer for all three (ADR-0128 D12).
var ErrObjectNotFound = errors.New("object not found")

// Operators of an ObjectCondition (ADR-0149 D6). Which apply to which value
// type is the validator's table: = != < <= > >= in for numbers and dates;
// = != in prefix for text; = in for entity refs; is_null for every type.
const (
	ObjectOpEq     = "eq"
	ObjectOpNe     = "ne"
	ObjectOpLt     = "lt"
	ObjectOpLe     = "le"
	ObjectOpGt     = "gt"
	ObjectOpGe     = "ge"
	ObjectOpIn     = "in"
	ObjectOpPrefix = "prefix"
	ObjectOpIsNull = "is_null"
)

// Metric functions of an objects request.
const (
	ObjectMetricCount = "count"
	ObjectMetricSum   = "sum"
	ObjectMetricAvg   = "avg"
	ObjectMetricMin   = "min"
	ObjectMetricMax   = "max"
)

// Bounds of an objects request (ADR-0149 D6 principle 6: checked before
// anything runs; a request over one is refused, never truncated silently).
const (
	ObjectsDefaultPageSize = 50
	ObjectsMaxPageSize     = 200
	ObjectsMaxLinkHops     = 2
	ObjectsMaxGroups       = 1000
	ObjectsMaxConditions   = 16
	ObjectsMaxInValues     = 100
	ObjectsMaxLinkFilters  = 4
	ObjectsMaxOrderBy      = 3
	ObjectsMaxGroupBy      = 2
	ObjectsMaxMetrics      = 8
)

// ObjectsQuery is the closed objects request (ADR-0149 D6). A request with
// GroupBy or Metrics is a GROUP request (buckets, no paging); any other is a
// LIST request (rows, keyset paging).
type ObjectsQuery struct {
	// Namespace "" = the default namespace.
	Namespace string
	// Kind is a declared kind with an object table (KindSpec.Table).
	Kind string
	// Where conditions are ANDed.
	Where []ObjectCondition
	// Links are filters over confirmed links from the object (≤ 2 hops), ANDed.
	Links []ObjectLinkFilter
	// OrderBy sorts a list; the entity id is always the final, ascending key.
	// Absent values sort last in either direction.
	OrderBy []ObjectOrder
	// GroupBy buckets by these properties (absent values form one bucket).
	GroupBy []string
	// Metrics are computed per bucket (or once, with no GroupBy: totals).
	Metrics []ObjectMetric
	// Fields masks a list's returned properties; empty = every declared one.
	Fields []string
	// PageSize 0 = ObjectsDefaultPageSize; at most ObjectsMaxPageSize.
	PageSize int
	// PageToken continues a list; it is bound to the request it came from and
	// refused under changed parameters (AIP-158). Only PageSize may change.
	PageToken string
}

// IsGroup reports whether q asks for buckets rather than rows.
func (q ObjectsQuery) IsGroup() bool { return len(q.GroupBy) > 0 || len(q.Metrics) > 0 }

// ObjectCondition is one typed test of one property. Values holds one literal
// (none for is_null; one or more for in), each of the property's declared type.
type ObjectCondition struct {
	Property string
	Op       string
	Values   []StatementValue
}

// ObjectLinkFilter keeps objects that reach, over confirmed links of the
// declared relations in Path (followed from → to, 1 or 2 hops), an object
// satisfying Where.
type ObjectLinkFilter struct {
	Path  []string
	Where ObjectCondition
}

// ObjectOrder is one sort key.
type ObjectOrder struct {
	Property string
	Desc     bool
}

// ObjectMetric is count (no Property) or sum|avg (number) / min|max (number,
// date) of a property.
type ObjectMetric struct {
	Fn       string
	Property string
}

// ObjectValue is one property's current value as the caller may see it.
// Value.Type "" means absent (a group key or metric with no value).
type ObjectValue struct {
	Property string
	Value    StatementValue
}

// ObjectRow is one listed object: its visible values among the requested fields.
type ObjectRow struct {
	EntityID string
	Values   []ObjectValue
}

// ObjectGroup is one bucket: its key (one value per GroupBy property, absent =
// Type "") and one value per metric (count and sum/avg are numbers, min/max the
// property's type; absent when no visible value fed it).
type ObjectGroup struct {
	Key     []ObjectValue
	Metrics []StatementValue
}

// ObjectsResult answers an ObjectsQuery: Rows and NextPageToken for a list,
// Groups for a group request. Truncated: more than ObjectsMaxGroups buckets
// exist (the first ObjectsMaxGroups by key are returned).
type ObjectsResult struct {
	Kind string
	// DeclVersion is the declarations snapshot version the request was
	// validated and planned against (ADR-0149 D2).
	DeclVersion   int64
	Rows          []ObjectRow
	NextPageToken string
	Groups        []ObjectGroup
	Truncated     bool
}

// ObjectProperty is one property on an object page (D7): the current value
// with its provenance. Alternatives are other sources' current values the
// caller may read that differ from it (ADR-0149 D4: losing values are kept and
// shown); Conflict is len(Alternatives) > 0 — a disagreement the caller cannot
// see is not reported.
type ObjectProperty struct {
	Property     string
	Value        StatementValue
	At           time.Time
	Source       string
	EvidenceID   string
	Conflict     bool
	Alternatives []ObjectAlternative
}

// ObjectAlternative is another source's current value for a property.
type ObjectAlternative struct {
	Value      StatementValue
	At         time.Time
	Source     string
	EvidenceID string
}

// ObjectLifecycle is the object's lifecycle state (KindLifecycle) and the
// states it may move to next. State "" = no visible state; Allowed are then the
// declared initial states.
type ObjectLifecycle struct {
	StatePredicate string
	State          string
	Allowed        []string
}

// Link directions on an object page.
const (
	ObjectLinkOut = "out"
	ObjectLinkIn  = "in"
)

// ObjectLink is one confirmed link touching the object, typed by its relation's
// declaration (ADR-0149 D5). Label is the relation for an outgoing link and its
// declared Inverse (when there is one) for an incoming one.
type ObjectLink struct {
	LinkID     string
	Relation   string
	Label      string
	Family     string
	Direction  string
	OtherRef   string
	OtherKind  string
	EvidenceID string
	AssertedAt time.Time
	ValidFrom  *time.Time
	ValidTo    *time.Time
}

// Timeline entry kinds.
const (
	ObjectTimelineEvent    = "event"
	ObjectTimelineProperty = "property"
)

// ObjectTimelineEntry is one entry of the object's recent timeline: an event it
// took part in, or a recorded value of one of its properties.
type ObjectTimelineEntry struct {
	Kind       string
	At         time.Time
	EventID    string
	EventType  string
	Roles      map[string]string
	Property   string
	Value      StatementValue
	EvidenceID string
}

// ObjectView is GetObject's answer (ADR-0149 D7): everything the caller may see
// about one object, in one call. Nothing the caller cannot see is present —
// omitted, never redacted. Derived properties (L5) and available actions (L4)
// have no section until those layers land.
type ObjectView struct {
	EntityID  string
	Kind      string
	Namespace string
	// CreatedAt and FirstSeenEvidence come from the entity handle; set only
	// when the caller may read the evidence that minted it.
	CreatedAt         *time.Time
	FirstSeenEvidence string
	DeclVersion       int64
	UpdatedAt         time.Time
	Properties        []ObjectProperty
	Lifecycle         *ObjectLifecycle
	// Aliases are confirmed identity links; Links every other confirmed link.
	Aliases  []ObjectLink
	Links    []ObjectLink
	Timeline []ObjectTimelineEntry
	// Evidence lists, once each, the evidence ids the sections above cite.
	Evidence []string
}

// Objects is the plugin seam over the object tables: it runs AS a principal,
// whose read predicate the kernel resolves (never chosen by the caller,
// ADR-0118 D1). A principal with no read predicate is refused with
// ErrQueryDenied.
type Objects interface {
	ListObjects(ctx context.Context, principal PrincipalRef, q ObjectsQuery) (ObjectsResult, error)
	GetObject(ctx context.Context, principal PrincipalRef, namespace, entityID string) (ObjectView, error)
}
