package postgres

import (
	"context"
	"github.com/cambrian-sh/core/domain"
)

// saveExperienceSQL is written by hand rather than through goqu on purpose.
//
// The builder renders a Go []string as a ROW literal, which Postgres rejects against a
// text[] column: `column "tags" is of type text[] but expression is of type record`
// (SQLSTATE 42804). pgx binds []string to text[] natively through a placeholder, so the
// parameterised form is both correct and simpler than coaxing the builder into it.
const saveExperienceSQL = `
INSERT INTO experiences (id, session_id, surface, tags, outcome, started_at, completed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
	outcome      = EXCLUDED.outcome,
	completed_at = EXCLUDED.completed_at,
	tags         = EXCLUDED.tags,
	surface      = EXCLUDED.surface`

// SaveExperience upserts an episode parent row (ADR-0095 D1).
//
// Idempotent on the plan-derived id, because that id is pre-allocated at plan start
// (ADR-0049 D5) and the row may be written more than once for the same episode — a
// retry, or a later pass that learns the outcome. Re-writing must UPDATE, not duplicate
// or fail, or a retried plan would lose its parent and orphan its chunks.
//
// `tags` is written from the kernel-stamped value only. Nothing here accepts
// caller-authored classification: an agent may narrow what it writes, never author the
// authoritative row (ADR-0035, applied to experiences by ADR-0095 D4).
func (p *PgVectorAdapter) SaveExperience(ctx context.Context, e domain.Experience) error {
	if e.ID == "" {
		return nil
	}
	tags := e.Tags
	if len(tags) == 0 {
		// Belt and braces for D4's "cannot be born untagged". The caller already
		// stamps this; if a future one forgets, the row is still governed by
		// something rather than by nothing — an untagged row has no tags for any
		// predicate to act on, which is how a boundary becomes inexpressible.
		tags = []string{domain.TagInternal}
	}
	// Zero times are written as NULL rather than 0001-01-01, which would otherwise
	// poison any retention or age predicate that later reads these columns.
	var started, completed any
	if !e.StartedAt.IsZero() {
		started = e.StartedAt
	}
	if !e.CompletedAt.IsZero() {
		completed = e.CompletedAt
	}
	_, err := p.pool.Exec(ctx, saveExperienceSQL,
		e.ID, e.SessionID, e.Surface, tags, e.Outcome, started, completed)
	return mapError("SaveExperience", err)
}

// linkDerivationSQL records provenance for an artifact distilled from N episodes.
// ON CONFLICT DO NOTHING because re-running the induction pass over the same episodes
// must be idempotent — a batch pass that duplicated its own provenance every night
// would make the D9 audit unreadable.
const linkDerivationSQL = `
INSERT INTO experience_derivations (derived_chunk_id, experience_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING`

// LinkDerivation implements domain.ExperienceStore (ADR-0095 D5).
//
// Best-effort per row: a missing episode (pruned by retention, say) skips its link
// rather than failing the whole write. Losing one provenance edge is recoverable;
// losing the artifact because one of its sources aged out is not.
func (p *PgVectorAdapter) LinkDerivation(ctx context.Context, derivedChunkID string, experienceIDs []string) error {
	if derivedChunkID == "" || len(experienceIDs) == 0 {
		return nil
	}
	for _, expID := range experienceIDs {
		if expID == "" {
			continue
		}
		if _, err := p.pool.Exec(ctx, linkDerivationSQL, derivedChunkID, expID); err != nil {
			return mapError("LinkDerivation", err)
		}
	}
	return nil
}
