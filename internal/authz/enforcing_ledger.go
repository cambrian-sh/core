package authz

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// EnforcingLedger is the ONE read path of the run ledger (ADR-0145 D1). It wraps
// the raw domain.LedgerStore and applies the reader's scope predicate — taken from
// ctx (domain.WithScope), the only channel a by-id read has (AGENTS.md §2.4) — to
// every read, by id or by query. It is the ledger twin of EnforcingVectorStore:
//
//   - no predicate on ctx           → ErrScopeMissing (fail-closed, loud)
//   - an entry the predicate denies → absent (Get returns nil; Find omits it), so a
//     reader cannot learn that out-of-reach material exists
//   - the owner fence (ADR-0142)    → applies under Bypass too (TagPredicate.Allows)
//
// The decorator is constructed only by kernel.NewLedgerStack with the store as a
// required argument, never attached later (AGENTS.md §2.3), and it is the only
// ledger type any reader is handed. Row filtering is TagPredicate.Allows over the
// entry's kernel-derived tags — the same test the artifact read path applies
// (ArtifactReadable), not a second policy (§2.5) — applied to the entry's own
// tags AND to the tags of every entry it was derived from
// (domain.LedgerEntryReadable), so a step output is never easier to read than the
// tool results it was built from (T-20261108-ledger-step-outputs-do-not-inherit-the).
type EnforcingLedger struct {
	inner  domain.LedgerReader
	logger *slog.Logger
}

var _ domain.LedgerReader = (*EnforcingLedger)(nil)

// NewEnforcingLedger wraps inner with read enforcement. A nil logger falls back to
// slog.Default().
func NewEnforcingLedger(inner domain.LedgerReader, logger *slog.Logger) *EnforcingLedger {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingLedger{inner: inner, logger: logger}
}

func (l *EnforcingLedger) predicate(ctx context.Context, op string) (*domain.TagPredicate, error) {
	eff, ok := domain.ScopeFromContext(ctx)
	if !ok || eff == nil {
		l.logger.WarnContext(ctx, "authz: denied unfiltered ledger read (fail-closed)",
			slog.String("event", "authz_deny"),
			slog.String("reason", string(domain.ReasonNoPrincipal)),
			slog.String("op", op))
		return nil, ErrScopeMissing
	}
	return eff, nil
}

// Get — ENFORCED. A forbidden entry is reported as absent.
func (l *EnforcingLedger) Get(ctx context.Context, id string) (*domain.LedgerEntry, error) {
	eff, err := l.predicate(ctx, "LedgerGet")
	if err != nil {
		return nil, err
	}
	e, err := l.inner.Get(ctx, id)
	if err != nil || e == nil {
		return nil, err
	}
	if !domain.LedgerEntryReadable(eff, *e) {
		l.logger.DebugContext(ctx, "authz: filtered ledger read",
			slog.String("event", "authz_filtered"), slog.String("op", "LedgerGet"))
		return nil, nil
	}
	return e, nil
}

// ledgerFindOverfetch is how many candidates Find asks the store for per wanted
// row, so filtering in the decorator does not starve a reader whose scope excludes
// some of a run's entries.
const ledgerFindOverfetch = 4

// Find — ENFORCED. Survivors only, in the store's order, at most q.Limit.
func (l *EnforcingLedger) Find(ctx context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	eff, err := l.predicate(ctx, "LedgerFind")
	if err != nil {
		return nil, err
	}
	want := q.Limit
	inner := q
	if want > 0 {
		inner.Limit = want * ledgerFindOverfetch
	}
	rows, err := l.inner.Find(ctx, inner)
	if err != nil {
		return nil, err
	}
	out := make([]domain.LedgerEntry, 0, len(rows))
	for _, e := range rows {
		if !domain.LedgerEntryReadable(eff, e) {
			continue
		}
		out = append(out, e)
		if want > 0 && len(out) == want {
			break
		}
	}
	return out, nil
}
