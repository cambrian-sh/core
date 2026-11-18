package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cambrian-sh/core/domain"
)

// The review lane over PAIRS (owner decisions 2026-11-18, ENG-2030).
//
// One claim — the same (namespace, family, from, to, relation) — is several rows:
// one per producer lane and per supporting source (ADR-0128 D5, "corroboration is
// information"), plus a mapping's deferred crosswalk. Each row keeps its own
// evidence and row classification, so a reader allowed to read ANY of them reaches
// the claim. The review inbox shows the claim ONCE, and the pair-level confirm and
// reject act on its rows:
//
//   - confirm = one confirmation per open candidate row of the pair the reviewer
//     can read, each through confirmTx (its own evidence, classification and
//     supporting evidence; source_ref confirm:<id>), in one transaction;
//   - reject  = retract every open candidate row of the pair the reviewer can read
//     and append ONE rejection row (domain.IsPairRejection) — the stored "never ask
//     me this again" for the claim, which hides the pair's later candidates from
//     the inbox (openCandidate) and stops every producer lane proposing it.
//
// Which rows the reviewer can read is the query plane's decision (ReviewPairs,
// ReadablePair); this file only reads and writes rows.

// pairRowLimit bounds the rows read for one pair. A claim with more than this
// many supporting rows is already the best-corroborated claim in the deployment.
const pairRowLimit = defaultIdentityLimit

// pairRejectionSourceRef is the rejection row's dedup member: a digest of the
// claim, so a second reject of the same pair is a no-op.
func pairRejectionSourceRef(k domain.LinkPairKey) string {
	h := sha256.Sum256([]byte(k.Family + "\x00" + k.FromRef + "\x00" + k.ToRef + "\x00" + k.Relation))
	return domain.LinkRejectionSourceRefPrefix + hex.EncodeToString(h[:16])
}

// liveConfirmed is a confirmed, unretracted row (over alias l): what makes a
// later candidate of the pair an "extend the confirmation?" card.
const liveConfirmed = `l.state = 'confirmed' AND l.retracted_at IS NULL`

// CandidatePairs is the review inbox grouped by claim: the pairs holding at least
// one open candidate (openCandidate — the one inbox definition), ordered like the
// row inbox by their best row (highest confidence, then the longest-waiting),
// each with its open candidates and its live confirmed rows. The grouping is in
// SQL, before the limit, so a pair's rows never straddle two pages.
func (s *PgLinkStore) CandidatePairs(ctx context.Context, namespace string, limit int) ([]domain.LinkPair, error) {
	namespace = orDefaultNamespace(namespace)
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.family, l.from_ref, l.to_ref, l.relation
		FROM links l
		WHERE l.namespace_id=$1 AND `+openCandidate+`
		GROUP BY l.family, l.from_ref, l.to_ref, l.relation
		ORDER BY max(l.confidence) DESC, min(l.asserted_at) ASC, l.from_ref, l.to_ref, l.relation, l.family
		LIMIT $2`, namespace, limit)
	if err != nil {
		return nil, fmt.Errorf("link pairs: %w", err)
	}
	var keys []domain.LinkPairKey
	for rows.Next() {
		k := domain.LinkPairKey{NamespaceID: namespace}
		if err := rows.Scan(&k.Family, &k.FromRef, &k.ToRef, &k.Relation); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.pairRows(ctx, namespace, keys)
}

// PairLinks returns one pair's open candidates and live confirmed rows.
func (s *PgLinkStore) PairLinks(ctx context.Context, key domain.LinkPairKey) (domain.LinkPair, error) {
	key = key.Canonical()
	key.NamespaceID = orDefaultNamespace(key.NamespaceID)
	pairs, err := s.pairRows(ctx, key.NamespaceID, []domain.LinkPairKey{key})
	if err != nil {
		return domain.LinkPair{}, err
	}
	return pairs[0], nil
}

// pairRows reads every key's open candidates (inbox order) and live confirmed rows
// in one statement, and returns one LinkPair per key in the keys' order.
func (s *PgLinkStore) pairRows(ctx context.Context, namespace string, keys []domain.LinkPairKey) ([]domain.LinkPair, error) {
	out := make([]domain.LinkPair, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	fam := make([]string, len(keys))
	from := make([]string, len(keys))
	to := make([]string, len(keys))
	rel := make([]string, len(keys))
	for i, k := range keys {
		out[i].Key = k
		fam[i], from[i], to[i], rel[i] = k.Family, k.FromRef, k.ToRef, k.Relation
	}
	rows, err := s.pool.Query(ctx, `
		SELECT k.ord, x.* FROM unnest($2::text[], $3::text[], $4::text[], $5::text[])
			WITH ORDINALITY AS k(family, from_ref, to_ref, relation, ord)
		CROSS JOIN LATERAL (SELECT `+linkColumns+` FROM links l
			WHERE l.namespace_id=$1 AND l.family=k.family AND l.from_ref=k.from_ref
			  AND l.to_ref=k.to_ref AND l.relation=k.relation
			  AND ((`+openCandidate+`) OR (`+liveConfirmed+`))
			ORDER BY `+candidateOrder+` LIMIT $6) x
		ORDER BY k.ord, x.confidence DESC, x.asserted_at ASC, x.id`,
		namespace, fam, from, to, rel, pairRowLimit)
	if err != nil {
		return nil, fmt.Errorf("link pair rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ord int64
		l, err := scanLink(rows, &ord)
		if err != nil {
			return nil, err
		}
		i := int(ord) - 1
		if l.State == domain.LinkStateConfirmed {
			out[i].Confirmed = append(out[i].Confirmed, l)
		} else {
			out[i].Candidates = append(out[i].Candidates, l)
		}
	}
	return out, rows.Err()
}

// ConfirmLinks confirms several candidate rows in ONE transaction, each exactly as
// ConfirmLink confirms one (confirmTx): a refusal of any row (retracted, a typed
// rule) rolls the whole act back, so a pair is never half-confirmed. Returns the
// confirmation rows in ids' order.
func (s *PgLinkStore) ConfirmLinks(ctx context.Context, namespace string, ids []string, actor string) ([]domain.Link, error) {
	namespace = orDefaultNamespace(namespace)
	if actor == "" {
		return nil, fmt.Errorf("link confirm: an actor is required: %w", domain.ErrLinkRefused)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	out := make([]domain.Link, 0, len(ids))
	for _, id := range ids {
		c, err := s.confirmTx(ctx, tx, namespace, id, actor)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, tx.Commit(ctx)
}

// RejectLinkPair retracts the given candidate rows of one pair (only rows that
// belong to the pair and are live; RetractLink's stamp, nothing else) and appends
// the pair's rejection row, in one transaction. Returns how many rows it
// retracted. Idempotent: a second reject retracts nothing and the rejection row's
// natural key (pairRejectionSourceRef) admits no second row.
//
// The rejection row cites no evidence (a person is the evidence, ADR-0128 D3) and
// is born retracted, so no read path ever returns it as an assertion: every
// fenced read requires readable evidence (guardNodes fails closed on none) and
// every default read skips retracted rows. Erasure leaves it, because it holds no
// content of the erased rows — only the pair's refs.
func (s *PgLinkStore) RejectLinkPair(ctx context.Context, key domain.LinkPairKey, ids []string, actor string) (int, error) {
	key = key.Canonical()
	key.NamespaceID = orDefaultNamespace(key.NamespaceID)
	if actor == "" {
		return 0, fmt.Errorf("link reject: an actor is required: %w", domain.ErrLinkRefused)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	tag, err := tx.Exec(ctx, `
		UPDATE links SET state=$7, retracted_at=now()
		WHERE namespace_id=$1 AND family=$2 AND from_ref=$3 AND to_ref=$4 AND relation=$5
		  AND id = ANY($6) AND retracted_at IS NULL`,
		key.NamespaceID, key.Family, key.FromRef, key.ToRef, key.Relation, ids, domain.LinkStateRetracted)
	if err != nil {
		return 0, fmt.Errorf("link reject: %w", err)
	}
	reg := s.relations.RelationsFor(key.NamespaceID)
	rejection := domain.Link{
		ID:          "lnk_" + uuid.NewString(),
		NamespaceID: key.NamespaceID,
		Family:      key.Family,
		FromRef:     key.FromRef,
		ToRef:       key.ToRef,
		Relation:    key.Relation,
		State:       domain.LinkStateRetracted,
		Mechanism:   domain.LinkMechanismHuman,
		Confidence:  1.0,
		AssertedBy:  actor,
		AssertedAt:  time.Now().UTC(),
		SourceRef:   pairRejectionSourceRef(key),
	}
	if err := reg.ValidateLink(rejection); err != nil {
		return 0, err
	}
	created, err := insertLink(ctx, tx, rejection, reg.Version())
	if err != nil {
		return 0, fmt.Errorf("link reject: %w", err)
	}
	if created {
		// Born retracted: the insert has no retracted_at column, so the same
		// transaction stamps it before anyone can read the row live.
		if _, err := tx.Exec(ctx, `UPDATE links SET retracted_at=now() WHERE id=$1`, rejection.ID); err != nil {
			return 0, fmt.Errorf("link reject: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
