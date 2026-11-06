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

// Mode is the boot-time migration policy, config key database.migrations.mode
// (T-20261104-kernel-binaries-auto-apply-migrations-to-the).
//
// It exists because applying migrations is the one boot step that changes a database
// for every OTHER binary too: on 2026-11-03 a smoke-test build booted on the owner's
// working database by mistake, applied 0020-0022 on its way up, and every older binary
// then refused that database as "ahead" (T-20261103-smoke-test-agent-accidentally-booted-the-a1).
type Mode string

const (
	// ModeAuto applies every pending migration at boot (the historical behaviour and the
	// default) — except on a database named in Policy.ProtectedDBNames.
	ModeAuto Mode = "auto"
	// ModeRefusePending boots only when nothing is pending; otherwise it fails before
	// writing anything and names the pending changes.
	ModeRefusePending Mode = "refuse_pending"
	// ModeOff skips the runner at boot entirely (the same as storage.auto_migrate=false);
	// schema is then managed by `migrate up` or external tooling.
	ModeOff Mode = "off"
)

// ParseMode validates a configured mode. Empty is ModeAuto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModeRefusePending, ModeOff:
		return m, nil
	default:
		return "", fmt.Errorf("unknown migration mode %q (want auto | refuse_pending | off)", s)
	}
}

// Policy is what the boot path may do to the schema of the database it connected to.
// The zero value is ModeAuto with nothing protected — the pre-guard behaviour.
type Policy struct {
	Mode Mode
	// ProtectedDBNames are databases on which a pending schema change is refused at
	// boot whatever Mode says (ModeOff still skips the runner). Matched case-insensitively
	// against current_database() — the database actually connected, not the config string.
	ProtectedDBNames []string
	// AllowPending is the explicit, per-boot opt-in that lets a guarded boot apply its
	// pending changes (CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true).
	AllowPending bool
}

// protects reports whether db is on the protected list.
func (p Policy) protects(db string) bool {
	for _, n := range p.ProtectedDBNames {
		if strings.EqualFold(strings.TrimSpace(n), db) {
			return true
		}
	}
	return false
}

// ErrPendingRefused is returned (wrapped) when the policy refuses a pending schema change.
var ErrPendingRefused = errors.New("migration policy refuses to change this database's schema at boot")

// Boot is the boot-time entry point: it applies Policy, then runs the runner. A refusal
// is decided by reads only — before schema_migrations is created, before any migration
// and before any concurrent index build — so a refused boot leaves the database exactly
// as it found it.
func Boot(ctx context.Context, pool *pgxpool.Pool, dim int, p Policy) error {
	mode := p.Mode
	if mode == "" {
		mode = ModeAuto
	}
	if mode == ModeOff {
		return nil
	}
	db, err := currentDatabase(ctx, pool)
	if err != nil {
		return err
	}
	protected := p.protects(db)
	if mode != ModeRefusePending && !protected {
		return migrate(ctx, pool, dim, db)
	}
	reason := "database.migrations.mode is refuse_pending"
	if protected {
		reason = "database.migrations.protected_dbnames lists it"
	}
	pending, err := pendingChanges(ctx, pool, dim)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return migrate(ctx, pool, dim, db) // nothing to apply: the runner only re-checks
	}
	if !p.AllowPending {
		return fmt.Errorf("%w: database %q has %d pending schema change(s) [%s] that this binary "+
			"would apply (%s). Nothing was written. If %q is the database you meant to upgrade, "+
			"boot once, or run `<binary> migrate up`, with "+
			"CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true (a protected database refuses "+
			"`migrate up` without it too); otherwise point database.dbname "+
			"(CAMBRIAN_DATABASE__DBNAME) at the database this binary is meant for",
			ErrPendingRefused, db, len(pending), strings.Join(pending, ", "), reason, db)
	}
	slog.Warn("migrate: applying pending schema changes to a guarded database by explicit opt-in "+
		"(database.migrations.allow_pending)", "database", db, "pending", pending, "guard", reason)
	return migrate(ctx, pool, dim, db)
}

// Migrate brings schema_migrations up to date: it creates the table if absent, refuses
// when the DB is ahead of this binary, and applies every pending migration in version
// order (each in its own transaction). dim substitutes ${EMBEDDING_DIM} in the SQL.
// It applies no Policy: it is the tests' entry point. Boot and the `migrate up` CLI
// (postgres.RunMigrations) go through Boot, so both honour protected_dbnames.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dim int) error {
	db, err := currentDatabase(ctx, pool)
	if err != nil {
		return err
	}
	return migrate(ctx, pool, dim, db)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, dim int, db string) error {
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
	if err := refuseAhead(migs, applied); err != nil {
		return err
	}

	// Apply every pending migration in version order, each in its own transaction.
	// WARN, with the database name: a schema change is visible to every other binary
	// that uses this database, so it must never be a quiet line in a boot log.
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		slog.Warn("migrate: applying schema migration", "database", db,
			"version", m.version, "name", m.name)
		if err := applyMigration(ctx, pool, m); err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
		applied[m.version] = true
	}
	return ensureConcurrentIndexes(ctx, pool, db, applied, concurrentIndexesFor(dim))
}

// refuseAhead refuses if the DB carries a version this binary does not know — an older
// binary against a DB migrated by a newer one (PLAT-02: "refuse to start when DB is ahead").
func refuseAhead(migs []migration, applied map[int64]bool) error {
	known, maxKnown := knownVersions(migs)
	if v, ahead := unknownApplied(applied, known); ahead {
		return fmt.Errorf("database is ahead of this binary: schema_migrations has version %d "+
			"which this build does not know (highest known %d) — upgrade the binary", v, maxKnown)
	}
	return nil
}

// pendingChanges lists, READ-ONLY, every schema change migrate would make: each
// unapplied migration ("0024_extraction_versions") and each missing or INVALID
// concurrent index ("index idx_…"). It creates nothing — on a database with no
// schema_migrations table every migration is pending. It refuses a database that is
// ahead of this binary, exactly as migrate would.
func pendingChanges(ctx context.Context, pool *pgxpool.Pool, dim int) ([]string, error) {
	migs, err := loadMigrations(dim)
	if err != nil {
		return nil, err
	}
	var hasTable bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&hasTable); err != nil {
		return nil, fmt.Errorf("migrate: inspect schema_migrations: %w", err)
	}
	applied := map[int64]bool{}
	if hasTable {
		if applied, err = appliedVersions(ctx, pool); err != nil {
			return nil, err
		}
	}
	if err := refuseAhead(migs, applied); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range migs {
		if !applied[m.version] {
			out = append(out, fmt.Sprintf("%04d_%s", m.version, m.name))
		}
	}
	// Indexes are checked only for migrations already applied: an index whose
	// migration is pending is covered by naming that migration.
	for _, ix := range concurrentIndexesFor(dim) {
		if !applied[ix.requires] {
			continue
		}
		valid, err := indexValid(ctx, pool, ix.name)
		if err != nil {
			return nil, err
		}
		if valid == nil || !*valid {
			out = append(out, "index "+ix.name)
		}
	}
	return out, nil
}

// currentDatabase is the database the pool is actually connected to.
func currentDatabase(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var db string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return "", fmt.Errorf("migrate: read current_database: %w", err)
	}
	return db, nil
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
	// ADR-0136 A4: the document-scoped fact lane finds the facts of a few source
	// documents by their recorded source_document value (postgres.PgFactItemReader
	// scopedSearchSQL, which repeats this literal predicate). Partial, so it holds one
	// row per fact. 0022_fact_items is the version whose memory_fact kind writes it.
	{name: "idx_statement_values_source_document", table: "statement_values",
		using: "(value_text) WHERE predicate = 'source_document'", requires: 22},
	// ADR-0138 B1.1: an operator upload looks up (writer, revision digest) before it
	// writes, so an exact re-upload is a no-op (postgres.PgDocumentLifecycle
	// FindByContent). 0026_document_lifecycle creates both columns. Partial: rows
	// written before 0026 carry no digest and can never match.
	{name: "idx_documents_writer_digest", table: "documents",
		using: "(written_by, content_digest) WHERE content_digest IS NOT NULL", requires: 26},
}

// ItemEmbeddingIndexName is the per-dimension partial HNSW index of the ADR-0136 A1 item
// projection (knowledge_item_embeddings, 0022). The fact lane's SQL names the same cast
// and the same `dims = <dim>` predicate (postgres.PgFactItemReader), because a partial
// expression index serves only a query that repeats its expression and implies its
// predicate.
func ItemEmbeddingIndexName(dim int) string {
	return fmt.Sprintf("idx_knowledge_item_embeddings_hnsw_d%d", dim)
}

// concurrentIndexesFor is concurrentIndexes plus the indexes whose shape depends on the
// deployment's embedding dimension. The item projection's column is an UNTYPED vector
// (ADR-0107 D1/D2), so its HNSW index is a partial index over a cast to the active
// dimension. It is built here — the migration's companion step — rather than by a
// backfill tool, so the fact lane is never an exact scan by accident (A1.5). A
// deployment that changes its embedder dimension gets a second index on the next boot;
// the old one keeps serving rows of the old dimension.
func concurrentIndexesFor(dim int) []concurrentIndex {
	out := append([]concurrentIndex(nil), concurrentIndexes...)
	if dim > 0 {
		out = append(out, concurrentIndex{
			name:  ItemEmbeddingIndexName(dim),
			table: "knowledge_item_embeddings",
			using: fmt.Sprintf("USING hnsw ((embedding::vector(%d)) vector_cosine_ops) WITH (m = 16, ef_construction = 64) WHERE dims = %d", dim, dim),
			// 0022_fact_items creates the table.
			requires: 22,
		})
	}
	return out
}

// ensureConcurrentIndexes builds every concurrentIndex that is missing or INVALID.
//
// Idempotent and safe to run on every boot: a valid index is a single catalog lookup.
// An interrupted CONCURRENTLY build leaves an INVALID index behind, which CREATE INDEX
// IF NOT EXISTS would then skip forever while the planner ignores it — so an invalid
// one is dropped (also concurrently) and rebuilt. Runs on the pool, never in a tx.
func ensureConcurrentIndexes(ctx context.Context, pool *pgxpool.Pool, db string, applied map[int64]bool, indexes []concurrentIndex) error {
	for _, ix := range indexes {
		if !applied[ix.requires] {
			continue
		}
		valid, err := indexValid(ctx, pool, ix.name)
		if err != nil {
			return err
		}
		if valid != nil && *valid {
			continue
		}
		if valid != nil {
			slog.Warn("migrate: rebuilding an INVALID index (an earlier concurrent build was interrupted)",
				"database", db, "index", ix.name)
			if _, err := pool.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+ix.name); err != nil {
				return fmt.Errorf("drop invalid index %s: %w", ix.name, err)
			}
		}
		started := time.Now()
		slog.Warn("migrate: building index concurrently (writes are not blocked)",
			"database", db, "index", ix.name, "table", ix.table)
		if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s %s",
			ix.name, ix.table, ix.using)); err != nil {
			return fmt.Errorf("build index %s concurrently: %w", ix.name, err)
		}
		slog.Info("migrate: index built", "database", db, "index", ix.name, "took", time.Since(started).String())
	}
	return nil
}

// indexValid returns pg_index.indisvalid for the named index in the current schema, or
// nil when no such index exists.
func indexValid(ctx context.Context, pool *pgxpool.Pool, name string) (*bool, error) {
	var valid *bool
	err := pool.QueryRow(ctx, `
		SELECT i.indisvalid
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, name).Scan(&valid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("inspect index %s: %w", name, err)
	}
	return valid, nil
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
