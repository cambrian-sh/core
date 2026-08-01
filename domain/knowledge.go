package domain

import (
	"context"
	"sort"
	"time"
)

// KnowledgeItem is one APPEND-ONLY typed interpretation derived from evidence —
// the second stage of the substrate's epistemic pipeline (ADR-0106; memo §6):
//
//	Evidence → KnowledgeItem → Assessment → Resolution → Effect
//
// Two contradictory items about the same entity are BOTH valid records of what
// different sources said; nothing here prevents their coexistence, because the
// disagreement is the signal the layer exists to preserve.
type KnowledgeItem struct {
	ID          KnowledgeItemID
	NamespaceID string
	// Kind names the interpretation type ("commitment", …). Kinds are DATA —
	// the kernel never branches on one.
	Kind string
	// EvidenceID links to the evidence this item derives from. Empty in Phase
	// 2a (the first producer's lane cannot see evidence ids yet); Phase 2b
	// populates it. See DECISIONS 2026-08-01 D-D.
	EvidenceID EvidenceID
	// EntityID is the source-scoped entity the item is about
	// ("purchase_order/PO-4471").
	EntityID string
	// AssertedBy and AssertedAt attribute the assertion to an actor and the
	// actor's OWN clock — distinct from when the row was stored (memo §8).
	AssertedBy string
	AssertedAt time.Time
	// SourceRef is the source-native reference of the assertion (message id).
	// Together with (Kind, EntityID) it is the idempotency key; empty means
	// the item has no replay protection of its own.
	SourceRef string
	// Negation retires the actor's prior assertion about this entity without
	// replacing it. A retraction is itself an item — never a deletion.
	Negation       bool
	Classification []string
	ValidFrom      time.Time
	ValidTo        time.Time
	Values         []StatementValue
}

// KnowledgeItemID identifies one knowledge item.
type KnowledgeItemID string

// StatementValue is one typed predicate value on an item. Exactly one of the
// value fields is set — enforced in the schema (memo §6: typed columns from day
// one keep the deferred sub-type split additive).
type StatementValue struct {
	Predicate string
	Type      string // "date" | "number" | "text" | "entity"
	Date      time.Time
	Number    float64
	Text      string
	EntityRef string
}

// Resolution is one version of the substrate's DERIVED belief for
// (namespace, kind, entity, actor) under a named policy. Derived and
// rebuildable — never evidence. Item is the hydrated winning item; nil when
// the actor's latest word was a negation.
type Resolution struct {
	NamespaceID string
	Kind        string
	EntityID    string
	Actor       string
	Policy      string
	Item        *KnowledgeItem
	ReasonCode  string
	SystemFrom  time.Time
}

// ResolutionPolicyLatestAssertion is the one policy Phase 2a ships: the
// actor's latest assertion wins; a latest negation means no current belief.
const ResolutionPolicyLatestAssertion = "latest_assertion"

// Resolution reason codes for ResolutionPolicyLatestAssertion.
const (
	ReasonLatestAssertion = "latest_assertion"
	ReasonNegated         = "negated"
)

// ResolveLatestAssertion computes the winning item among ALL of one
// (namespace, kind, entity, actor) key's items. nil item + ReasonNegated when
// the latest word is a negation; nil item + "" when items is empty.
//
// The function is PURE and its result is a function of the SET, not the
// sequence: ordering is (AssertedAt, SourceRef, Negation) with lexicographic
// tie-breaks, so replaying the same items in any arrival order yields the same
// winner (memo §13 — arrival triggers computation; it must not define
// semantics). This is the property the order-independence gate tests, and any
// store implementing KnowledgeStore must derive resolutions with exactly this
// function rather than by comparing against "the prior" row.
func ResolveLatestAssertion(items []KnowledgeItem) (*KnowledgeItem, string) {
	if len(items) == 0 {
		return nil, ""
	}
	sorted := make([]KnowledgeItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.AssertedAt.Equal(b.AssertedAt) {
			return a.AssertedAt.Before(b.AssertedAt)
		}
		if a.SourceRef != b.SourceRef {
			return a.SourceRef < b.SourceRef
		}
		// A negation at the exact same instant as an assertion loses to it
		// (sorts earlier, so the assertion is the last element): retiring a
		// statement you are simultaneously making is the less destructive
		// reading, and the choice must merely be deterministic.
		return a.Negation && !b.Negation
	})
	winner := sorted[len(sorted)-1]
	if winner.Negation {
		return nil, ReasonNegated
	}
	w := winner
	return &w, ReasonLatestAssertion
}

// KnowledgeStore is the substrate's minimal typed boundary for items and
// resolutions (memo §18: it ships WITH the first producer so no consumer ever
// grows direct SQL against substrate tables). Plugins receive this port —
// never a pool, never a predicate.
type KnowledgeStore interface {
	// PutItem appends one item and recomputes the resolution for its
	// (namespace, kind, entity, actor) key under every policy the store
	// maintains, from the FULL item set (ResolveLatestAssertion).
	//
	// Idempotent: when (namespace, kind, entity, source_ref) already exists —
	// SourceRef non-empty — nothing is written and the existing id returns
	// with inserted=false.
	PutItem(ctx context.Context, item KnowledgeItem) (id KnowledgeItemID, inserted bool, err error)

	// GetItem returns one item with its values.
	GetItem(ctx context.Context, id KnowledgeItemID) (*KnowledgeItem, error)

	// CurrentResolutions lists the CURRENT belief rows for a kind under a
	// policy, hydrated with their winning items. Rows whose latest word was a
	// negation are omitted — a caller asking "what is currently believed"
	// must not receive tombstones.
	CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]Resolution, error)

	// EraseItems PERMANENTLY deletes matching items, their values, and every
	// resolution version derived from them, then re-derives the projection for
	// any key that lost only part of its items.
	//
	// This is the one true deletion in the substrate, and it exists for exactly
	// one reason: a compliance erasure outranks append-only history. It is NOT
	// retraction (a negation item) and must never be reached for anything less
	// than an erasure request. Returns the number of items removed.
	EraseItems(ctx context.Context, namespace, kind string, sel KnowledgeErasure) (int, error)
}

// KnowledgeErasure selects items for permanent deletion: items ABOUT the named
// entities, plus items carrying an entity-typed statement value equal to
// ValueEntityRef (the "data about this counterparty" half of an erasure).
// An empty selector matches nothing — erasing everything requires saying so
// entity by entity.
type KnowledgeErasure struct {
	Entities       []string
	ValueEntityRef string
}
