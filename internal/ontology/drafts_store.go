package ontology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// draftEvent is one row of ontology_draft_events (migration 0041).
type draftEvent struct {
	DraftID        string
	State          domain.OntologyDraftState
	ApprovalID     string
	Actor          string
	Detail         string
	AppliedVersion int64
	At             time.Time
}

// draftStore persists drafts and their state log. Both are append-only: a draft
// row is written once, and a state is a new event row, never an update.
type draftStore interface {
	// insertDraft writes d once; inserted=false when its id already exists.
	insertDraft(ctx context.Context, d domain.OntologyDraft) (inserted bool, err error)
	// getDraft reads one draft with its state folded from the event log.
	getDraft(ctx context.Context, id string) (d domain.OntologyDraft, found bool, err error)
	// appendEvent records a state; inserted=false when that state, or (for a
	// terminal state) any terminal state, is already recorded.
	appendEvent(ctx context.Context, e draftEvent) (inserted bool, err error)
	// awaiting lists drafts whose approval was raised and that have no terminal
	// state yet.
	awaiting(ctx context.Context) ([]domain.OntologyDraft, error)
}

type pgDraftStore struct{ pool *pgxpool.Pool }

func (s pgDraftStore) insertDraft(ctx context.Context, d domain.OntologyDraft) (bool, error) {
	change, err := json.Marshal(d.Change)
	if err != nil {
		return false, err
	}
	changes, err := json.Marshal(d.Changes)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO ontology_drafts
		(draft_id, namespace, change, proposer, base_version, class, changes, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (draft_id) DO NOTHING`,
		d.ID, d.Namespace, change, d.Proposer, d.BaseVersion, string(d.Class), changes, d.Reason, d.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("ontology draft %s: insert: %w", d.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// draftSelect folds the event log into the draft: the pending event names the
// approval; the one terminal event, when present, is the state.
const draftSelect = `
	SELECT d.draft_id, d.namespace, d.change, d.proposer, d.base_version, d.class, d.changes, d.reason, d.created_at,
	       p.approval_id, p.actor, p.at,
	       t.state, t.approval_id, t.actor, t.detail, t.applied_version, t.at
	FROM ontology_drafts d
	LEFT JOIN ontology_draft_events p ON p.draft_id = d.draft_id AND p.state = 'pending'
	LEFT JOIN ontology_draft_events t ON t.draft_id = d.draft_id AND t.state <> 'pending'`

func scanDraft(row pgx.Row) (domain.OntologyDraft, error) {
	var d domain.OntologyDraft
	var change, changes []byte
	var class string
	var pApproval, pActor, tState, tApproval, tActor, tDetail *string
	var pAt, tAt *time.Time
	var tVersion *int64
	if err := row.Scan(&d.ID, &d.Namespace, &change, &d.Proposer, &d.BaseVersion, &class, &changes, &d.Reason, &d.CreatedAt,
		&pApproval, &pActor, &pAt, &tState, &tApproval, &tActor, &tDetail, &tVersion, &tAt); err != nil {
		return d, err
	}
	d.Class = domain.OntologyChangeClass(class)
	if err := json.Unmarshal(change, &d.Change); err != nil {
		return d, fmt.Errorf("ontology draft %s: change: %w", d.ID, err)
	}
	if err := json.Unmarshal(changes, &d.Changes); err != nil {
		return d, fmt.Errorf("ontology draft %s: changes: %w", d.ID, err)
	}
	d.State = domain.OntologyDraftPending
	if pApproval != nil {
		d.ApprovalID, d.StateBy, d.StateAt = *pApproval, *pActor, *pAt
	}
	if tState != nil {
		d.State, d.StateBy, d.Detail, d.StateAt = domain.OntologyDraftState(*tState), *tActor, *tDetail, *tAt
		if d.ApprovalID == "" {
			d.ApprovalID = *tApproval
		}
		if tVersion != nil {
			d.AppliedVersion = *tVersion
		}
	}
	return d, nil
}

func (s pgDraftStore) getDraft(ctx context.Context, id string) (domain.OntologyDraft, bool, error) {
	d, err := scanDraft(s.pool.QueryRow(ctx, draftSelect+` WHERE d.draft_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("ontology draft %s: read: %w", id, err)
	}
	return d, true, nil
}

func (s pgDraftStore) appendEvent(ctx context.Context, e draftEvent) (bool, error) {
	var version *int64
	if e.AppliedVersion > 0 {
		version = &e.AppliedVersion
	}
	// ON CONFLICT DO NOTHING covers both the primary key (this state again) and
	// the terminal index (another terminal state won): the first one stands.
	tag, err := s.pool.Exec(ctx, `INSERT INTO ontology_draft_events
		(draft_id, state, approval_id, actor, detail, applied_version, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		e.DraftID, string(e.State), e.ApprovalID, e.Actor, e.Detail, version, e.At)
	if err != nil {
		return false, fmt.Errorf("ontology draft %s: record %s: %w", e.DraftID, e.State, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s pgDraftStore) awaiting(ctx context.Context) ([]domain.OntologyDraft, error) {
	rows, err := s.pool.Query(ctx, draftSelect+` WHERE p.approval_id IS NOT NULL AND p.approval_id <> '' AND t.state IS NULL
		ORDER BY d.created_at, d.draft_id`)
	if err != nil {
		return nil, fmt.Errorf("ontology drafts: list awaiting: %w", err)
	}
	defer rows.Close()
	var out []domain.OntologyDraft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
