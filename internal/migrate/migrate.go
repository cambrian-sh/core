// Package migrate is a minimal, pure-Go (no CGO, no external migration library)
// forward-only schema migration runner over a pgx pool. PLAT-02 / ADR-0064.
//
// Every schema change — including the head baseline (0001_baseline.sql) — is an
// embedded, versioned SQL file (migrations/NNNN_name.sql) applied in order, each in its
// own transaction, and recorded in schema_migrations. The baseline is fully idempotent
// (CREATE … IF NOT EXISTS / OR REPLACE), so applying it to a pre-existing database is a
// safe no-op that simply adopts the version table. ${EMBEDDING_DIM} in a file is
// substituted from config (pgvector cannot ALTER a VECTOR column's dimension). The
// runner refuses when the database carries a version this binary does not know.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed all:migrations
var migrationFS embed.FS

// BaselineVersion is the version of the head-schema baseline migration.
const BaselineVersion int64 = 1

// Record is one row of schema_migrations (Pending=true for a known, not-yet-applied one).
type Record struct {
	Version   int64
	Name      string
	AppliedAt time.Time
	Pending   bool
}

// migration is one versioned schema change loaded from an embedded SQL file.
type migration struct {
	version int64
	name    string
	sql     string
}

// Migrate brings schema_migrations up to date: it creates the table if absent, refuses
// when the DB is ahead of this binary, and applies every pending migration in version
// order (each in its own transaction). dim substitutes ${EMBEDDING_DIM} in the SQL.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dim int) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}
	migs, err := loadMigrations(dim)
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	// Refuse if the DB carries a version this binary does not know — an older binary
	// against a DB migrated by a newer one (PLAT-02: "refuse to start when DB is ahead").
	known, maxKnown := knownVersions(migs)
	if v, ahead := unknownApplied(applied, known); ahead {
		return fmt.Errorf("database is ahead of this binary: schema_migrations has version %d "+
			"which this build does not know (highest known %d) — upgrade the binary", v, maxKnown)
	}

	// Apply every pending migration in version order, each in its own transaction.
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, pool, m); err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
		applied[m.version] = true
	}
	return ensureConcurrentIndexes(ctx, pool, applied)
}

// Event-time expressions (ADR-0134). The span of a row is
// [EventStartExpr, EventEndExpr]; a NULL event_time (every row written before 0019,
// and every writer that states no time) reads as the ingest clock, created_at, which is
// a UTC wall clock in a TIMESTAMP WITHOUT TIME ZONE column (see 0019_event_time.sql).
//
// They live HERE, beside the indexes built on them, and the pgvector adapter builds its
// window predicate from these same constants: an expression index serves a query only
// when the query's expression is the indexed one, so two hand-typed copies that drifted
// by one parenthesis would turn every windowed read into a sequential scan with no error.
// The adapter qualifies the column names for its JOIN; qualification does not affect
// the match.
const (
	EventStartExpr = "COALESCE(event_time, (created_at AT TIME ZONE 'UTC'))"
	EventEndExpr   = "COALESCE(event_time_end, event_time, (created_at AT TIME ZONE 'UTC'))"
)

// concurrentIndex is an index that must be built OUTSIDE a transaction
// (CREATE INDEX CONCURRENTLY), so it cannot live in a migration file: the runner applies
// each file inside one transaction (applyMigration), and a plain CREATE INDEX on the
// largest table would hold a write-blocking lock for the whole build.
type concurrentIndex struct {
	name  string
	table string
	// using is everything after "ON <table>" — the method and the key expression.
	using string
	// requires is the migration version that creates the indexed columns; the build is
	// skipped until it is applied.
	requires int64
}

// concurrentIndexes is the out-of-band index list, built after every migration file.
//
// ADR-0134 P2: the event-time window is a span-overlap test (event_after inclusive,
// event_before exclusive) — `EventEndExpr >= after AND EventStartExpr < before` — so one
// bound reads each end of the span, and each gets its own b-tree. The metadata JSONB
// GIN index cannot serve a range predicate, which is why the times are typed columns.
var concurrentIndexes = []concurrentIndex{
	{name: "idx_chunks_event_start", table: "chunks", using: "(" + EventStartExpr + ")", requires: 19},
	{name: "idx_chunks_event_end", table: "chunks", using: "(" + EventEndExpr + ")", requires: 19},
}

// ensureConcurrentIndexes builds every concurrentIndex that is missing or INVALID.
//
// Idempotent and safe to run on every boot: a valid index is a single catalog lookup.
// An interrupted CONCURRENTLY build leaves an INVALID index behind, which CREATE INDEX
// IF NOT EXISTS would then skip forever while the planner ignores it — so an invalid
// one is dropped (also concurrently) and rebuilt. Runs on the pool, never in a tx.
func ensureConcurrentIndexes(ctx context.Context, pool *pgxpool.Pool, applied map[int64]bool) error {
	for _, ix := range concurrentIndexes {
		if !applied[ix.requires] {
			continue
		}
		var valid *bool
		err := pool.QueryRow(ctx, `
			SELECT i.indisvalid
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, ix.name).Scan(&valid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("inspect index %s: %w", ix.name, err)
		}
		if valid != nil && *valid {
			continue
		}
		if valid != nil {
			slog.Warn("migrate: rebuilding an INVALID index (an earlier concurrent build was interrupted)",
				"index", ix.name)
			if _, err := pool.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+ix.name); err != nil {
				return fmt.Errorf("drop invalid index %s: %w", ix.name, err)
			}
		}
		started := time.Now()
		slog.Info("migrate: building index concurrently (writes are not blocked)",
			"index", ix.name, "table", ix.table)
		if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s %s",
			ix.name, ix.table, ix.using)); err != nil {
			return fmt.Errorf("build index %s concurrently: %w", ix.name, err)
		}
		slog.Info("migrate: index built", "index", ix.name, "took", time.Since(started).String())
	}
	return nil
}

// Status returns every known migration (applied and pending) in version order.
func Status(ctx context.Context, pool *pgxpool.Pool) ([]Record, error) {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return nil, err
	}
	migs, err := loadMigrations(0) // dim irrelevant for listing
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT version, name, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	appliedAt := map[int64]Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Version, &r.Name, &r.AppliedAt); err != nil {
			return nil, err
		}
		appliedAt[r.Version] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Record, 0, len(migs))
	for _, m := range migs {
		if r, ok := appliedAt[m.version]; ok {
			out = append(out, r)
		} else {
			out = append(out, Record{Version: m.version, Name: m.name, Pending: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	return err
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int64]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	// A migration file with no args runs via the simple protocol, so multiple
	// semicolon-separated statements (incl. functions) execute as one batch.
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// loadMigrations parses the embedded SQL files (versioned, sorted, ${EMBEDDING_DIM}
// substituted).
func loadMigrations(dim int) ([]migration, error) {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, e := range entries {
		version, name, err := parseName(path.Base(e))
		if err != nil {
			return nil, err
		}
		if version < 1 {
			return nil, fmt.Errorf("migration %s: version must be >= 1", path.Base(e))
		}
		raw, err := migrationFS.ReadFile(e)
		if err != nil {
			return nil, err
		}
		sql := strings.ReplaceAll(string(raw), "${EMBEDDING_DIM}", strconv.Itoa(dim))
		migs = append(migs, migration{version: version, name: name, sql: sql})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i := 1; i < len(migs); i++ {
		if migs[i].version == migs[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", migs[i].version)
		}
	}
	return migs, nil
}

// knownVersions returns the set of versions this binary knows and the highest one.
func knownVersions(migs []migration) (map[int64]bool, int64) {
	known := map[int64]bool{}
	var maxKnown int64
	for _, m := range migs {
		known[m.version] = true
		if m.version > maxKnown {
			maxKnown = m.version
		}
	}
	return known, maxKnown
}

// unknownApplied returns an applied version the binary does not know (DB ahead), if any.
func unknownApplied(applied, known map[int64]bool) (int64, bool) {
	for v := range applied {
		if !known[v] {
			return v, true
		}
	}
	return 0, false
}

// parseName parses "NNNN_some_name.sql" into (version, name).
func parseName(base string) (int64, string, error) {
	trimmed := strings.TrimSuffix(base, ".sql")
	idx := strings.IndexByte(trimmed, '_')
	if idx <= 0 {
		return 0, "", fmt.Errorf("migration filename %q must be NNNN_name.sql", base)
	}
	version, err := strconv.ParseInt(trimmed[:idx], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration filename %q: bad version: %w", base, err)
	}
	return version, trimmed[idx+1:], nil
}
