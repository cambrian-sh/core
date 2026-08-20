package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgEntityStore is the Postgres adapter for domain.EntityStore (five-planes step 1;
// FIVE-PLANES-BUILD.md). Table comes from migration 0016; no DDL here.
//
// Minting is deliberately the cheapest operation in the substrate: one insert, no
// validation beyond the id's shape. Everything that could be WRONG about an entity —
// that it is the same as another, that it stands in some relation — is a link, and links
// are where refusal and review live. A store that made minting expensive would push
// producers into dropping rows they could not verify, which is how the identity plane
// would end up sparser than the data.
type PgEntityStore struct {
	pool *pgxpool.Pool
}

// NewPgEntityStore wraps an existing pool.
func NewPgEntityStore(pool *pgxpool.Pool) *PgEntityStore {
	return &PgEntityStore{pool: pool}
}

var _ domain.EntityStore = (*PgEntityStore)(nil)

const entityColumns = `id, namespace_id, kind, first_seen_evidence, created_at`

// EnsureEntity mints the handle if absent, idempotent on the id. The kind is derived
// from the id's prefix stem when the caller left it empty — a producer holding
// "customer/C-1042" should not have to say "customer" twice — and validated for
// well-formedness only (amendment S3).
func (s *PgEntityStore) EnsureEntity(ctx context.Context, e domain.Entity) (bool, error) {
	if e.ID == "" {
		return false, fmt.Errorf("entity mint: an id is required: %w", domain.ErrLinkRefused)
	}
	kind := e.Kind
	if kind == "" {
		stem, ok := domain.EntityKindFromID(e.ID)
		if !ok {
			return false, fmt.Errorf("entity id %q carries no kind prefix (want \"kind/local\") — an "+
				"unscoped id is the collision the scoping exists to prevent: %w", e.ID, domain.ErrLinkRefused)
		}
		kind = stem
	}
	if err := domain.ValidateEntityKind(kind); err != nil {
		return false, err
	}
	ns := e.NamespaceID
	if ns == "" {
		ns = "default"
	}
	var firstSeen *string
	if e.FirstSeenEvidence != "" {
		v := string(e.FirstSeenEvidence)
		firstSeen = &v
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO entities (id, namespace_id, kind, first_seen_evidence)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, ns, kind, firstSeen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetEntity returns the handle; ok=false when it was never minted — an answer, not an
// error.
func (s *PgEntityStore) GetEntity(ctx context.Context, namespace, id string) (domain.Entity, bool, error) {
	if namespace == "" {
		namespace = "default"
	}
	e, err := scanEntity(s.pool.QueryRow(ctx,
		`SELECT `+entityColumns+` FROM entities WHERE namespace_id=$1 AND id=$2`,
		namespace, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Entity{}, false, nil
	}
	if err != nil {
		return domain.Entity{}, false, err
	}
	return e, true, nil
}

// ListEntitiesByKind returns minted handles of one kind, newest first.
func (s *PgEntityStore) ListEntitiesByKind(ctx context.Context, namespace, kind string, limit int) ([]domain.Entity, error) {
	if namespace == "" {
		namespace = "default"
	}
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+entityColumns+` FROM entities
		WHERE namespace_id=$1 AND kind=$2
		ORDER BY created_at DESC, id
		LIMIT $3`,
		namespace, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolveEntityByLocal returns the handles whose id ends in "/"+local, across kinds
// (five-planes step 2 / build doc P2, D-W2-2) — the lookup a producer needs when it holds
// an id-shaped token out of prose and no kind to scope it with.
//
// TWO THINGS ABOUT THE SQL, both deliberate:
//
// The match is a right-anchored LIKE over a pattern escaped by the package's existing
// escapeLike, paired with the explicit ESCAPE '\'. Escaping matters more than
// it looks: source ids in the wild contain "_" and "%", and an unescaped "OPS_12" would
// wildcard-match "OPS-12" and "OPSX12" — turning a clean one-hit resolution into a
// spurious ambiguity, or worse, a confident link to the wrong entity.
//
// It is a SEQUENTIAL SCAN, and knowingly so. A trailing-suffix predicate cannot use the
// btree on id; making it indexable would mean a reverse(id) expression index, which is a
// migration this build doc does not own, on a table whose row count is bounded by the
// entities a deployment has actually met. The limit caps the work either way. If entity
// counts ever make this hot, the fix is that index — not a looser match.
func (s *PgEntityStore) ResolveEntityByLocal(ctx context.Context, namespace, local string, limit int) ([]domain.Entity, error) {
	if namespace == "" {
		namespace = "default"
	}
	if local == "" {
		// Not an error: "nothing is called nothing" is the honest answer, and a
		// producer asking about an empty token gets zero hits and records the
		// ambiguity, exactly as it would for an unknown one.
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+entityColumns+` FROM entities
		WHERE namespace_id=$1 AND id LIKE $2 ESCAPE '\'
		ORDER BY id
		LIMIT $3`,
		namespace, "%/"+escapeLike(local), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEntity(row pgx.Row) (domain.Entity, error) {
	var e domain.Entity
	var firstSeen *string
	if err := row.Scan(&e.ID, &e.NamespaceID, &e.Kind, &firstSeen, &e.CreatedAt); err != nil {
		return domain.Entity{}, err
	}
	if firstSeen != nil {
		e.FirstSeenEvidence = domain.EvidenceID(*firstSeen)
	}
	return e, nil
}
