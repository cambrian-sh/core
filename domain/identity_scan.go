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
	// EntityExists reports whether a scoped handle has been minted (and may be
	// known under scope).
	EntityExists(ctx context.Context, scope *TagPredicate, namespace, id string) (bool, error)
	// EntitiesByLocal returns the scoped ids ending in "/"+local; local is data,
	// never a pattern.
	EntitiesByLocal(ctx context.Context, scope *TagPredicate, namespace, local string, limit int) ([]string, error)
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
}

// IdentityEntityRow is a minted handle as a scan sees it.
type IdentityEntityRow struct {
	ID                string
	Kind              string
	CreatedAt         time.Time
	FirstSeenEvidence string
}

// IdentityPartyRow pairs a party identity with an evidence row carrying it.
type IdentityPartyRow struct {
	Party      string
	EvidenceID string
}

// IdentityEventRow is an occurrence as a scan sees it.
type IdentityEventRow struct {
	ID         string
	EventType  string
	OccurredAt time.Time
	EvidenceID string
}
