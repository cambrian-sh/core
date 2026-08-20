package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgLinkStore is the Postgres adapter for domain.LinkStore (five-planes step 2;
// FIVE-PLANES-BUILD.md). Table comes from migration 0016; no DDL here.
//
// This adapter is the ENFORCEMENT POINT for the identity plane's three refusals —
// undeclared verb, trust ceiling, admissibility. They are checked here rather than in the
// producers because a rule that lives in a producer binds only the producers that
// remembered it, and the whole point of the plane is that a heuristic cannot promote
// itself into the answer set no matter who wrote it. The RULES themselves are pure
// functions in domain/identity.go (one place for the mechanism names); this file supplies
// the chokepoint and the SQL.
//
// The other invariant this file owns: an existing row is NEVER rewritten except for
// state/retracted_at (ADR-0093 D6). Confirmation appends; retraction stamps. What the
// machine proposed and what the human decided stay two separate, readable assertions.
type PgLinkStore struct {
	pool *pgxpool.Pool
	reg  *domain.RelationRegistry
}

// NewPgLinkStore wraps an existing pool. reg may be nil, in which case only the built-in
// seed verbs are declared (domain.RelationRegistry's nil behaviour).
func NewPgLinkStore(pool *pgxpool.Pool, reg *domain.RelationRegistry) *PgLinkStore {
	return &PgLinkStore{pool: pool, reg: reg}
}

var _ domain.LinkStore = (*PgLinkStore)(nil)

// defaultIdentityLimit caps an unbounded identity-plane read. A traversal that forgot to
// say how much it wanted should get a page, not the graph.
const defaultIdentityLimit = 200

// confirmationSourceRef derives the source_ref of a confirmation row from the row being
// confirmed. It is what makes re-confirming a no-op: the dedup key already contains it,
// so the second click inserts nothing instead of stacking a second human assertion that
// says the same thing.
func confirmationSourceRef(linkID string) string { return "confirm:" + linkID }

const linkColumns = `id, namespace_id, family, from_ref, to_ref, relation, state, mechanism,
	producer, confidence, evidence_id, asserted_by, asserted_at, recorded_at,
	valid_from, valid_to, retracted_at, source_ref`

// PutLink appends one assertion, idempotent on links_dedup.
//
// Defaults are filled before validation, so a producer states only what it actually
// decided: an unstated state is a candidate (the conservative half), an unstated family
// is the one the verb's declaration already names, an unstated confidence is 1.0.
func (s *PgLinkStore) PutLink(ctx context.Context, l domain.Link) (bool, error) {
	if l.NamespaceID == "" {
		l.NamespaceID = "default"
	}
	if l.State == "" {
		l.State = domain.LinkStateCandidate
	}
	if l.Family == "" {
		// The verb already declares its family; making a producer repeat it is one
		// more place for the two to disagree.
		if spec, ok := s.reg.Spec(l.Relation); ok {
			l.Family = spec.Family
		}
	}
	if l.Confidence == 0 {
		l.Confidence = 1.0
	}
	if l.AssertedAt.IsZero() {
		l.AssertedAt = time.Now().UTC()
	}
	if err := s.reg.ValidateLink(l); err != nil {
		return false, err
	}
	// AFTER validation, because canonical ordering must not be able to turn an
	// inadmissible assertion into an admissible one.
	l = domain.CanonicalizeLink(l)
	if l.ID == "" {
		l.ID = "lnk_" + uuid.NewString()
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO links (id, namespace_id, family, from_ref, to_ref, relation, state,
			mechanism, producer, confidence, evidence_id, asserted_by, asserted_at,
			valid_from, valid_to, source_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT ON CONSTRAINT links_dedup DO NOTHING`,
		l.ID, l.NamespaceID, l.Family, l.FromRef, l.ToRef, l.Relation, l.State,
		l.Mechanism, l.Producer, l.Confidence, nullableEvidence(l.EvidenceID), l.AssertedBy,
		l.AssertedAt.UTC(), utcOrNil(l.ValidFrom), utcOrNil(l.ValidTo), l.SourceRef)
	if err != nil {
		return false, fmt.Errorf("link put: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ConfirmLink writes a NEW human-mechanism row and leaves the confirmed one untouched.
//
// The alternative — flipping the candidate's state in place — was rejected for the
// reason the whole plane exists: the review lane's product is a record of what each
// producer proposed and what a person made of it. Overwriting the proposal destroys
// exactly the evidence needed to decide whether that producer should keep running.
//
// The confirmation INHERITS the candidate's evidence: the human is endorsing that basis,
// not inventing a new one, and a confirmation with no basis would be the admissibility
// rule defeated by a click.
func (s *PgLinkStore) ConfirmLink(ctx context.Context, namespace, linkID, actor string) (domain.Link, error) {
	if namespace == "" {
		namespace = "default"
	}
	if actor == "" {
		return domain.Link{}, fmt.Errorf("link confirm: an actor is required: %w", domain.ErrLinkRefused)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Link{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	src, err := scanLink(tx.QueryRow(ctx,
		`SELECT `+linkColumns+` FROM links WHERE namespace_id=$1 AND id=$2`,
		namespace, linkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Link{}, fmt.Errorf("link confirm: no link %q in namespace %q: %w",
			linkID, namespace, domain.ErrLinkRefused)
	}
	if err != nil {
		return domain.Link{}, err
	}

	confirmed := domain.Link{
		ID:          "lnk_" + uuid.NewString(),
		NamespaceID: src.NamespaceID,
		Family:      src.Family,
		FromRef:     src.FromRef,
		ToRef:       src.ToRef,
		Relation:    src.Relation,
		State:       domain.LinkStateConfirmed,
		Mechanism:   domain.LinkMechanismHuman,
		Confidence:  1.0,
		EvidenceID:  src.EvidenceID,
		AssertedBy:  actor,
		AssertedAt:  time.Now().UTC(),
		ValidFrom:   src.ValidFrom,
		ValidTo:     src.ValidTo,
		SourceRef:   confirmationSourceRef(linkID),
	}
	if err := s.reg.ValidateLink(confirmed); err != nil {
		return domain.Link{}, err
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO links (id, namespace_id, family, from_ref, to_ref, relation, state,
			mechanism, producer, confidence, evidence_id, asserted_by, asserted_at,
			valid_from, valid_to, source_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'',$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT ON CONSTRAINT links_dedup DO NOTHING`,
		confirmed.ID, confirmed.NamespaceID, confirmed.Family, confirmed.FromRef, confirmed.ToRef,
		confirmed.Relation, confirmed.State, confirmed.Mechanism, confirmed.Confidence,
		nullableEvidence(confirmed.EvidenceID), confirmed.AssertedBy, confirmed.AssertedAt,
		utcOrNil(confirmed.ValidFrom), utcOrNil(confirmed.ValidTo), confirmed.SourceRef)
	if err != nil {
		return domain.Link{}, fmt.Errorf("link confirm: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already confirmed: return the standing confirmation rather than an error.
		// A second click on a review card is a repeat, not a fault.
		confirmed, err = scanLink(tx.QueryRow(ctx, `
			SELECT `+linkColumns+` FROM links
			WHERE namespace_id=$1 AND family=$2 AND from_ref=$3 AND to_ref=$4
			  AND relation=$5 AND mechanism=$6 AND source_ref=$7`,
			confirmed.NamespaceID, confirmed.Family, confirmed.FromRef, confirmed.ToRef,
			confirmed.Relation, confirmed.Mechanism, confirmed.SourceRef))
		if err != nil {
			return domain.Link{}, fmt.Errorf("link confirm replay lookup: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Link{}, err
	}
	return confirmed, nil
}

// RetractLink stamps state and retracted_at — and touches NO other column, which is the
// single reason retraction is modelled as an update at all rather than as an append.
//
// actor and reason are NOT written to the row: the row would then be a row that can be
// rewritten, and the operator mutation lane already records who asked and why
// (command_id + reason, audit-first) at the RPC boundary where a human actually is.
func (s *PgLinkStore) RetractLink(ctx context.Context, namespace, linkID, actor, reason string) error {
	_ = actor  // recorded by the caller's audit lane; see the doc comment
	_ = reason // ditto — a link row never gains a column
	if namespace == "" {
		namespace = "default"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE links SET state=$3, retracted_at=now()
		WHERE namespace_id=$1 AND id=$2 AND retracted_at IS NULL`,
		namespace, linkID, domain.LinkStateRetracted)
	if err != nil {
		return fmt.Errorf("link retract: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing updated: either already retracted (idempotent, fine) or no such link
	// (a caller pointing at a link that does not exist should hear so).
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM links WHERE namespace_id=$1 AND id=$2)`,
		namespace, linkID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("link retract: no link %q in namespace %q: %w",
			linkID, namespace, domain.ErrLinkRefused)
	}
	return nil
}

// RetractByProducer revokes every unretracted row a producer wrote, in one statement,
// and returns the count. This is why `producer` is a column: a pass that turns out to be
// wrong is undone wholesale, which is the difference between a fixable mistake and a
// permanent one.
func (s *PgLinkStore) RetractByProducer(ctx context.Context, namespace, producer, actor string) (int, error) {
	_ = actor // as in RetractLink: audited by the caller, never stamped on the row
	if namespace == "" {
		namespace = "default"
	}
	if producer == "" {
		// An empty producer is the DEFAULT of the column, so this would revoke every
		// hand-written and declared link in the namespace. Refuse loudly.
		return 0, fmt.Errorf("link revoke: a producer name is required — an empty one would "+
			"match every row that never named a producer: %w", domain.ErrLinkRefused)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE links SET state=$3, retracted_at=now()
		WHERE namespace_id=$1 AND producer=$2 AND retracted_at IS NULL`,
		namespace, producer, domain.LinkStateRetracted)
	if err != nil {
		return 0, fmt.Errorf("link revoke: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// LinksFor returns the assertions touching a typed ref.
//
// Direction is decided by the REGISTRY, never by naming a verb: a symmetric verb's links
// come back for either endpoint (canonical ordering only decided which column the ref
// landed in — it is not a statement about direction), and an asymmetric verb's inbound
// rows appear only when the caller asked for the backward walk.
func (s *PgLinkStore) LinksFor(ctx context.Context, namespace, ref string, opts domain.LinkQuery) ([]domain.Link, error) {
	if namespace == "" {
		namespace = "default"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	// The symmetric-verbs parameter exists only when its clause does: a bound
	// parameter the SQL text never references is a protocol error on a real
	// server ("could not determine data type of parameter $3"), which the
	// DB-less tests could not see. Caught by the live-PG Wave-2 gate.
	args := []any{namespace, ref}
	where := `namespace_id=$1 AND (from_ref=$2 OR to_ref=$2)`
	if !opts.IncludeIncoming {
		args = append(args, s.reg.SymmetricVerbs())
		where = `namespace_id=$1 AND (from_ref=$2 OR (to_ref=$2 AND relation = ANY($3::text[])))`
	}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(clause, strconv.Itoa(len(args)))
	}
	if opts.Family != "" {
		add(` AND family=$%s`, opts.Family)
	}
	if opts.State != "" {
		add(` AND state=$%s`, opts.State)
	}
	if opts.Relation != "" {
		add(` AND relation=$%s`, opts.Relation)
	}
	if !opts.IncludeRetracted {
		where += ` AND retracted_at IS NULL`
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx,
		`SELECT `+linkColumns+` FROM links WHERE `+where+
			` ORDER BY asserted_at DESC, id LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return collectLinks(rows)
}

// Candidates is the review inbox: unretracted proposals, highest confidence first — the
// order idx_links_review exists to serve.
func (s *PgLinkStore) Candidates(ctx context.Context, namespace string, limit int) ([]domain.Link, error) {
	if namespace == "" {
		namespace = "default"
	}
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+linkColumns+` FROM links
		WHERE namespace_id=$1 AND state=$2 AND retracted_at IS NULL
		ORDER BY confidence DESC, recorded_at DESC, id
		LIMIT $3`,
		namespace, domain.LinkStateCandidate, limit)
	if err != nil {
		return nil, err
	}
	return collectLinks(rows)
}

func collectLinks(rows pgx.Rows) ([]domain.Link, error) {
	defer rows.Close()
	var out []domain.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLink(row pgx.Row) (domain.Link, error) {
	var l domain.Link
	var evidenceID *string
	if err := row.Scan(&l.ID, &l.NamespaceID, &l.Family, &l.FromRef, &l.ToRef, &l.Relation,
		&l.State, &l.Mechanism, &l.Producer, &l.Confidence, &evidenceID, &l.AssertedBy,
		&l.AssertedAt, &l.RecordedAt, &l.ValidFrom, &l.ValidTo, &l.RetractedAt,
		&l.SourceRef); err != nil {
		return domain.Link{}, err
	}
	if evidenceID != nil {
		l.EvidenceID = domain.EvidenceID(*evidenceID)
	}
	return l, nil
}

// nullableEvidence keeps an absent basis as SQL NULL rather than "", so the FK to
// evidence(id) stays satisfiable for the human-mechanism rows that legitimately have none.
func nullableEvidence(id domain.EvidenceID) *string {
	if id == "" {
		return nil
	}
	v := string(id)
	return &v
}

func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
