package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// The substrate catalog's three LISTINGS (domain.SubstrateCatalog Links, Events,
// EntityHandles; T-20261113-link-building-suite-reads-the-kernel-s-postgres, owner
// decision 2026-11-14). Before them the only way to see the identity plane's links,
// its minted handles, or which event a source record became was SQL against the
// substrate tables — the link_building benchmark did exactly that, coupling a
// score to core's private table layout. They are read through the same fenced
// catalog as Overview, so a listing can never show a row the bound scope may not
// read:
//   - links:    filtered on each link's OWN evidence (filterLinks, the entity op's
//     and the review inbox's rule); an assertion without evidence survives only
//     an unfiltered scope;
//   - events:   through evidence_id narrowed by the row classification
//     (sqlfence.TypedRowExists, ADR-0158 D2) and never a retired
//     row (ADR-0147 D6);
//   - entities: through first_seen_evidence narrowed by first_seen_classification
//     (the minting row's classification, ADR-0158 D4), as the identity scans
//     read them.
//
// Every listing is paged in id order with an exclusive id cursor, and confined to
// one namespace (an empty one is refused).

var errListingNamespace = errors.New("substrate catalog: a namespace is required")

func (c pgCatalog) checkListing(namespace string) error {
	if err := c.check(); err != nil {
		return err
	}
	if strings.TrimSpace(namespace) == "" {
		return errListingNamespace
	}
	return nil
}

// Links — ENFORCED per row on the link's own evidence, AFTER the page is read, so
// the page is bounded by what was scanned and the cursor is the last row scanned.
func (c pgCatalog) Links(ctx context.Context, f domain.LinkListFilter) (domain.LinkPage, error) {
	var out domain.LinkPage
	if err := c.checkListing(f.Namespace); err != nil {
		return out, err
	}
	limit := boundLimit(f.Limit)
	a := &sqlArgs{}
	q := `SELECT ` + linkColumns + ` FROM links WHERE namespace_id=` + a.add(f.Namespace) +
		` AND id > ` + a.add(f.AfterID)
	if len(f.States) > 0 {
		q += ` AND state = ANY(` + a.add(f.States) + `)`
	}
	if f.ProducerPrefix != "" {
		q += ` AND producer LIKE ` + a.add(likePrefix(f.ProducerPrefix)) + ` ESCAPE '\'`
	}
	if !f.IncludeRetracted {
		q += ` AND retracted_at IS NULL`
	}
	q += ` ORDER BY id LIMIT ` + a.add(limit)
	rows, err := c.plane.pool.Query(ctx, q, a.args...)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: links: %w", err)
	}
	var scanned []domain.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			rows.Close()
			return out, fmt.Errorf("substrate catalog: links: %w", err)
		}
		scanned = append(scanned, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("substrate catalog: links: %w", err)
	}
	if len(scanned) == limit {
		out.Next = scanned[len(scanned)-1].ID
	}
	kept, err := c.plane.filterLinks(ctx, c.scope, scanned)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: links: %w", err)
	}
	out.Links = kept
	return out, nil
}

// Events — ENFORCED through each event's evidence; retired rows never listed.
func (c pgCatalog) Events(ctx context.Context, f domain.EventListFilter) (domain.EventPage, error) {
	var out domain.EventPage
	if err := c.checkListing(f.Namespace); err != nil {
		return out, err
	}
	limit := boundLimit(f.Limit)
	a := &sqlArgs{}
	q := `SELECT x.id, x.event_type, x.source_ref, x.occurred_at FROM events x
		WHERE x.namespace_id=` + a.add(f.Namespace) + ` AND x.source_ref <> ''
		  AND x.id > ` + a.add(f.AfterID)
	if f.SourceRefPrefix != "" {
		q += ` AND x.source_ref LIKE ` + a.add(likePrefix(f.SourceRefPrefix)) + ` ESCAPE '\'`
	}
	q += notRetired("x") + latestRevision("x") + a.viaTypedRow(c.scope, "x.evidence_id", "x.classification") +
		` ORDER BY x.id LIMIT ` + a.add(limit)
	rows, err := c.plane.pool.Query(ctx, q, a.args...)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: events: %w", err)
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.CatalogEvent, error) {
		var e domain.CatalogEvent
		err := r.Scan(&e.ID, &e.EventType, &e.SourceRef, &e.OccurredAt)
		return e, err
	})
	if err != nil {
		return out, fmt.Errorf("substrate catalog: events: %w", err)
	}
	if len(evs) == limit {
		out.Next = evs[len(evs)-1].ID
	}
	out.Events = evs
	return out, nil
}

// EntityHandles — ENFORCED through each handle's first-seen evidence and the
// minting row's classification (the typed-row fence, ADR-0158 D4).
func (c pgCatalog) EntityHandles(ctx context.Context, f domain.EntityHandleFilter) (domain.EntityHandlePage, error) {
	var out domain.EntityHandlePage
	if err := c.checkListing(f.Namespace); err != nil {
		return out, err
	}
	limit := boundLimit(f.Limit)
	a := &sqlArgs{}
	q := `SELECT en.id, en.kind FROM entities en
		WHERE en.namespace_id=` + a.add(f.Namespace) + ` AND en.id > ` + a.add(f.AfterID) +
		a.viaTypedRow(c.scope, "en.first_seen_evidence", "en.first_seen_classification") +
		` ORDER BY en.id LIMIT ` + a.add(limit)
	rows, err := c.plane.pool.Query(ctx, q, a.args...)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: entity handles: %w", err)
	}
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.CatalogHandle, error) {
		var h domain.CatalogHandle
		err := r.Scan(&h.ID, &h.Kind)
		return h, err
	})
	if err != nil {
		return out, fmt.Errorf("substrate catalog: entity handles: %w", err)
	}
	if len(hs) == limit {
		out.Next = hs[len(hs)-1].ID
	}
	out.Handles = hs
	return out, nil
}
