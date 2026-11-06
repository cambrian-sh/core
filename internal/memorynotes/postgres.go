package memorynotes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore is the Postgres Store over migration 0030_memory_notes.sql.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore builds the store and checks the schema is present, so a kernel on an
// unmigrated database finds out at boot rather than on the first write.
func NewPgStore(ctx context.Context, pool *pgxpool.Pool) (*PgStore, error) {
	if pool == nil {
		return nil, errors.New("memorynotes: nil pool")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('memory_notes', 'memory_notes_budgets', 'memory_notes_history')`).Scan(&n); err != nil {
		return nil, fmt.Errorf("memorynotes: schema check: %w", err)
	}
	if n != 3 {
		return nil, fmt.Errorf("memorynotes: schema missing (migration 0030_memory_notes not applied; found %d of 3 tables)", n)
	}
	return &PgStore{pool: pool}, nil
}

// Get implements Store.
func (p *PgStore) Get(ctx context.Context, key Key) (Document, error) {
	d := Document{Key: key}
	err := p.pool.QueryRow(ctx, `SELECT content, revision, deleted, updated_at, updated_by
		FROM memory_notes WHERE scope = $1 AND owner_id = $2`, string(key.Scope), key.OwnerID).
		Scan(&d.Content, &d.Revision, &d.Deleted, &d.UpdatedAt, &d.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{Key: key}, nil
	}
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

// List implements Store.
func (p *PgStore) List(ctx context.Context, f ListFilter) ([]Document, error) {
	rows, err := p.pool.Query(ctx, `SELECT scope, owner_id, content, revision, updated_at, updated_by
		FROM memory_notes
		WHERE NOT deleted AND ($1 = '' OR scope = $1) AND ($2 = '' OR owner_id = $2)
		ORDER BY scope, owner_id`, string(f.Scope), f.OwnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		var d Document
		var scope string
		if err := rows.Scan(&scope, &d.OwnerID, &d.Content, &d.Revision, &d.UpdatedAt, &d.UpdatedBy); err != nil {
			return nil, err
		}
		d.Scope = Scope(scope)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *PgStore) write(ctx context.Context, key Key, content string, expect int64, w WriteMeta, deleted bool) (Document, error) {
	if w.At.IsZero() {
		w.At = time.Now().UTC()
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Document{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	var prior string
	var tag interface{ RowsAffected() int64 }
	if expect == 0 {
		if deleted {
			return Document{}, ErrRevisionConflict
		}
		ct, err := tx.Exec(ctx, `INSERT INTO memory_notes (scope, owner_id, content, revision, deleted, updated_at, updated_by)
			VALUES ($1, $2, $3, 1, false, $4, $5) ON CONFLICT (scope, owner_id) DO NOTHING`,
			string(key.Scope), key.OwnerID, content, w.At, w.Actor)
		if err != nil {
			return Document{}, err
		}
		tag = ct
	} else {
		if deleted {
			if err := tx.QueryRow(ctx, `SELECT content FROM memory_notes WHERE scope = $1 AND owner_id = $2 AND revision = $3`,
				string(key.Scope), key.OwnerID, expect).Scan(&prior); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return Document{}, ErrRevisionConflict
				}
				return Document{}, err
			}
		}
		ct, err := tx.Exec(ctx, `UPDATE memory_notes SET content = $4, revision = revision + 1, deleted = $5,
			updated_at = $6, updated_by = $7
			WHERE scope = $1 AND owner_id = $2 AND revision = $3`,
			string(key.Scope), key.OwnerID, expect, content, deleted, w.At, w.Actor)
		if err != nil {
			return Document{}, err
		}
		tag = ct
	}
	if tag.RowsAffected() != 1 {
		return Document{}, ErrRevisionConflict
	}
	hist := content
	if deleted {
		hist = prior
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memory_notes_history (scope, owner_id, revision, op, actor, reason, content, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (scope, owner_id, revision) DO NOTHING`,
		string(key.Scope), key.OwnerID, expect+1, w.Op, w.Actor, w.Reason, hist, w.At); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Document{}, err
	}
	return Document{Key: key, Content: content, Revision: expect + 1, Deleted: deleted,
		UpdatedAt: w.At, UpdatedBy: w.Actor}, nil
}

// Put implements Store.
func (p *PgStore) Put(ctx context.Context, key Key, content string, expect int64, w WriteMeta) (Document, error) {
	return p.write(ctx, key, content, expect, w, false)
}

// Delete implements Store.
func (p *PgStore) Delete(ctx context.Context, key Key, expect int64, w WriteMeta) (Document, error) {
	return p.write(ctx, key, "", expect, w, true)
}

// GetBudget implements Store.
func (p *PgStore) GetBudget(ctx context.Context, scope Scope, owner string) (int, bool, error) {
	var b int
	err := p.pool.QueryRow(ctx, `SELECT budget_chars FROM memory_notes_budgets WHERE scope = $1 AND owner_id = $2`,
		string(scope), owner).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return b, true, nil
}

// SetBudget implements Store.
func (p *PgStore) SetBudget(ctx context.Context, scope Scope, owner string, budget int) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO memory_notes_budgets (scope, owner_id, budget_chars, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (scope, owner_id) DO UPDATE SET budget_chars = EXCLUDED.budget_chars, updated_at = now()`,
		string(scope), owner, budget)
	return err
}

// ClearBudget implements Store.
func (p *PgStore) ClearBudget(ctx context.Context, scope Scope, owner string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM memory_notes_budgets WHERE scope = $1 AND owner_id = $2`, string(scope), owner)
	return err
}

// History returns the writes of one document in revision order.
func (p *PgStore) History(ctx context.Context, key Key) ([]HistoryEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT revision, op, actor, reason, content, at FROM memory_notes_history
		WHERE scope = $1 AND owner_id = $2 ORDER BY revision`, string(key.Scope), key.OwnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		h := HistoryEntry{Key: key}
		if err := rows.Scan(&h.Revision, &h.Op, &h.Actor, &h.Reason, &h.Content, &h.At); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
