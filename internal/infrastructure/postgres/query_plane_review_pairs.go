package postgres

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ReviewPairs is the review inbox grouped by claim (ENG-2030; owner decisions
// 2026-11-18), read through the plane: every candidate and every confirmation of
// every pair is judged on its OWN evidence and row classification by filterLinks,
// exactly as ReviewCandidates judges rows. A pair is listed when at least one of
// its candidates is readable, and lists only the readable rows; Confirmed holds
// only readable confirmations, so "already confirmed via another source" is said
// only about a confirmation the reader could see. Like ReviewCandidates, the page
// is filled from at most `limit` pairs and can come back short, never wider.
func (p *PgQueryPlane) ReviewPairs(ctx context.Context, namespace string, limit int, scope *domain.TagPredicate) ([]domain.LinkPair, error) {
	if scope == nil {
		return nil, domain.ErrQueryScopeMissing
	}
	if p == nil || p.links == nil {
		return nil, nil
	}
	pairs, err := p.links.CandidatePairs(ctx, namespace, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.LinkPair, 0, len(pairs))
	for _, pr := range pairs {
		kept, err := p.readablePair(ctx, scope, pr)
		if err != nil {
			return nil, err
		}
		if len(kept.Candidates) > 0 {
			out = append(out, kept)
		}
	}
	return out, nil
}

// ReadablePair is one pair's rows as scope may read them — the guard in front of
// the pair-level confirm and reject, which act on exactly these candidates and
// nothing the reviewer could not see. A pair with no readable candidate answers
// as an absent one (empty Candidates), as LinkReadable does for one row.
func (p *PgQueryPlane) ReadablePair(ctx context.Context, key domain.LinkPairKey, scope *domain.TagPredicate) (domain.LinkPair, error) {
	if scope == nil {
		return domain.LinkPair{Key: key}, domain.ErrQueryScopeMissing
	}
	if p == nil || p.links == nil {
		return domain.LinkPair{Key: key}, nil
	}
	pr, err := p.links.PairLinks(ctx, key)
	if err != nil {
		return domain.LinkPair{Key: key}, err
	}
	return p.readablePair(ctx, scope, pr)
}

func (p *PgQueryPlane) readablePair(ctx context.Context, scope *domain.TagPredicate, pr domain.LinkPair) (domain.LinkPair, error) {
	cands, err := p.filterLinks(ctx, scope, pr.Candidates)
	if err != nil {
		return domain.LinkPair{}, err
	}
	conf, err := p.filterLinks(ctx, scope, pr.Confirmed)
	if err != nil {
		return domain.LinkPair{}, err
	}
	return domain.LinkPair{Key: pr.Key, Candidates: cands, Confirmed: conf}, nil
}
