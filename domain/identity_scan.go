package domain

import (
	"context"
	"time"
)

// IdentityScanSource is the narrow read port the background identity producers
// scan the substrate through (owner decision 2026-11-14, ENG-1393; ADR-0106 D4:
// "never SQL against substrate tables").
//
// The producers ask a question no other port asks — "what has arrived since I last
// looked", over a whole namespace, in bounded pages that survive a restart — so the
// port is shaped by their scans rather than widened onto EventStore or LinkStore,
// whose point lookups a hundred callers share. They keep WRITING through LinkStore,
// where the identity plane's refusals live.
//
// The substrate's read rules are applied INSIDE every method, so no caller can
// re-implement or forget them:
//   - ADR-0147 D6: a typed row (observation, event) whose source_ref has a live
//     retirement is never returned — a record deleted at its source must not keep
//     producing identity proposals;
//   - the access predicate: every row is fenced through the evidence it rests on
//     (observations and events by evidence_id, entities by first-seen evidence,
//     evidence rows themselves, links by their evidence), rendered by
//     internal/authz/sqlfence. A nil scope is refused with ErrQueryScopeMissing.
//     A kernel lane with no principal passes ScopeSystem, which still applies the
//     owner fence (domain/owner_scope.go), so one owner's private records never
//     seed a shared identity proposal;
//   - every read is confined to one namespace, and an empty namespace is refused.
//
// Every row also carries back the ROW classification it rests on (ADR-0158 D1;
// ENG-1999). A kernel lane reading at ScopeSystem sees rows a mapping restricted,
// so whatever it derives from them must carry their tags forward: a candidate link
// proposed from a restricted observation inherits the observation's R, and is then
// fenced like it. nil ≡ empty.
//
// Every method is bounded by limit; none can return "all of it".
type IdentityScanSource interface {
	// AliasObservations pages text-valued observations whose predicate is in
	// predicates and whose identity column is > afterID, oldest first.
	AliasObservations(ctx context.Context, scope *TagPredicate, namespace string, predicates []string, afterID int64, limit int) ([]IdentityAliasRow, error)
	// EntitiesSince pages minted handles after a (created_at, id) position.
	EntitiesSince(ctx context.Context, scope *TagPredicate, namespace string, after IdentityScanPosition, limit int) ([]IdentityEntityRow, error)
	// PartiesForEntity returns the ADR-0121 party identities on the evidence rows
	// an entity appears on (minted by, observed in, or playing a role in an event
	// citing), each with one such evidence row.
	PartiesForEntity(ctx context.Context, scope *TagPredicate, namespace, entityID string, limit int) ([]IdentityPartyRow, error)
	// EntitiesWithParty returns the entities of one kind appearing on recent
	// evidence carrying a party identity — the reverse of PartiesForEntity.
	EntitiesWithParty(ctx context.Context, scope *TagPredicate, namespace, party, kind string, limit int) ([]IdentityEntityRow, error)
	// EventsSince pages occurrences after an (occurred_at, id) position.
	EventsSince(ctx context.Context, scope *TagPredicate, namespace string, after IdentityScanPosition, limit int) ([]IdentityEventRow, error)
	// RoleEntities returns the participants of one event in the namespace.
	RoleEntities(ctx context.Context, scope *TagPredicate, namespace, eventID string, limit int) ([]string, error)
	// PriorEventsSharingEntity returns events STRICTLY BEFORE the given position
	// (the same (time, id) comparison the scan orders by) that shared at least one
	// of entities and occurred at or after since. Newest first.
	PriorEventsSharingEntity(ctx context.Context, scope *TagPredicate, namespace string, entities []string, before IdentityScanPosition, since time.Time, limit int) ([]IdentityEventRow, error)
	// ConfirmedIdentityLinks returns the confirmed, unretracted identity links.
	ConfirmedIdentityLinks(ctx context.Context, scope *TagPredicate, namespace string, limit int) ([]Link, error)
	// CandidateLinksSince pages unretracted candidate links written by one of
	// mechanisms after a (recorded_at, id) position, oldest first.
	CandidateLinksSince(ctx context.Context, scope *TagPredicate, namespace string, mechanisms []string, after IdentityScanPosition, limit int) ([]Link, error)
	// EntityByID returns a scoped handle when it has been minted and may be known
	// under scope; ok is false otherwise. The row carries the mint's
	// classification, which a link that names the handle must inherit.
	EntityByID(ctx context.Context, scope *TagPredicate, namespace, id string) (row IdentityEntityRow, ok bool, err error)
	// EntitiesByLocal returns the scoped handles whose id ends in "/"+local;
	// local is data, never a pattern.
	EntitiesByLocal(ctx context.Context, scope *TagPredicate, namespace, local string, limit int) ([]IdentityEntityRow, error)
}

// IdentityScanPosition is a (time, id) scan bookmark. Time alone is not a cursor:
// two rows can share a timestamp, so the id breaks the tie and the scans compare
// the pair as a row value. The zero value is "from the beginning".
type IdentityScanPosition struct {
	At time.Time
	ID string
}

// IdentityAliasRow is one alias-bearing observation.
type IdentityAliasRow struct {
	// ID is the observation's identity column, and the scan cursor.
	ID       int64
	EntityID string
	// Value is the text exactly as stored, never pre-normalised.
	Value      string
	EvidenceID string
	OccurredAt time.Time
	// Classification is the observation's own row classification.
	Classification []string
}

// IdentityEntityRow is a minted handle as a scan sees it.
type IdentityEntityRow struct {
	ID                string
	Kind              string
	CreatedAt         time.Time
	FirstSeenEvidence string
	// Classification is what a proposal derived from this row must carry: the
	// mint's first_seen_classification and, for EntitiesWithParty, also the row
	// classification of the route row (mint, observation or event) that put the
	// handle on the party's evidence — the least restricted such route, so the
	// tags are those of one real supporting row.
	Classification []string
	// RouteEvidence is, for EntitiesWithParty only, the evidence row of that same
	// route row (the party-carrying evidence the handle appears on), so a link
	// derived from it can record it as a supporting row (ENG-2032). "" elsewhere.
	RouteEvidence string
}

// IdentityPartyRow pairs a party identity with an evidence row carrying it.
type IdentityPartyRow struct {
	Party      string
	EvidenceID string
	// Classification is the row classification of the route row (the mint, an
	// observation or an event) that put the handle on this evidence — the least
	// restricted such route, so the tags are those of one real supporting row.
	Classification []string
}

// IdentityEventRow is an occurrence as a scan sees it.
type IdentityEventRow struct {
	ID         string
	EventType  string
	OccurredAt time.Time
	EvidenceID string
	// Classification is the event's own row classification.
	Classification []string
}
