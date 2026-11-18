package domain

import (
	"context"
	"time"
)

// SubstrateCatalog is the substrate's NAMES-AND-COUNTS read surface: what the
// store holds (entities, predicates, event types, item kinds, actors, per-source
// and per-kind counts), never a value. It exists beside the closed query AST
// (QueryPlane) because two consumers need it and the AST has no shape for it —
// the operator explore surface (Overview, ListEntities) and the NL AST-former's
// grounding catalog (ADR-0118 explore/ask).
//
// Names are not harmless metadata. ADR-0128 D12 made existence protected
// content, so every method answers ONLY over rows the bound scope may read —
// the same evidence-classification test (owner fence included) the query plane
// applies to the rows those names stand for. A name derived only from rows the
// reader cannot see is absent, and so is every count it would have contributed:
// "hidden" and "never existed" must read the same.
//
// A catalog is BOUND to one scope when it is obtained (PgQueryPlane.Catalog);
// plugins never hold the unbound form. They receive a principal-resolving seam
// (KernelServices.KnowledgeCatalog), so the caller can no more choose its own
// predicate here than on QueryKnowledge.
type SubstrateCatalog interface {
	// Overview reports what the store holds for this reader.
	Overview(ctx context.Context) (SubstrateOverview, error)
	// Entities lists entity handles the reader can reach a row of, newest first.
	Entities(ctx context.Context, f EntityFilter) ([]CatalogEntity, error)
	// Predicates lists distinct observation predicates over readable rows.
	Predicates(ctx context.Context, limit int) ([]string, error)
	// EventTypes lists distinct event types over readable events.
	EventTypes(ctx context.Context, limit int) ([]string, error)
	// ItemKinds lists the kinds of readable items with a live resolution.
	ItemKinds(ctx context.Context, limit int) ([]string, error)
	// Actors lists the actors of readable live resolutions about the given
	// entities (all entities when entityIDs is empty).
	Actors(ctx context.Context, entityIDs []string, limit int) ([]string, error)

	// The three LISTINGS below (T-20261113-link-building-suite-reads-the-kernel-s-postgres,
	// owner decision 2026-11-14) are the reads a black-box client needs to see what
	// the identity plane produced — the links, the minted handles, and which event a
	// source record became — without SQL against the substrate tables. They return
	// rows, not values: refs and verbs, never an observation's value. Each is paged
	// in id order and fenced like everything else here.

	// Links pages a namespace's link assertions in id order. Every row is kept only
	// when the reader may read the evidence it rests on (an assertion with no
	// evidence only for an unfiltered scope) — the filterLinks rule the entity op
	// and the review inbox apply. A page can therefore come back SHORT; its Next
	// cursor is the last row scanned, so paging never skips nor repeats a row.
	Links(ctx context.Context, f LinkListFilter) (LinkPage, error)
	// Events pages readable, unretired events whose source_ref starts with
	// f.SourceRefPrefix (empty = any non-empty source_ref), in id order.
	Events(ctx context.Context, f EventListFilter) (EventPage, error)
	// EntityHandles pages a namespace's minted handles in id order, fenced through
	// each handle's first-seen evidence.
	EntityHandles(ctx context.Context, f EntityHandleFilter) (EntityHandlePage, error)
}

// LinkListFilter narrows SubstrateCatalog.Links. Namespace is required.
type LinkListFilter struct {
	Namespace string
	// States keeps only these states (candidate | confirmed); empty = all.
	States []string
	// ProducerPrefix keeps links whose producer starts with it (literal, not a pattern).
	ProducerPrefix string
	// IncludeRetracted also returns retracted assertions (default: left out).
	IncludeRetracted bool
	// AfterID is the cursor: rows with id > AfterID.
	AfterID string
	Limit   int
}

// LinkPage is one page of links. Next is "" when nothing follows.
type LinkPage struct {
	Links []Link
	Next  string
}

// EventListFilter narrows SubstrateCatalog.Events. Namespace is required.
type EventListFilter struct {
	Namespace       string
	SourceRefPrefix string
	AfterID         string
	Limit           int
}

// CatalogEvent is an occurrence as the event listing reports it.
type CatalogEvent struct {
	ID         string
	EventType  string
	SourceRef  string
	OccurredAt time.Time
}

// EventPage is one page of events. Next is "" when nothing follows.
type EventPage struct {
	Events []CatalogEvent
	Next   string
}

// EntityHandleFilter narrows SubstrateCatalog.EntityHandles. Namespace is required.
type EntityHandleFilter struct {
	Namespace string
	AfterID   string
	Limit     int
}

// CatalogHandle is one minted entity handle.
type CatalogHandle struct {
	ID   string
	Kind string
}

// EntityHandlePage is one page of handles. Next is "" when nothing follows.
type EntityHandlePage struct {
	Handles []CatalogHandle
	Next    string
}

// EntityFilter narrows SubstrateCatalog.Entities. Zero fields do not filter.
// Exactly the shapes its consumers ask; it is not a query language.
type EntityFilter struct {
	// IDs keeps only these exact entity ids.
	IDs []string
	// Substrings keeps ids containing any of these, case-insensitively. Literal:
	// the implementation escapes LIKE metacharacters.
	Substrings []string
	// Predicate keeps entities with a readable observation of this predicate.
	Predicate string
	// EventType keeps entities that took part in a readable event of this type.
	EventType string
	// IncludeResolutions also discovers entities through readable live
	// resolutions (item-backed), not only through observations/events.
	IncludeResolutions bool
	// Limit bounds the result (implementation default and cap apply).
	Limit int
	// PredicateLimit bounds CatalogEntity.Predicates per entity (0 = default).
	PredicateLimit int
}

// CatalogEntity is one entity handle as the catalog reports it. Counts and
// predicates are over READABLE observations only.
type CatalogEntity struct {
	ID               string
	ObservationCount int64
	Predicates       []string
	LastSeen         time.Time
}

// SubstrateOverview is the explore surface's summary, every figure over rows the
// reader may read.
type SubstrateOverview struct {
	EvidenceCount, EventCount, ObservationCount int64
	KnowledgeItemCount, LiveResolutionCount     int64
	EntityCount, ActorCount, OutboxPending      int64
	// OutboxDead counts outbox items parked dead after their last attempt
	// (migration 0039): unprocessed, never retried automatically. OutboxPending
	// does NOT include them — pending is the work that will still happen (due,
	// backing off, or held) — so the two never double-count (owner decision
	// 2026-11-13, T-20261113-substrate-overview-has-no-separate-dead-letter).
	OutboxDead       int64
	Earliest, Latest time.Time // zero when nothing is readable
	Sources          []CatalogSource
	Kinds            []CatalogKindCount
}

// CatalogSource is one evidence source with the distinct classification tags
// of its READABLE evidence (so an owner tag appears only to its owner).
type CatalogSource struct {
	SourceID       string
	EvidenceCount  int64
	LastSeen       time.Time
	Classification []string
}

// CatalogKindCount is one knowledge-item kind and its readable item count.
type CatalogKindCount struct {
	Kind  string
	Count int64
}
