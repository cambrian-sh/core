package app

import (
	"context"
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
	}
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

func (w linkWriteSource) ConfirmLink(ctx context.Context, namespace, linkID, actor string) (operator.LinkRecord, error) {
	l, err := w.links.ConfirmLink(ctx, namespace, linkID, actor)
	if err != nil {
		return operator.LinkRecord{}, err
	}
	return linkRecord(l), nil
}

func (w linkWriteSource) RetractLink(ctx context.Context, namespace, linkID, actor, reason string) error {
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
