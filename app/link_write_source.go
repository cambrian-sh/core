package app

import (
	"context"
	"fmt"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// linkWriteSource binds the operator plane's review lane to the identity plane's
// store (contract 0098; five-planes step 2, FIVE-PLANES-BUILD.md).
//
// It is a straight adapter and deliberately holds no rules of its own. Every
// refusal the identity plane owes a caller — undeclared verb, trust ceiling,
// admissibility — is enforced inside the store, because a rule that lived here
// would bind the operator console and nothing else, and the whole point of the
// plane is that a heuristic cannot promote itself into the answer set no matter
// who wrote it. The composition root's job is to say WHICH store; it is not to
// re-decide what the store already decided.
//
// It lives beside configWriteSource for the same reason that one does: the
// operator package speaks its own DTOs so a store change is not a contract
// change, and something has to translate.
type linkWriteSource struct {
	links domain.LinkStore
	// review is the query plane's scoped read of the candidate inbox. The
	// inbox is a READ, and reads of the substrate go through the plane, which
	// judges each candidate on its own evidence (owner fence included). nil
	// review lists nothing rather than falling back to the raw store: an
	// unfenced fallback would be the leak this field closes
	// (T-20261107-link-review-inbox-and-entity-op-candidates-skip).
	review interface {
		ReviewCandidates(ctx context.Context, namespace string, limit int, scope *domain.TagPredicate) ([]domain.Link, error)
		// LinkReadable guards the review WRITES (ConfirmLink, RetractLink): the
		// link must exist and be readable at the operator plane's scope, judged
		// on its own evidence as the inbox judges it.
		LinkReadable(ctx context.Context, namespace, linkID string, scope *domain.TagPredicate) (bool, error)
		// ReviewPairs / ReadablePair are the inbox grouped by claim and one claim's
		// readable rows (contract 0109, ENG-2030): the pair-level writes act on
		// exactly the candidates ReadablePair returns.
		ReviewPairs(ctx context.Context, namespace string, limit int, scope *domain.TagPredicate) ([]domain.LinkPair, error)
		ReadablePair(ctx context.Context, key domain.LinkPairKey, scope *domain.TagPredicate) (domain.LinkPair, error)
	}
	// pairs writes the pair-level confirm and reject (postgres.PgLinkStore). nil
	// refuses them (fail closed), like a nil review.
	pairs linkPairWriter
}

// linkPairWriter is the store's pair-level write half (consumer-side, AGENTS.md
// §3.1): several confirmations in one transaction, and the pair rejection.
type linkPairWriter interface {
	ConfirmLinks(ctx context.Context, namespace string, ids []string, actor string) ([]domain.Link, error)
	RejectLinkPair(ctx context.Context, key domain.LinkPairKey, ids []string, actor string) (int, error)
}

var _ operator.LinkWriter = linkWriteSource{}

// linkRecord flattens the domain's optional times into the operator DTO's zero
// values. nil and the zero instant mean the same thing here — unbounded, or not
// retracted — and the mapper on the far side renders both as 0.
func linkRecord(l domain.Link) operator.LinkRecord {
	r := operator.LinkRecord{
		ID: l.ID, NamespaceID: l.NamespaceID, Family: l.Family,
		FromRef: l.FromRef, ToRef: l.ToRef, Relation: l.Relation,
		State: l.State, Mechanism: l.Mechanism, Producer: l.Producer,
		Confidence: l.Confidence, EvidenceID: string(l.EvidenceID),
		AssertedBy: l.AssertedBy, AssertedAt: l.AssertedAt,
		RecordedAt: l.RecordedAt, SourceRef: l.SourceRef,
	}
	for _, e := range l.SupportingEvidence {
		r.SupportingEvidenceIDs = append(r.SupportingEvidenceIDs, string(e))
	}
	deref := func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	}
	r.ValidFrom = deref(l.ValidFrom)
	r.ValidTo = deref(l.ValidTo)
	r.RetractedAt = deref(l.RetractedAt)
	return r
}

// requireReadable refuses a review write on a link the operator plane cannot
// read (T-20261107-operator-confirmlink-retractlink-act-on-and). The inbox does
// not LIST a candidate proposed from someone's owner:X evidence (ScopeSystem
// keeps the owner fence), so it must not be confirmable or retractable by an id
// learned elsewhere (a log line, an audit entry, a guess) either — and
// ConfirmLink would otherwise hand back the row's from/to refs and evidence id.
//
// A hidden link answers EXACTLY as an absent one: the same ErrLinkRefused and
// the same message the store gives for an unknown id, so the refusal cannot be
// used to probe for existence. No review plane ⇒ no write (fail closed; the
// inbox lists nothing in that case too).
func (w linkWriteSource) requireReadable(ctx context.Context, op, namespace, linkID string) error {
	ok := false
	if w.review != nil {
		var err error
		if ok, err = w.review.LinkReadable(ctx, namespace, linkID, domain.ScopeSystem); err != nil {
			return err
		}
	}
	if !ok {
		ns := namespace
		if ns == "" {
			ns = "default" // the store's own default (postgres.orDefaultNamespace), so the message matches
		}
		return fmt.Errorf("link %s: no link %q in namespace %q: %w", op, linkID, ns, domain.ErrLinkRefused)
	}
	return nil
}

func (w linkWriteSource) ConfirmLink(ctx context.Context, namespace, linkID, actor string) (operator.LinkRecord, error) {
	if err := w.requireReadable(ctx, "confirm", namespace, linkID); err != nil {
		return operator.LinkRecord{}, err
	}
	l, err := w.links.ConfirmLink(ctx, namespace, linkID, actor)
	if err != nil {
		return operator.LinkRecord{}, err
	}
	return linkRecord(l), nil
}

func (w linkWriteSource) RetractLink(ctx context.Context, namespace, linkID, actor, reason string) error {
	if err := w.requireReadable(ctx, "retract", namespace, linkID); err != nil {
		return err
	}
	return w.links.RetractLink(ctx, namespace, linkID, actor, reason)
}

func (w linkWriteSource) RetractLinksByProducer(ctx context.Context, namespace, producer, actor string) (int, error) {
	return w.links.RetractByProducer(ctx, namespace, producer, actor)
}

// ListLinkCandidates reads the review inbox at domain.ScopeSystem — the operator
// plane's scope, which waives policy tag terms but NOT the owner fence
// (domain/owner_scope.go). A candidate proposed from someone's owner:X evidence
// is therefore not listed to the operator, and nothing reports how many were
// withheld. Only ScopeMaintenance would list it, and no operator RPC uses it.
func (w linkWriteSource) ListLinkCandidates(ctx context.Context, namespace string, limit int) ([]operator.LinkRecord, error) {
	if w.review == nil {
		return []operator.LinkRecord{}, nil
	}
	rows, err := w.review.ReviewCandidates(ctx, namespace, limit, domain.ScopeSystem)
	if err != nil {
		return nil, err
	}
	out := make([]operator.LinkRecord, 0, len(rows))
	for _, l := range rows {
		out = append(out, linkRecord(l))
	}
	return out, nil
}

func pairKeyDomain(k operator.LinkPairKey) domain.LinkPairKey {
	ns := k.NamespaceID
	if ns == "" {
		ns = "default" // the store's own default (postgres.orDefaultNamespace)
	}
	return domain.LinkPairKey{NamespaceID: ns, Family: k.Family, FromRef: k.FromRef, ToRef: k.ToRef, Relation: k.Relation}.Canonical()
}

func linkRecords(in []domain.Link) []operator.LinkRecord {
	out := make([]operator.LinkRecord, 0, len(in))
	for _, l := range in {
		out = append(out, linkRecord(l))
	}
	return out
}

// ListLinkPairs reads the inbox grouped by claim at domain.ScopeSystem, the same
// scope and the same per-row judgement as ListLinkCandidates.
func (w linkWriteSource) ListLinkPairs(ctx context.Context, namespace string, limit int) ([]operator.LinkPairRecord, error) {
	if w.review == nil {
		return []operator.LinkPairRecord{}, nil
	}
	pairs, err := w.review.ReviewPairs(ctx, namespace, limit, domain.ScopeSystem)
	if err != nil {
		return nil, err
	}
	out := make([]operator.LinkPairRecord, 0, len(pairs))
	for _, p := range pairs {
		rec := operator.LinkPairRecord{
			Key: operator.LinkPairKey{
				NamespaceID: p.Key.NamespaceID, Family: p.Key.Family,
				FromRef: p.Key.FromRef, ToRef: p.Key.ToRef, Relation: p.Key.Relation,
			},
			Candidates:       linkRecords(p.Candidates),
			Confirmations:    linkRecords(p.Confirmed),
			AlreadyConfirmed: len(p.Confirmed) > 0,
		}
		for _, c := range p.Candidates {
			rec.Confidence = max(rec.Confidence, c.Confidence)
		}
		out = append(out, rec)
	}
	return out, nil
}

// readablePair is requireReadable for a claim: the open candidates the operator
// plane may read, or the absent-link refusal when there are none, so a hidden
// claim answers exactly as an absent one.
func (w linkWriteSource) readablePair(ctx context.Context, op string, key domain.LinkPairKey) ([]string, error) {
	var ids []string
	if w.review != nil && w.pairs != nil {
		p, err := w.review.ReadablePair(ctx, key, domain.ScopeSystem)
		if err != nil {
			return nil, err
		}
		for _, c := range p.Candidates {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("link pair %s: no open candidate for %s %s %s in namespace %q: %w",
			op, key.FromRef, key.Relation, key.ToRef, key.NamespaceID, domain.ErrLinkRefused)
	}
	return ids, nil
}

// ConfirmLinkPair confirms every readable open candidate of the claim, one
// confirmation per row (owner decision 2026-11-18 Q1), atomically.
func (w linkWriteSource) ConfirmLinkPair(ctx context.Context, pair operator.LinkPairKey, actor string) ([]operator.LinkRecord, error) {
	key := pairKeyDomain(pair)
	ids, err := w.readablePair(ctx, "confirm", key)
	if err != nil {
		return nil, err
	}
	confirmed, err := w.pairs.ConfirmLinks(ctx, key.NamespaceID, ids, actor)
	if err != nil {
		return nil, err
	}
	return linkRecords(confirmed), nil
}

// RejectLinkPair retracts every readable open candidate of the claim and records
// the claim's rejection (owner decision 2026-11-18 Q3). reason is audited by the
// operator lane, never stored on a row.
func (w linkWriteSource) RejectLinkPair(ctx context.Context, pair operator.LinkPairKey, actor, reason string) (int, error) {
	_ = reason // recorded by the operator mutation lane's audit entry; a link row never gains a field
	key := pairKeyDomain(pair)
	ids, err := w.readablePair(ctx, "reject", key)
	if err != nil {
		return 0, err
	}
	return w.pairs.RejectLinkPair(ctx, key, ids, actor)
}
