package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgSkillLifecycleStore is the durable domain.SkillLifecycleStore (ADR-0139 D13)
// over the 0029_skill_lifecycle tables. Schema comes from the migration runner;
// this adapter never creates tables. Rules live in internal/skill/lifecycle; this
// is storage, with one exception the database also enforces: skill_ledger is
// append-only (a trigger refuses UPDATE and DELETE).
type PgSkillLifecycleStore struct {
	pool *pgxpool.Pool
}

var _ domain.SkillLifecycleStore = (*PgSkillLifecycleStore)(nil)

// NewPgSkillLifecycleStore wraps the shared pool (PgVectorAdapter.Pool()).
func NewPgSkillLifecycleStore(pool *pgxpool.Pool) *PgSkillLifecycleStore {
	return &PgSkillLifecycleStore{pool: pool}
}

// Ready reports whether the 0029 tables exist (an older or un-migrated database
// lacks them; the kernel then leaves the lifecycle unwired and unadvertised).
func (s *PgSkillLifecycleStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('skill_records','skill_ledger','skill_proposals','skill_versions','skill_run_uses')`).Scan(&n)
	return err == nil && n == 5
}

// nullTime is knowledge_store.go's (zero → NULL).

func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

const proposalCols = `id, name, skill_md, content_hash, rationale, evidence_cids, base_version, state,
	proposer_agent_id, principal_id, source_run_id, session_id, scan_verdict, scan_findings,
	created_at, decided_at, decided_by, decision_note`

func scanProposal(row pgx.Row) (domain.SkillProposal, error) {
	var p domain.SkillProposal
	var decided *time.Time
	err := row.Scan(&p.ID, &p.Name, &p.SkillMD, &p.ContentHash, &p.Rationale, &p.EvidenceCIDs, &p.BaseVersion, &p.State,
		&p.ProposerAgentID, &p.PrincipalID, &p.SourceRunID, &p.SessionID, &p.ScanVerdict, &p.ScanFindings,
		&p.CreatedAt, &decided, &p.DecidedBy, &p.DecisionNote)
	if decided != nil {
		p.DecidedAt = *decided
	}
	return p, err
}

// InsertProposal implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) InsertProposal(ctx context.Context, p domain.SkillProposal) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO skill_proposals (`+proposalCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (id) DO NOTHING`,
		p.ID, p.Name, p.SkillMD, p.ContentHash, p.Rationale, strs(p.EvidenceCIDs), p.BaseVersion, p.State,
		p.ProposerAgentID, p.PrincipalID, p.SourceRunID, p.SessionID, p.ScanVerdict, strs(p.ScanFindings),
		p.CreatedAt, nullTime(p.DecidedAt), p.DecidedBy, p.DecisionNote)
	if err != nil {
		return false, fmt.Errorf("insert skill proposal: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetProposal implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) GetProposal(ctx context.Context, id string) (domain.SkillProposal, bool, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM skill_proposals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillProposal{}, false, nil
	}
	if err != nil {
		return domain.SkillProposal{}, false, fmt.Errorf("get skill proposal: %w", err)
	}
	return p, true, nil
}

// ListProposals implements domain.SkillLifecycleStore (newest first).
func (s *PgSkillLifecycleStore) ListProposals(ctx context.Context, f domain.SkillProposalFilter) ([]domain.SkillProposal, int, error) {
	var conds []string
	var args []any
	add := func(col, val string) {
		if val != "" {
			args = append(args, val)
			conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	add("state", f.State)
	add("name", f.SkillName)
	add("proposer_agent_id", f.Proposer)
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM skill_proposals`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count skill proposals: %w", err)
	}
	q := `SELECT ` + proposalCols + ` FROM skill_proposals` + where + ` ORDER BY created_at DESC, id`
	q, args = limitOffset(q, args, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list skill proposals: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillProposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan skill proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func limitOffset(q string, args []any, limit, offset int) (string, []any) {
	if limit > 0 {
		args = append(args, limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if offset > 0 {
		args = append(args, offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	return q, args
}

const skillCols = `name, source, version, state, state_changed_at, current_since, locked,
	use_count, success_count, last_used_at, ledger_entry_id, ledger_seq, created_at`

func scanSkill(row pgx.Row) (domain.SkillRecord, error) {
	var r domain.SkillRecord
	var last *time.Time
	err := row.Scan(&r.Name, &r.Source, &r.Version, &r.State, &r.StateChangedAt, &r.CurrentSince, &r.Locked,
		&r.UseCount, &r.SuccessCount, &last, &r.LedgerEntryID, &r.LedgerSeq, &r.CreatedAt)
	if last != nil {
		r.LastUsedAt = *last
	}
	return r, err
}

// GetSkill implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) GetSkill(ctx context.Context, name string) (domain.SkillRecord, bool, error) {
	r, err := scanSkill(s.pool.QueryRow(ctx, `SELECT `+skillCols+` FROM skill_records WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillRecord{}, false, nil
	}
	if err != nil {
		return domain.SkillRecord{}, false, fmt.Errorf("get skill: %w", err)
	}
	return r, true, nil
}

// ListSkills implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) ListSkills(ctx context.Context) ([]domain.SkillRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+skillCols+` FROM skill_records ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillRecord
	for rows.Next() {
		r, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetVersion implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) GetVersion(ctx context.Context, name, version string) (domain.SkillVersion, bool, error) {
	var v domain.SkillVersion
	err := s.pool.QueryRow(ctx, `SELECT skill_name, version, skill_md, source, proposal_id, created_at
		FROM skill_versions WHERE skill_name = $1 AND version = $2`, name, version).
		Scan(&v.Name, &v.Version, &v.SkillMD, &v.Source, &v.ProposalID, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillVersion{}, false, nil
	}
	if err != nil {
		return domain.SkillVersion{}, false, fmt.Errorf("get skill version: %w", err)
	}
	return v, true, nil
}

// CountVersions implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) CountVersions(ctx context.Context, name string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM skill_versions WHERE skill_name = $1`, name).Scan(&n); err != nil {
		return 0, fmt.Errorf("count skill versions: %w", err)
	}
	return n, nil
}

// LedgerHead implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) LedgerHead(ctx context.Context, name string) (int64, error) {
	var h int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM skill_ledger WHERE skill_name = $1`, name).Scan(&h); err != nil {
		return 0, fmt.Errorf("skill ledger head: %w", err)
	}
	return h, nil
}

// Commit implements domain.SkillLifecycleStore: one transaction, serialised per
// skill by an advisory lock, with the ledger head re-checked under it.
func (s *PgSkillLifecycleStore) Commit(ctx context.Context, c domain.SkillCommit) (domain.SkillLedgerEntry, error) {
	e := c.Entry
	name := e.SkillName
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('skill:' || $1, 0))`, name); err != nil {
		return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: lock: %w", err)
	}
	var head int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM skill_ledger WHERE skill_name = $1`, name).Scan(&head); err != nil {
		return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: head: %w", err)
	}
	if head != c.ExpectSeq {
		return domain.SkillLedgerEntry{}, domain.ErrSkillLedgerConflict
	}
	e.Seq = head + 1
	e.ID = domain.SkillLedgerEntryID(name, e.Seq)

	if v := c.NewVersion; v != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO skill_versions (skill_name, version, skill_md, source, proposal_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (skill_name, version) DO NOTHING`,
			v.Name, v.Version, v.SkillMD, v.Source, v.ProposalID, v.CreatedAt); err != nil {
			return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: version: %w", err)
		}
	}
	if r := c.Record; r != nil {
		// Usage counters are owned by RecordUse/RecordRunOutcome: an upsert never
		// overwrites them with the caller's read.
		if _, err := tx.Exec(ctx, `INSERT INTO skill_records (`+skillCols+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,0,0,NULL,$8,$9,$10)
			ON CONFLICT (name) DO UPDATE SET source = EXCLUDED.source, version = EXCLUDED.version,
				state = EXCLUDED.state, state_changed_at = EXCLUDED.state_changed_at,
				current_since = EXCLUDED.current_since, locked = EXCLUDED.locked,
				ledger_entry_id = EXCLUDED.ledger_entry_id, ledger_seq = EXCLUDED.ledger_seq`,
			r.Name, r.Source, r.Version, r.State, r.StateChangedAt, r.CurrentSince, r.Locked,
			e.ID, e.Seq, r.CreatedAt); err != nil {
			return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: record: %w", err)
		}
	}
	if p := c.Proposal; p != nil {
		if _, err := tx.Exec(ctx, `UPDATE skill_proposals SET state = $2, decided_at = $3, decided_by = $4, decision_note = $5 WHERE id = $1`,
			p.ID, p.State, nullTime(p.DecidedAt), p.DecidedBy, p.DecisionNote); err != nil {
			return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: proposal: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO skill_ledger (id, skill_name, seq, mutation, from_version, to_version,
			from_state, to_state, locked, actor, reason, at, proposal_id, rollback_of, command_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		e.ID, name, e.Seq, e.Mutation, e.FromVersion, e.ToVersion, e.FromState, e.ToState, e.Locked,
		e.Actor, e.Reason, e.At, e.ProposalID, e.RollbackOf, e.CommandID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: a raced seq or a reused command_id
			return domain.SkillLedgerEntry{}, domain.ErrSkillLedgerConflict
		}
		return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.SkillLedgerEntry{}, fmt.Errorf("skill commit: %w", err)
	}
	return e, nil
}

const ledgerCols = `id, skill_name, seq, mutation, from_version, to_version, from_state, to_state, locked,
	actor, reason, at, proposal_id, rollback_of, command_id`

func scanLedger(row pgx.Row) (domain.SkillLedgerEntry, error) {
	var e domain.SkillLedgerEntry
	err := row.Scan(&e.ID, &e.SkillName, &e.Seq, &e.Mutation, &e.FromVersion, &e.ToVersion, &e.FromState, &e.ToState,
		&e.Locked, &e.Actor, &e.Reason, &e.At, &e.ProposalID, &e.RollbackOf, &e.CommandID)
	return e, err
}

// ListLedger implements domain.SkillLifecycleStore (newest first).
func (s *PgSkillLifecycleStore) ListLedger(ctx context.Context, name string, limit, offset int) ([]domain.SkillLedgerEntry, int, error) {
	where, args, order := "", []any{}, ` ORDER BY at DESC, skill_name, seq DESC`
	if name != "" {
		// One skill's history is its sequence, whatever the clocks said.
		where, args, order = " WHERE skill_name = $1", append(args, name), ` ORDER BY seq DESC`
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM skill_ledger`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count skill ledger: %w", err)
	}
	q, args := limitOffset(`SELECT `+ledgerCols+` FROM skill_ledger`+where+order, args, limit, offset)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list skill ledger: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillLedgerEntry
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan skill ledger: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func (s *PgSkillLifecycleStore) ledgerWhere(ctx context.Context, col, val string) (domain.SkillLedgerEntry, bool, error) {
	e, err := scanLedger(s.pool.QueryRow(ctx, `SELECT `+ledgerCols+` FROM skill_ledger WHERE `+col+` = $1`, val))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillLedgerEntry{}, false, nil
	}
	if err != nil {
		return domain.SkillLedgerEntry{}, false, fmt.Errorf("get skill ledger entry: %w", err)
	}
	return e, true, nil
}

// GetLedgerEntry implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) GetLedgerEntry(ctx context.Context, id string) (domain.SkillLedgerEntry, bool, error) {
	return s.ledgerWhere(ctx, "id", id)
}

// LedgerByCommand implements domain.SkillLifecycleStore.
func (s *PgSkillLifecycleStore) LedgerByCommand(ctx context.Context, commandID string) (domain.SkillLedgerEntry, bool, error) {
	if commandID == "" {
		return domain.SkillLedgerEntry{}, false, nil
	}
	return s.ledgerWhere(ctx, "command_id", commandID)
}

// RecordUse implements domain.SkillLifecycleStore. A name with no skills row (a
// skill registered over the wire) is not counted.
func (s *PgSkillLifecycleStore) RecordUse(ctx context.Context, name, runID string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE skill_records SET use_count = use_count + 1,
		last_used_at = GREATEST(COALESCE(last_used_at, $2), $2) WHERE name = $1`, name, at)
	if err != nil {
		return fmt.Errorf("record skill use: %w", err)
	}
	if tag.RowsAffected() == 0 || runID == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO skill_run_uses (run_id, skill_name, first_used) VALUES ($1,$2,$3)
		ON CONFLICT (run_id, skill_name) DO NOTHING`, runID, name, at); err != nil {
		return fmt.Errorf("record skill run use: %w", err)
	}
	return nil
}

// RecordRunOutcome implements domain.SkillLifecycleStore: settles the run's uses
// exactly once (UPDATE … WHERE NOT settled RETURNING is the once), crediting
// success_count on success.
func (s *PgSkillLifecycleStore) RecordRunOutcome(ctx context.Context, runID string, success bool) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH settled AS (
			UPDATE skill_run_uses SET settled = TRUE WHERE run_id = $1 AND NOT settled RETURNING skill_name
		), credited AS (
			UPDATE skill_records SET success_count = success_count + 1
			WHERE $2 AND name IN (SELECT skill_name FROM settled) RETURNING name
		)
		SELECT name FROM credited`, runID, success)
	if err != nil {
		return nil, fmt.Errorf("record skill run outcome: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
