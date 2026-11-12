package domain

import (
	"context"
	"time"
)

// Event is one n-ary OCCURRENCE — something that happened at a time, with
// typed participants (ADR-0108; memo §7B). Participants are Roles with real
// entity edges, never keys inside a payload: "hop distance to a sanctioned
// wallet" is only computable when the receiver is a row.
type Event struct {
	ID          EventID
	NamespaceID string
	// Type names the occurrence kind ("gate_passage", "transfer"). Data — the
	// kernel never branches on a value.
	Type       string
	OccurredAt time.Time
	EvidenceID EvidenceID
	// SourceRef is the source-native reference; with the namespace it is the
	// idempotency key.
	SourceRef string
	Roles     []EventRole
	// TimeBasis says which clock OccurredAt came from (ADR-0147 D8):
	// TimeBasisEvent, TimeBasisReceived, or "" when the writer did not say.
	TimeBasis string
}

// EventID identifies one event.
type EventID string

// EventRole is one participant edge: HOW (Role) an entity took part.
type EventRole struct {
	Role     string
	EntityID string
}

// Observation is one entity/predicate/value reading at a time — the substrate's
// high-volume shape (memo §7A/§7E). A raw observation is a partitioned row and
// deliberately NOT a KnowledgeItem: promotion to the semantic layers is a
// separate, rule-driven lane this type must not imply.
type Observation struct {
	NamespaceID string
	EntityID    string
	Predicate   string
	Value       StatementValue
	// Location is where the source placed the entity, when it said so. A
	// convenience column; a traversable location belongs in an event role.
	Location   string
	OccurredAt time.Time
	Confidence float64
	EvidenceID EvidenceID
	SourceRef  string
	// TimeBasis says which clock OccurredAt came from (ADR-0147 D8):
	// TimeBasisEvent, TimeBasisReceived, or "" when the writer did not say.
	TimeBasis string
}

// Time bases (ADR-0147 D8). Which clock a typed row's occurred_at is.
const (
	// TimeBasisEvent: the source's own event or change time, declared by the
	// writer's mapping.
	TimeBasisEvent = "event"
	// TimeBasisReceived: our receipt time, because the source's mapping
	// declared no event time. Only live deliveries may carry it; a history fill
	// without an event time is refused before anything is written.
	TimeBasisReceived = "received"
)

// Retirement reasons (ADR-0147 D6, D11). Why a typed row stopped counting.
const (
	// RetireSuperseded: a mapping repair produced a newer revision of the row.
	RetireSuperseded = "superseded"
	// RetireSourceDeleted: the source record the row came from was deleted.
	RetireSourceDeleted = "source_deleted"
)

// EventStore persists events and observations and answers the two question
// shapes this phase gates on (memo §14):
//
//	Point lookup — "where was that car last seen?"  → exact latest STORED row
//	History      — "everywhere it went yesterday"   → exact range over STORED rows
//
// Both are plain SQL over typed rows — no embedding, no model, no ranking.
// The guarantee is exact-over-stored-data; the identity behind an entity id
// remains epistemically uncertain, and nothing here claims world truth.
type EventStore interface {
	// RecordEvent appends one event with its roles, atomically. Idempotent on
	// (namespace, source_ref) when SourceRef is set: a replayed delivery
	// returns the existing id with inserted=false and writes nothing.
	RecordEvent(ctx context.Context, ev Event) (id EventID, inserted bool, err error)

	// RecordObservation appends one observation. Idempotent on
	// (namespace, source_ref) when SourceRef is set.
	RecordObservation(ctx context.Context, ob Observation) (inserted bool, err error)

	// PointLookup returns the LATEST stored observation for the entity and
	// predicate, or nil when none exists — "no stored observation" is an
	// answer, never an error.
	PointLookup(ctx context.Context, namespace, entityID, predicate string) (*Observation, error)

	// History returns the entity's observations for the predicate within
	// [from, to), oldest first.
	History(ctx context.Context, namespace, entityID, predicate string, from, to time.Time) ([]Observation, error)

	// EventsForEntity returns events in which the entity held ANY role within
	// [from, to), oldest first, roles hydrated — the traversal §7B exists for.
	EventsForEntity(ctx context.Context, namespace, entityID string, from, to time.Time) ([]Event, error)

	// RetireRows records that the typed rows written under sourceRefs no longer
	// count (ADR-0147 D6): every read above, and the query plane, skip them.
	// A record beside the rows, never a delete or an edit of them — the rows
	// stay stored. Idempotent: an already-retired ref keeps its first record.
	// reason is RetireSuperseded or RetireSourceDeleted; retiredBy names what
	// retired them ("mapping_revision:3", a tombstone evidence id). Returns how
	// many refs this call newly retired.
	RetireRows(ctx context.Context, namespace string, sourceRefs []string, reason, retiredBy string) (int, error)

	// ReinstateRows lifts a retirement, for a repair rolled back to the
	// mapping revision that wrote the rows. Refs that are not retired are
	// ignored. Returns how many refs this call reinstated.
	ReinstateRows(ctx context.Context, namespace string, sourceRefs []string) (int, error)
}

// EvidenceTransformer is the transformation stage's add-many seam (ADR-0108
// D3): the outbox consumer hands each evidence row — bytes included — to every
// registered transformer, AFTER the archive write and never before (the two
// refusals, memo §10). Delivery is at-least-once, so a transformer must be
// replay-safe; the typed stores' source_ref idempotency makes that structural
// rather than each transformer's private discipline.
//
// A transformer that does not recognise the evidence returns (false, nil) —
// "not mine" and "mine but failed" must never look the same, because the
// second leaves the outbox item pending for retry and the first must not.
type EvidenceTransformer interface {
	// Name identifies the transformer in logs and metrics.
	Name() string
	// Transform processes one evidence delivery. handled=false means the
	// evidence is not this transformer's shape.
	Transform(ctx context.Context, ev Evidence, content []byte) (handled bool, err error)
}
