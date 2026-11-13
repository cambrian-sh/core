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
	pool      *pgxpool.Pool
	relations domain.RelationSource
}

// NewPgLinkStore wraps an existing pool. relations is the live ontology or a fixed
// *domain.RelationRegistry; nil means only the built-in seed verbs are declared
// (domain.RelationRegistry's nil behaviour).
func NewPgLinkStore(pool *pgxpool.Pool, relations domain.RelationSource) *PgLinkStore {
	if relations == nil {
		relations = (*domain.RelationRegistry)(nil)
	}
	return &PgLinkStore{pool: pool, relations: relations}
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
	l.NamespaceID = orDefaultNamespace(l.NamespaceID)
	reg := s.relations.RelationsFor(l.NamespaceID)
	if l.State == "" {
		l.State = domain.LinkStateCandidate
	}
	if l.Family == "" {
		// The verb already declares its family; making a producer repeat it is one
		// more place for the two to disagree.
		if spec, ok := reg.Spec(l.Relation); ok {
			l.Family = spec.Family
		}
	}
	if l.Confidence == 0 {
		l.Confidence = 1.0
	}
	if l.AssertedAt.IsZero() {
		l.AssertedAt = time.Now().UTC()
	}
	if err := reg.ValidateLink(l); err != nil {
		return false, err
	}
	// AFTER validation, because canonical ordering must not be able to turn an
	// inadmissible assertion into an admissible one.
	l = domain.CanonicalizeLink(l)
	if l.ID == "" {
		l.ID = "lnk_" + uuid.NewString()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	if err := checkSingleValued(ctx, tx, reg, l); err != nil {
		return false, err
	}
	created, err := insertLink(ctx, tx, l, reg.Version())
	if err != nil {
		return false, fmt.Errorf("link put: %w", err)
	}
	return created, tx.Commit(ctx)
}

// insertLink appends one assertion, idempotent on links_dedup; created=false on a
// replay. declVersion is the declarations version it was validated against.
func insertLink(ctx context.Context, tx pgx.Tx, l domain.Link, declVersion int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO links (id, namespace_id, family, from_ref, to_ref, relation, state,
			mechanism, producer, confidence, evidence_id, asserted_by, asserted_at,
			valid_from, valid_to, source_ref, decl_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT ON CONSTRAINT links_dedup DO NOTHING`,
		l.ID, l.NamespaceID, l.Family, l.FromRef, l.ToRef, l.Relation, l.State,
		l.Mechanism, l.Producer, l.Confidence, nullableEvidence(l.EvidenceID), l.AssertedBy,
		l.AssertedAt.UTC(), utcOrNil(l.ValidFrom), utcOrNil(l.ValidTo), l.SourceRef,
		declVersionOrNil(declVersion))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// checkSingleValued enforces Cardinality "one" (ADR-0149 D5) on a CONFIRMED link:
// its from endpoint may hold no other confirmed, unretracted link of the verb to a
// different target whose validity window overlaps this one's. A reassignment that
// bounds the old link (valid_to) before the new one starts is therefore allowed,
// which keeps "who owned this in March" answerable. Candidates are not checked —
// they are proposals awaiting review (ADR-0128). The advisory lock serialises two
// writers of the same (verb, from) so both cannot pass the check.
func checkSingleValued(ctx context.Context, tx pgx.Tx, reg *domain.RelationRegistry, l domain.Link) error {
	spec, _ := reg.Spec(l.Relation)
	if l.State != domain.LinkStateConfirmed || !spec.SingleValued() {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		resolutionLockKey(l.NamespaceID, "links|"+l.Relation, l.FromRef, "")); err != nil {
		return err
	}
	var otherID, otherTo string
	err := tx.QueryRow(ctx, `
		SELECT id, to_ref FROM links
		WHERE namespace_id=$1 AND relation=$2 AND from_ref=$3 AND to_ref<>$4
		  AND state='confirmed' AND retracted_at IS NULL
		  AND `+LinkValidityOverlaps("", "$5", "$6")+`
		ORDER BY recorded_at, id LIMIT 1`,
		l.NamespaceID, l.Relation, l.FromRef, l.ToRef, utcOrNil(l.ValidFrom), utcOrNil(l.ValidTo),
	).Scan(&otherID, &otherTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("link cardinality check: %w", err)
	}
	return fmt.Errorf("link relation %q has cardinality one: %s already holds confirmed link %s to %s "+
		"over an overlapping validity window (retract it, or bound its valid_to, first): %w",
		l.Relation, l.FromRef, otherID, otherTo, domain.ErrLinkRefused)
}

// LinkValidityOverlaps renders "a links row's validity window overlaps [from,
// to)": alias qualifies the row ("" = unqualified), from/to are SQL expressions,
// and a NULL bound is unbounded. The cardinality check and the ontology
// violations report share it, so a refused write and the report can never
// disagree about what overlapping means.
func LinkValidityOverlaps(alias, from, to string) string {
	if alias != "" {
		alias += "."
	}
	return `tstzrange(COALESCE(` + alias + `valid_from,'-infinity'), COALESCE(` + alias + `valid_to,'infinity'))
		   && tstzrange(COALESCE(` + from + `::timestamptz,'-infinity'), COALESCE(` + to + `::timestamptz,'infinity'))`
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
	namespace = orDefaultNamespace(namespace)
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
	reg := s.relations.RelationsFor(namespace)
	if err := reg.ValidateLink(confirmed); err != nil {
		return domain.Link{}, err
	}
	if err := checkSingleValued(ctx, tx, reg, confirmed); err != nil {
		return domain.Link{}, err
	}

	created, err := insertLink(ctx, tx, confirmed, reg.Version())
	if err != nil {
		return domain.Link{}, fmt.Errorf("link confirm: %w", err)
	}
	if !created {
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
	namespace = orDefaultNamespace(namespace)
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
	namespace = orDefaultNamespace(namespace)
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
	byRef, err := s.LinksForRefs(ctx, namespace, []string{ref}, opts)
	if err != nil {
		return nil, err
	}
	return byRef[ref], nil
}

// LinksForRefs is LinksFor over a set of refs in ONE statement: each ref runs
// the very query LinksFor always ran (same WHERE, same order, Limit and Offset
// per ref) as a LATERAL subquery, so a walk reads a whole frontier in one round
// trip instead of one per node. LinksFor is this with one ref — there is one
// rendering of "the links touching a ref", not two.
func (s *PgLinkStore) LinksForRefs(ctx context.Context, namespace string, refs []string, opts domain.LinkQuery) (map[string][]domain.Link, error) {
	namespace = orDefaultNamespace(namespace)
	out := map[string][]domain.Link{}
	if len(refs) == 0 {
		return out, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	offset := max(opts.Offset, 0)
	// The symmetric-verbs parameter exists only when its clause does: a bound
	// parameter the SQL text never references is a protocol error on a real
	// server ("could not determine data type of parameter $3"), which the
	// DB-less tests could not see. Caught by the live-PG Wave-2 gate.
	args := []any{namespace, refs}
	where := `namespace_id=$1 AND (from_ref=f.ref OR to_ref=f.ref)`
	if !opts.IncludeIncoming {
		args = append(args, s.relations.RelationsFor(namespace).SymmetricVerbs())
		where = `namespace_id=$1 AND (from_ref=f.ref OR (to_ref=f.ref AND relation = ANY($3::text[])))`
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
	args = append(args, offset, limit)
	rows, err := s.pool.Query(ctx,
		`SELECT f.ref, l.* FROM (SELECT DISTINCT unnest($2::text[]) AS ref) f
		CROSS JOIN LATERAL (SELECT `+linkColumns+` FROM links WHERE `+where+
			` ORDER BY asserted_at DESC, id OFFSET $`+strconv.Itoa(len(args)-1)+
			` LIMIT $`+strconv.Itoa(len(args))+`) l
		ORDER BY f.ref, l.asserted_at DESC, l.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		l, err := scanLink(rows, &ref)
		if err != nil {
			return nil, err
		}
		out[ref] = append(out[ref], l)
	}
	return out, rows.Err()
}

// openCandidate is THE definition of "in the review inbox", over a links row
// aliased l: an unretracted candidate that no person has confirmed yet. Both inbox
// reads (Candidates, CandidatesForRefs) render it, so the whole inbox and one
// entity's slice of it never disagree on membership.
//
// The NOT EXISTS is how an append-only store answers "already decided":
// ConfirmLink leaves the candidate untouched and appends a human row whose
// source_ref is confirmationSourceRef(candidate id) (ADR-0128 D4), so membership is
// decided by the presence of that row, never by editing the candidate. Any
// confirmation row counts, retracted or not: retracting a confirmation is a second
// human decision about the same proposal, not a request to review it again (and
// re-confirming would only replay the standing row). Literal 'candidate' and
// 'human' (not bound parameters) so the planner can match the partial indexes
// idx_links_review / idx_links_candidate_* / idx_links_confirmations (0038).
const openCandidate = `l.state = 'candidate' AND l.retracted_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM links c
		WHERE c.namespace_id = l.namespace_id AND c.mechanism = 'human'
		  AND c.source_ref = 'confirm:' || l.id)`

// candidateOrder is the review inbox's order (owner decision 2026-11-12, ADR-0128
// §3): most likely first, and among equals the one that has waited longest
// (asserted_at ASC) — the order the operator console sorts to (ui
// LinkReviewConsole orderCandidates). id makes it total. idx_links_review serves it.
const candidateOrder = `l.confidence DESC, l.asserted_at ASC, l.id`

// Candidates is the review inbox: open candidates (openCandidate) in
// candidateOrder.
func (s *PgLinkStore) Candidates(ctx context.Context, namespace string, limit int) ([]domain.Link, error) {
	namespace = orDefaultNamespace(namespace)
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+linkColumns+` FROM links l
		WHERE l.namespace_id=$1 AND `+openCandidate+`
		ORDER BY `+candidateOrder+`
		LIMIT $2`,
		namespace, limit)
	if err != nil {
		return nil, err
	}
	return collectLinks(rows)
}

// CandidatesForRefs is the review inbox restricted to the candidates touching each
// ref (either endpoint), in the inbox's own order, with Offset and Limit applied
// PER REF — the LinksForRefs shape. The subject filter is in SQL, before the
// limit: reading the whole inbox to a page and keeping the subject's rows lost a
// subject's candidates behind every higher-confidence candidate elsewhere
// (T-20261112-entity-op-s-candidates-read-cuts-the-whole).
func (s *PgLinkStore) CandidatesForRefs(ctx context.Context, namespace string, refs []string, offset, limit int) (map[string][]domain.Link, error) {
	namespace = orDefaultNamespace(namespace)
	out := map[string][]domain.Link{}
	if len(refs) == 0 {
		return out, nil
	}
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT f.ref, x.* FROM (SELECT DISTINCT unnest($2::text[]) AS ref) f
		CROSS JOIN LATERAL (SELECT `+linkColumns+` FROM links l
			WHERE l.namespace_id=$1 AND (l.from_ref=f.ref OR l.to_ref=f.ref) AND `+openCandidate+`
			ORDER BY `+candidateOrder+` OFFSET $3 LIMIT $4) x
		ORDER BY f.ref, x.confidence DESC, x.asserted_at ASC, x.id`,
		namespace, refs, max(offset, 0), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		l, err := scanLink(rows, &ref)
		if err != nil {
			return nil, err
		}
		out[ref] = append(out[ref], l)
	}
	return out, rows.Err()
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

// scanLink scans linkColumns, after any leading columns the query selected
// first (LinksForRefs selects the ref each row was found for).
func scanLink(row pgx.Row, leading ...any) (domain.Link, error) {
	var l domain.Link
	var evidenceID *string
	dest := append(leading, &l.ID, &l.NamespaceID, &l.Family, &l.FromRef, &l.ToRef, &l.Relation,
		&l.State, &l.Mechanism, &l.Producer, &l.Confidence, &evidenceID, &l.AssertedBy,
		&l.AssertedAt, &l.RecordedAt, &l.ValidFrom, &l.ValidTo, &l.RetractedAt,
		&l.SourceRef)
	if err := row.Scan(dest...); err != nil {
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
