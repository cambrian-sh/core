package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/objects"
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
	pool  *pgxpool.Pool
	kinds domain.KindSource
}

// NewPgEntityStore wraps an existing pool. kinds is the live ontology or a fixed
// *domain.KindRegistry (nil means no declared kinds): a mint of an object of a
// table kind recomputes its object row (ADR-0149 D8), because the evidence
// that minted an entity is one of the fence classes the row carries.
func NewPgEntityStore(pool *pgxpool.Pool, kinds domain.KindSource) *PgEntityStore {
	if kinds == nil {
		kinds = (*domain.KindRegistry)(nil)
	}
	return &PgEntityStore{pool: pool, kinds: kinds}
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
	ns = orDefaultNamespace(ns)
	var firstSeen *string
	if e.FirstSeenEvidence != "" {
		v := string(e.FirstSeenEvidence)
		firstSeen = &v
	}
	// The mint is one of the object maintainer's write paths (ADR-0149 D8): the
	// object row's vis_classes carries the class of the evidence that minted the
	// entity, and producers mint AFTER they write the observations (the ingress
	// studio writes typed rows, then its link batch), so a row already exists
	// whose classes this insert changes. Recompute it in the same transaction.
	// Only a created row matters: first_seen_evidence is never updated.
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO entities (id, namespace_id, kind, first_seen_evidence)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (namespace_id, id) DO NOTHING`,
			e.ID, ns, kind, firstSeen)
		if err != nil {
			return err
		}
		if created = tag.RowsAffected() == 1; !created {
			return nil
		}
		return objects.ApplyStateTx(ctx, tx, s.kinds, ns, e.ID)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// GetEntity returns the handle; ok=false when it was never minted — an answer, not an
// error.
func (s *PgEntityStore) GetEntity(ctx context.Context, namespace, id string) (domain.Entity, bool, error) {
	namespace = orDefaultNamespace(namespace)
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
	namespace = orDefaultNamespace(namespace)
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
