package domain

import (
	"context"
	"time"
)

// Experience is one EPISODE — a single plan execution (ADR-0095 D1). It is the
// parent row that the chunks a plan produced hang from, and the row access policy
// attaches to.
//
// It exists because ADR-0093 concluded that agent-written memory has no parent, which
// was right about `documents` and wrong about parentage in general: the parent is not a
// document, it is the episode. Without this row an experiential memory had nothing to
// carry `classification_tags`, so ADR-0091's closed-tag enforcement had nothing to bind
// to and the boundary was inexpressible — not hard, inexpressible.
type Experience struct {
	// ID is derived from the plan id, so records written mid-plan can reference a
	// parent that already exists (mirrors ADR-0049 D5's pre-allocated scene id).
	ID        string
	SessionID string
	// Surface is the ingress the producing session was opened on (ADR-0090
	// Session.Surface, decided ONCE by the ingress). Half of the D4 born-tagged stamp.
	Surface string
	// Tags is the AUTHORITATIVE classification. Kernel-stamped and unforgeable: an
	// agent may narrow it, never author it (the ADR-0035 pattern).
	Tags        []string
	Outcome     string
	StartedAt   time.Time
	CompletedAt time.Time
}

// TagInternal is the default classification every experience is born with (ADR-0095
// D4). An episode is a byproduct of the system doing work — unlike a corpus document,
// nobody decided it should be retrievable — so it starts internal and a policy widens
// it, never the reverse.
const TagInternal = "internal"

// ExperienceStore persists episode parent rows. Implemented by the Postgres adapter;
// nil in a deployment that has not enabled experience records, in which case chunks
// keep a NULL parent rather than failing to write (ADR-0095 D5: a write must never
// fail over bookkeeping).
type ExperienceStore interface {
	SaveExperience(ctx context.Context, e Experience) error
	// LinkDerivation records that a derived artifact (a procedure, a session
	// narrative) was distilled from these episodes — ADR-0095 D5.
	//
	// It is what turns ADR-0095 D9's rule from an intention into an auditable fact:
	// with the sources enumerated, "was this derived across a closed-tag boundary?" is
	// a query. Without it, the induction pass could refuse cross-boundary derivation
	// perfectly today and nobody could ever verify it had.
	LinkDerivation(ctx context.Context, derivedChunkID string, experienceIDs []string) error
}

// BornTags builds the D4 stamp for an episode opened on `surface`. Always returns at
// least TagInternal: there is no code path that produces an untagged experience, which
// is what makes the boundary enforceable rather than dependent on operator diligence
// (ADR-0091 hit the same prerequisite with MCP tools — an untagged resource has no tags
// for any predicate to act on).
func BornTags(surface SurfaceRef) []string {
	tags := []string{TagInternal}
	// Kind first: it is the coarse channel ("chat", "operator", "reactive") a policy
	// most often wants to gate on. The concrete id is added too, so a policy CAN name
	// one ingress without having to enumerate every other.
	if surface.Kind != "" {
		tags = append(tags, "surface:"+surface.Kind)
	}
	if surface.ID != "" && surface.ID != surface.Kind {
		tags = append(tags, "ingress:"+surface.ID)
	}
	return tags
}
