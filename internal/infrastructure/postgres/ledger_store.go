package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// LedgerBodyStore is the artifact vault's byte store as the ledger uses it
// (satisfied by *vault.ArtifactVault): content-addressed, so a body shared with an
// artifact or another entry is stored once.
type LedgerBodyStore interface {
	Store(content []byte) (string, error)
	Load(hash string) ([]byte, error)
	Delete(ctx context.Context, hash string) error
}

// PgLedgerStore is the RAW run-ledger store (ADR-0145 D1): metadata in the
// 0034_ledger_entries table beside the artifact vault, bodies in the vault's byte
// store. It applies NO read enforcement — kernel.NewLedgerStack is the only place
// it is constructed, and it hands readers authz.EnforcingLedger over it, never
// this type (AGENTS.md §2.1/§2.2).
//
// Since ADR-0157 D5 it is also the ONE artifact registry: UploadArtifact, the
// scratch out/ collection and tool-written files are kind=artifact entries, so
// the vault holds no bytes that no ledger row names and the retention purge may
// delete a body as soon as its last row goes.
type PgLedgerStore struct {
	pool   *pgxpool.Pool
	bodies LedgerBodyStore
}

var _ domain.LedgerStore = (*PgLedgerStore)(nil)

// NewPgLedgerStore wraps the shared pool and the vault byte store.
func NewPgLedgerStore(pool *pgxpool.Pool, bodies LedgerBodyStore) *PgLedgerStore {
	return &PgLedgerStore{pool: pool, bodies: bodies}
}

// ledgerUnreadableSourceTag stands in for a source classification that could not
// be decoded (scanLedgerEntry); no reader predicate is expected to hold it.
const ledgerUnreadableSourceTag = "ledger:unreadable-source-classification"

// Ready reports whether the 0034 table, the 0035 source_tags column and the 0053
// artifact provenance columns exist.
func (s *PgLedgerStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_name = 'ledger_entries' AND column_name IN ('source_tags', 'source_refs')`).Scan(&n)
	return err == nil && n == 2
}

// ledgerSearchHead bounds the body text kept for ledger_find's text filter.
const ledgerSearchHead = 16 * 1024

// Put stores e's body in the vault and its metadata row. An existing id is left
// unchanged (the id is derived from the content, so it is the same entry).
func (s *PgLedgerStore) Put(ctx context.Context, e domain.LedgerEntry) error {
	if e.ID == "" || !domain.ValidLedgerKind(e.Kind) {
		return fmt.Errorf("ledger put: invalid entry (id %q, kind %q)", e.ID, e.Kind)
	}
	hash := e.BodyHash
	if hash == "" && len(e.Body) > 0 {
		h, err := s.bodies.Store(e.Body)
		if err != nil {
			return fmt.Errorf("ledger put: vault: %w", err)
		}
		hash = h
	}
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, _ := json.Marshal(tags)
	sources := e.SourceTags
	if sources == nil {
		sources = [][]string{}
	}
	sourcesJSON, _ := json.Marshal(sources)
	derivedJSON, _ := json.Marshal(nonNil(e.DerivedFrom))
	refsJSON, _ := json.Marshal(nonNil(e.SourceRefs))
	var args []byte
	if len(e.Args) > 0 && json.Valid(e.Args) {
		args = e.Args
	}
	search := e.ToolName + " " + string(args) + " " + e.Preview
	if len(e.Body) > 0 && isText(e.Body) {
		head := e.Body
		if len(head) > ledgerSearchHead {
			head = head[:ledgerSearchHead]
		}
		search += " " + string(head)
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO ledger_entries (id, kind, conversation_id, turn_id, run_id, step_index, tool_name, args,
    writer, owner, tags, content_type, body_hash, size_bytes, truncated, preview, content_hash,
    search_text, created_at, expires_at, source_tags, session_id, instance_id, derived_from, source_refs)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
ON CONFLICT (id) DO NOTHING`,
		e.ID, string(e.Kind), e.ConversationID, e.TurnID, e.RunID, e.StepIndex, e.ToolName, args,
		e.Writer, e.Owner, tagsJSON, e.ContentType, hash, e.SizeBytes, e.Truncated, e.Preview, e.ContentHash,
		sanitizeText(search), e.CreatedAt.UTC(), e.ExpiresAt.UTC(), sourcesJSON,
		e.SessionID, e.InstanceID, derivedJSON, refsJSON)
	if err != nil {
		return fmt.Errorf("ledger put: %w", err)
	}
	return nil
}

const runLedgerCols = `id, kind, conversation_id, turn_id, run_id, step_index, tool_name, args, writer, owner,
    tags, content_type, body_hash, size_bytes, truncated, preview, content_hash, created_at, expires_at, source_tags,
    session_id, instance_id, derived_from, source_refs`

func scanLedgerEntry(row pgx.Row) (domain.LedgerEntry, error) {
	var e domain.LedgerEntry
	var kind string
	var args, tags, sources, derived, refs []byte
	if err := row.Scan(&e.ID, &kind, &e.ConversationID, &e.TurnID, &e.RunID, &e.StepIndex, &e.ToolName, &args,
		&e.Writer, &e.Owner, &tags, &e.ContentType, &e.BodyHash, &e.SizeBytes, &e.Truncated, &e.Preview,
		&e.ContentHash, &e.CreatedAt, &e.ExpiresAt, &sources,
		&e.SessionID, &e.InstanceID, &derived, &refs); err != nil {
		return e, err
	}
	_ = json.Unmarshal(derived, &e.DerivedFrom) // provenance only; never a read gate
	_ = json.Unmarshal(refs, &e.SourceRefs)     // erase reach; never a read gate
	e.Kind = domain.LedgerKind(kind)
	if len(args) > 0 {
		e.Args = json.RawMessage(args)
	}
	_ = json.Unmarshal(tags, &e.Tags) // a JSONB array the writer encoded; empty on a malformed row
	if err := json.Unmarshal(sources, &e.SourceTags); err != nil && len(sources) > 0 {
		// A derived entry whose inherited classification cannot be read must not
		// read as "derived from nothing": that is the fail-open answer. An
		// unsatisfiable source set makes it unreadable to every predicate but a
		// bypass (the kernel's own reads), which is the fail-closed one.
		e.SourceTags = [][]string{{ledgerUnreadableSourceTag}}
	}
	return e, nil
}

// Get reads one entry with its body; nil when absent or expired.
func (s *PgLedgerStore) Get(ctx context.Context, id string) (*domain.LedgerEntry, error) {
	e, err := scanLedgerEntry(s.pool.QueryRow(ctx,
		`SELECT `+runLedgerCols+` FROM ledger_entries WHERE id = $1 AND expires_at > now()`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger get: %w", err)
	}
	if e.BodyHash != "" {
		body, lerr := s.bodies.Load(e.BodyHash)
		if lerr != nil {
			return nil, fmt.Errorf("ledger get: vault load: %w", lerr)
		}
		e.Body = body
	}
	return &e, nil
}

// Find lists entries (metadata only, no body) in creation order. It requires a
// conversation, turn or run, so a query never spans the whole ledger.
func (s *PgLedgerStore) Find(ctx context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	if !q.Scoped() {
		return nil, fmt.Errorf("ledger find: a conversation, turn or run is required")
	}
	where := []string{"expires_at > now()"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.SessionID != "" {
		add("session_id = $%d", q.SessionID)
	}
	if q.ConversationID != "" {
		add("conversation_id = $%d", q.ConversationID)
	}
	if q.TurnID != "" {
		add("turn_id = $%d", q.TurnID)
	}
	if q.RunID != "" {
		add("run_id = $%d", q.RunID)
	}
	if q.HasStep {
		add("step_index = $%d", q.StepIndex)
	}
	if q.ToolName != "" {
		add("tool_name = $%d", q.ToolName)
	}
	if q.Kind != "" {
		add("kind = $%d", string(q.Kind))
	}
	if q.Text != "" {
		add("strpos(lower(search_text), lower($%d)) > 0", q.Text)
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	sql := `SELECT ` + runLedgerCols + ` FROM ledger_entries WHERE `
	for i, w := range where {
		if i > 0 {
			sql += " AND "
		}
		sql += w
	}
	sql += fmt.Sprintf(" ORDER BY created_at, id LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger find: %w", err)
	}
	defer rows.Close()
	var out []domain.LedgerEntry
	for rows.Next() {
		e, serr := scanLedgerEntry(rows)
		if serr != nil {
			return nil, fmt.Errorf("ledger find: scan: %w", serr)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteScratch removes a turn's scratch entries (ADR-0145 D1: scratch lives for
// the turn only).
func (s *PgLedgerStore) DeleteScratch(ctx context.Context, turnID string) (int, error) {
	if turnID == "" {
		return 0, nil
	}
	return s.deleteReturning(ctx, `DELETE FROM ledger_entries WHERE turn_id = $1 AND kind = 'scratch' RETURNING body_hash`, turnID)
}

// PurgeExpired removes entries whose retention ended (execution.ledger.retention_hours).
func (s *PgLedgerStore) PurgeExpired(ctx context.Context, now time.Time) (int, error) {
	return s.deleteReturning(ctx, `DELETE FROM ledger_entries WHERE expires_at <= $1 RETURNING body_hash`, now.UTC())
}

// deleteReturning runs a DELETE ... RETURNING body_hash and then drops the vault
// bytes no remaining entry names (the ledger is the vault's only registry since
// ADR-0157 D5).
func (s *PgLedgerStore) deleteReturning(ctx context.Context, sql string, arg any) (int, error) {
	rows, err := s.pool.Query(ctx, sql, arg)
	if err != nil {
		return 0, fmt.Errorf("ledger delete: %w", err)
	}
	var hashes []string
	n := 0
	for rows.Next() {
		var h string
		if serr := rows.Scan(&h); serr == nil && h != "" {
			hashes = append(hashes, h)
		}
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("ledger delete: %w", err)
	}
	if _, failed, rerr := s.ReleaseBodies(ctx, hashes); rerr != nil {
		// Best effort: an undeleted body is unreachable (no entry names it) and the
		// next purge or erasure naming the same bytes retries it.
		slog.Warn("ledger: vault bodies not deleted", "failed", failed, "err", rerr)
	}
	return n, nil
}

// ReleaseBodies deletes from the vault each body in hashes that no ledger entry
// names any more, and keeps the rest (content-addressed: another entry, e.g. a
// second owner's upload of the same bytes, may share it). It is the ledger's one
// body-release path: the retention purge, scratch cleanup and the retirement of
// the pre-ADR-0157 bbolt artifact bucket (kernel.retireLegacyArtifacts) all use
// it, with the same refcount the ADR-0137 erasure worker applies
// (VaultBodyStillReferenced). Returns how many bodies were deleted and how many
// could not be checked or deleted; err joins those failures. Deleting a body
// that is already gone is not an error, so a retry is safe.
func (s *PgLedgerStore) ReleaseBodies(ctx context.Context, hashes []string) (deleted, failed int, err error) {
	seen := make(map[string]bool, len(hashes))
	var errs []error
	for _, h := range hashes {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		var still bool
		if qerr := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE body_hash = $1)`, h).Scan(&still); qerr != nil {
			failed++
			errs = append(errs, fmt.Errorf("ledger body refcount: %w", qerr))
			continue
		}
		if still {
			continue
		}
		if derr := s.bodies.Delete(ctx, h); derr != nil {
			failed++
			errs = append(errs, derr)
			continue
		}
		deleted++
	}
	return deleted, failed, errors.Join(errs...)
}

func isText(b []byte) bool { return utf8.Valid(b) }

// sanitizeText makes s storable in a TEXT column: Postgres refuses NUL bytes and
// invalid UTF-8 (a head cut mid-rune), so both are dropped.
func sanitizeText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.ReplaceAll(s, string(rune(0)), "")
}
