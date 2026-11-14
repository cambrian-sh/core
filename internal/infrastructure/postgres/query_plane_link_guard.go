package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// LinkReadable reports whether link linkID exists in namespace AND scope may
// read it — judged on the link's own evidence, exactly as every link this plane
// returns is (filterLinks; owner fence included, fail closed on unresolvable
// evidence).
//
// It is the guard in front of the operator's link WRITES (ConfirmLink,
// RetractLink; T-20261107-operator-confirmlink-retractlink-act-on-and): the
// review inbox no longer lists a candidate proposed from someone's owner:X
// evidence, so acting on one by id must not be possible either, and a hidden
// link must answer exactly as an absent one. Hence (false, nil) for both — the
// caller cannot tell them apart from this answer, by design.
//
// A nil plane reads nothing: (false, nil).
func (p *PgQueryPlane) LinkReadable(ctx context.Context, namespace, linkID string, scope *domain.TagPredicate) (bool, error) {
	if p == nil || p.pool == nil {
		return false, nil
	}
	if scope == nil {
		// No predicate is no read (fail closed), never an unfiltered one.
		return false, nil
	}
	l, err := scanLink(p.pool.QueryRow(ctx,
		`SELECT `+linkColumns+` FROM links WHERE namespace_id=$1 AND id=$2`,
		orDefaultNamespace(namespace), linkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("link readable: %w", err)
	}
	kept, err := p.filterLinks(ctx, scope, []domain.Link{l})
	if err != nil {
		return false, fmt.Errorf("link readable: %w", err)
	}
	return len(kept) == 1, nil
}
