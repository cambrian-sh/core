package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Destructive migrations and the mandatory pre-migration snapshot
// (T-20261106-decide-the-database-backup-strategy-for, ENG-681; owner decision 2026-11-15).
//
// The incident behind it: on 2026-11-05 a mis-targeted `migrate up` applied 0028
// (DELETE of every scene row) to the owner's working database, and the only recovery
// source was an ad-hoc dump taken two days earlier
// (T-20261106-migrations-0027-0031-were-applied-to-the-live). So before the runner
// applies a destructive migration to a database that already holds data, it takes a
// snapshot (Policy.Snapshot, supplied by internal/backup through the postgres adapter)
// and refuses to proceed when the snapshot fails — unless the operator explicitly says
// otherwise (Policy.AllowDestructiveWithoutSnapshot).
//
// What counts as destructive is an EXPLICIT marker in the migration file (or, for files
// shipped before the marker existed, shippedClassification), not a guess
// from its SQL: the runtime rule must be deterministic and testable, and SQL heuristics
// both miss (a data-moving UPDATE) and over-fire (a DELETE inside a function body that
// never runs at migration time). The marker cannot be forgotten silently:
// TestMigrations_DestructiveStatementsAreClassified fails for any core migration whose
// SQL contains a data-losing statement (DROP TABLE / DROP COLUMN / DROP SCHEMA /
// TRUNCATE / DELETE FROM / ALTER … TYPE) and carries neither marker.

// DestructiveMarker, on a line of its own in a migration file, marks the migration as
// able to lose or rewrite existing data. The runner snapshots the database before it.
const DestructiveMarker = "-- cambrian:destructive"

// NotDestructiveMarker, followed by a reason, records that a migration whose SQL looks
// destructive was reviewed and is not (for example, a DELETE inside a function body).
const NotDestructiveMarker = "-- cambrian:not-destructive:"

// ErrSnapshotRequired is returned (wrapped) when a destructive migration is pending on a
// database with history and no snapshot could be taken.
var ErrSnapshotRequired = errors.New("a destructive migration needs a snapshot of the database first")

// IsDestructive reports whether a migration's SQL carries DestructiveMarker on a line of
// its own (trailing text after a space is allowed, e.g. a reason).
func IsDestructive(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		l := strings.TrimSpace(line)
		if l == DestructiveMarker || strings.HasPrefix(l, DestructiveMarker+" ") || strings.HasPrefix(l, DestructiveMarker+":") {
			return true
		}
	}
	return false
}

// shippedClassification classifies the core migrations shipped BEFORE the marker
// existed (0001–0050). A shipped migration file is never edited (card.core.db-migrations),
// so their classification lives here instead of in the files; true = destructive.
// Reviewed 2026-11-15 (ENG-681) against every file whose SQL matches the lint patterns:
//   - 0001 baseline: DROP COLUMN of the legacy importance_score on adoption and a DELETE
//     inside a function body — reviewed, not destructive for a database it can adopt.
//   - 0007 split_documents: moves every documents row into new tables — destructive.
//   - 0017 drop_removed_plugin_tables, 0021 drop_memory_facts: DROP TABLE — destructive.
//   - 0028 delete_scenes, 0045 delete_neural_traces: DELETE of whole kinds — destructive.
//   - 0047 substrate_lane_stream_keys: rewrites event/observation keys in place — destructive.
//
// Every migration from 0051 on carries DestructiveMarker or NotDestructiveMarker instead.
var shippedClassification = map[int64]bool{
	1: false, 7: true, 17: true, 21: true, 28: true, 45: true, 47: true,
}

// lastShippedUnmarked is the highest version classified by shippedClassification.
const lastShippedUnmarked int64 = 50

// coreDestructive reports whether a core migration is destructive: its marker, or its
// shipped classification.
func coreDestructive(version int64, sql string) bool {
	return IsDestructive(sql) || shippedClassification[version]
}

// guardCore enforces the snapshot rule for core's pending migrations.
func guardCore(ctx context.Context, pool *pgxpool.Pool, dim int, db string, p Policy) error {
	destructive, err := coreDestructivePending(ctx, pool, dim)
	if err != nil {
		return err
	}
	return guardDestructive(ctx, db, p, destructive)
}

// guardDestructive takes the snapshot (or refuses) when destructive is non-empty. It
// runs after the policy's read-only checks and before the first write.
func guardDestructive(ctx context.Context, db string, p Policy, destructive []string) error {
	if len(destructive) == 0 {
		return nil
	}
	list := strings.Join(destructive, ", ")
	if p.AllowDestructiveWithoutSnapshot {
		slog.Warn("migrate: applying DESTRUCTIVE migration(s) WITHOUT a snapshot by explicit operator override "+
			"(database.migrations.allow_destructive_without_snapshot / --skip-snapshot)",
			"database", db, "destructive", destructive)
		return nil
	}
	if p.Snapshot == nil {
		return fmt.Errorf("%w: database %q has pending destructive migration(s) [%s] and this path cannot take a "+
			"snapshot. Nothing was written. Take one with `<binary> backup run --kind manual`, then re-run with "+
			"--skip-snapshot (or CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_DESTRUCTIVE_WITHOUT_SNAPSHOT=true)",
			ErrSnapshotRequired, db, list)
	}
	slog.Warn("migrate: destructive migration(s) pending — taking a snapshot first", "database", db, "destructive", destructive)
	where, err := p.Snapshot(ctx, db, destructive)
	if err != nil {
		return fmt.Errorf("%w: database %q has pending destructive migration(s) [%s] and the snapshot failed: %v. "+
			"Nothing was written. Fix the backup configuration (the `backup` config block: dir, pg_container; "+
			"pg_dump must be reachable), or, only if you hold another restorable copy, re-run with --skip-snapshot "+
			"(CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_DESTRUCTIVE_WITHOUT_SNAPSHOT=true)",
			ErrSnapshotRequired, db, list, err)
	}
	slog.Warn("migrate: snapshot taken before destructive migration(s)", "database", db, "snapshot", where)
	return nil
}

// coreDestructivePending lists, READ-ONLY, the pending core migrations marked
// destructive — empty when the database has no applied history (a fresh database has
// nothing a migration could lose).
func coreDestructivePending(ctx context.Context, pool *pgxpool.Pool, dim int) ([]string, error) {
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&hasTable); err != nil {
		return nil, fmt.Errorf("migrate: inspect schema_migrations: %w", err)
	}
	if !hasTable {
		return nil, nil
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}
	if len(applied) == 0 {
		return nil, nil
	}
	migs, err := loadMigrations(dim)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range migs {
		if m.destructive && !applied[m.version] {
			out = append(out, fmt.Sprintf("%04d_%s", m.version, m.name))
		}
	}
	return out, nil
}

// laneDestructivePending is coreDestructivePending for plugin lanes; a lane with no
// applied version is fresh and contributes nothing.
func laneDestructivePending(ctx context.Context, pool *pgxpool.Pool, lanes []Lane) ([]string, error) {
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('plugin_schema_migrations') IS NOT NULL`).Scan(&hasTable); err != nil {
		return nil, fmt.Errorf("migrate: inspect plugin_schema_migrations: %w", err)
	}
	if !hasTable {
		return nil, nil
	}
	var out []string
	for _, l := range lanes {
		migs, err := loadLane(l)
		if err != nil {
			return nil, err
		}
		applied, err := laneApplied(ctx, pool, l.Namespace)
		if err != nil {
			return nil, err
		}
		if len(applied) == 0 {
			continue
		}
		for _, m := range migs {
			if m.destructive && !applied[m.version] {
				out = append(out, fmt.Sprintf("%s/%04d_%s", l.Namespace, m.version, m.name))
			}
		}
	}
	return out, nil
}
