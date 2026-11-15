package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Plugin migration lanes (T-20261113-premium-has-no-schema-migration-chain-41-create,
// owner decision 2026-11-14).
//
// A plugin that owns tables (premium's authz, ingress studio, pipeline, receipts, …)
// registers its embedded NNNN_name.sql files as a Lane under its own NAMESPACE. The
// same runner applies them — forward-only, versioned, one transaction per file — and
// records them in plugin_schema_migrations keyed by (namespace, version), so a plugin's
// version numbers never collide with core's schema_migrations or with another plugin's.
//
// The mechanics are core's on purpose: before this, premium created ~65 tables and
// columns with IF NOT EXISTS DDL inside its store constructors, with no ledger,
// ordering, status or refusal, so `migrate status` could not report premium state and a
// non-additive change had no home. Core never imports premium: a lane is only a
// namespace and an fs.FS (ADR-0057).
//
// The boot Policy applies to lanes exactly as to core: ModeOff skips them, and a
// refuse_pending or protected database names the pending plugin versions and writes
// nothing (BootLanes decides by reads only before any write).

// Lane is one plugin's migration set.
type Lane struct {
	// Namespace keys the lane's ledger rows ("premium"). Lowercase, [a-z0-9_-],
	// starting with a letter; "core" is reserved.
	Namespace string
	// FS holds NNNN_name.sql files at its root. No ${EMBEDDING_DIM} substitution: a
	// plugin that needs the embedding dimension is touching core's tables.
	FS fs.FS
}

// LaneRecord is one row of a lane's status.
type LaneRecord struct {
	Namespace string
	Record
}

var laneNamespace = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ValidateLane checks a lane before it is registered: a usable namespace, at least
// one migration, well-formed unique versions. It reads the files once, so a broken
// lane fails the plugin's registration rather than the first boot against a database.
func ValidateLane(l Lane) error {
	if !laneNamespace.MatchString(l.Namespace) || l.Namespace == "core" {
		return fmt.Errorf("migration lane namespace %q: want [a-z][a-z0-9_-]{0,62}, not \"core\"", l.Namespace)
	}
	migs, err := loadLane(l)
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		return fmt.Errorf("migration lane %q: no NNNN_name.sql files at the root of its FS", l.Namespace)
	}
	return nil
}

// LaneDigest is a content digest of a lane's migration files (name + body), used to
// tell a repeated registration of the same lane from a conflicting one.
func LaneDigest(l Lane) (string, error) {
	migs, err := loadLane(l)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, m := range migs {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00", m.version, m.name, m.sql)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadLane parses a lane's files (versioned, sorted, no duplicates).
func loadLane(l Lane) ([]migration, error) {
	if l.FS == nil {
		return nil, fmt.Errorf("migration lane %q: nil FS", l.Namespace)
	}
	entries, err := fs.Glob(l.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, e := range entries {
		version, name, err := parseName(path.Base(e))
		if err != nil {
			return nil, fmt.Errorf("migration lane %q: %w", l.Namespace, err)
		}
		if version < 1 {
			return nil, fmt.Errorf("migration lane %q: %s: version must be >= 1", l.Namespace, e)
		}
		raw, err := fs.ReadFile(l.FS, e)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: version, name: name, sql: string(raw), destructive: IsDestructive(string(raw))})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i := 1; i < len(migs); i++ {
		if migs[i].version == migs[i-1].version {
			return nil, fmt.Errorf("migration lane %q: duplicate version %d", l.Namespace, migs[i].version)
		}
	}
	return migs, nil
}

// BootLanes applies the plugin lanes under the same Policy as Boot. Lanes run after
// core (a plugin's tables may reference core's) and in the order given.
func BootLanes(ctx context.Context, pool *pgxpool.Pool, p Policy, lanes []Lane) error {
	if len(lanes) == 0 {
		return nil
	}
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
	if mode == ModeRefusePending || protected {
		pending, err := pendingLaneChanges(ctx, pool, lanes)
		if err != nil {
			return err
		}
		if len(pending) > 0 && !p.AllowPending {
			reason := "database.migrations.mode is refuse_pending"
			if protected {
				reason = "database.migrations.protected_dbnames lists it"
			}
			return fmt.Errorf("%w: database %q has %d pending plugin schema change(s) [%s] that this "+
				"binary would apply (%s). Nothing was written. Run `<binary> migrate up` with "+
				"CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true if this is the database you meant to upgrade",
				ErrPendingRefused, db, len(pending), strings.Join(pending, ", "), reason)
		}
	}
	destructive, err := laneDestructivePending(ctx, pool, lanes)
	if err != nil {
		return err
	}
	if err := guardDestructive(ctx, db, p, destructive); err != nil {
		return err
	}
	return applyLanes(ctx, pool, db, lanes)
}

// MigrateLanes applies every pending lane migration with no Policy (tests).
func MigrateLanes(ctx context.Context, pool *pgxpool.Pool, lanes []Lane) error {
	db, err := currentDatabase(ctx, pool)
	if err != nil {
		return err
	}
	return applyLanes(ctx, pool, db, lanes)
}

func applyLanes(ctx context.Context, pool *pgxpool.Pool, db string, lanes []Lane) error {
	if err := ensureLaneTable(ctx, pool); err != nil {
		return err
	}
	for _, l := range lanes {
		migs, err := loadLane(l)
		if err != nil {
			return err
		}
		applied, err := laneApplied(ctx, pool, l.Namespace)
		if err != nil {
			return err
		}
		if err := refuseLaneAhead(l.Namespace, migs, applied); err != nil {
			return err
		}
		for _, m := range migs {
			if applied[m.version] {
				continue
			}
			slog.Warn("migrate: applying plugin schema migration", "database", db,
				"namespace", l.Namespace, "version", m.version, "name", m.name)
			if err := applyLaneMigration(ctx, pool, l.Namespace, m); err != nil {
				return fmt.Errorf("apply %s migration %04d_%s: %w", l.Namespace, m.version, m.name, err)
			}
		}
	}
	return nil
}

// pendingLaneChanges lists, READ-ONLY, every lane migration not yet applied
// ("premium/0001_baseline"). It creates nothing.
func pendingLaneChanges(ctx context.Context, pool *pgxpool.Pool, lanes []Lane) ([]string, error) {
	var hasTable bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('plugin_schema_migrations') IS NOT NULL`).Scan(&hasTable); err != nil {
		return nil, fmt.Errorf("migrate: inspect plugin_schema_migrations: %w", err)
	}
	var out []string
	for _, l := range lanes {
		migs, err := loadLane(l)
		if err != nil {
			return nil, err
		}
		applied := map[int64]bool{}
		if hasTable {
			if applied, err = laneApplied(ctx, pool, l.Namespace); err != nil {
				return nil, err
			}
		}
		if err := refuseLaneAhead(l.Namespace, migs, applied); err != nil {
			return nil, err
		}
		for _, m := range migs {
			if !applied[m.version] {
				out = append(out, fmt.Sprintf("%s/%04d_%s", l.Namespace, m.version, m.name))
			}
		}
	}
	return out, nil
}

func refuseLaneAhead(ns string, migs []migration, applied map[int64]bool) error {
	known, maxKnown := knownVersions(migs)
	if v, ahead := unknownApplied(applied, known); ahead {
		return fmt.Errorf("database is ahead of this binary: plugin_schema_migrations has %s version %d "+
			"which this build does not know (highest known %d) — upgrade the binary", ns, v, maxKnown)
	}
	return nil
}

// StatusLanes returns every lane migration (applied and pending), lane by lane in
// version order.
func StatusLanes(ctx context.Context, pool *pgxpool.Pool, lanes []Lane) ([]LaneRecord, error) {
	if len(lanes) == 0 {
		return nil, nil
	}
	if err := ensureLaneTable(ctx, pool); err != nil {
		return nil, err
	}
	var out []LaneRecord
	for _, l := range lanes {
		migs, err := loadLane(l)
		if err != nil {
			return nil, err
		}
		rows, err := pool.Query(ctx,
			`SELECT version, name, applied_at FROM plugin_schema_migrations WHERE namespace = $1`, l.Namespace)
		if err != nil {
			return nil, err
		}
		at := map[int64]Record{}
		for rows.Next() {
			var r Record
			if err := rows.Scan(&r.Version, &r.Name, &r.AppliedAt); err != nil {
				rows.Close()
				return nil, err
			}
			at[r.Version] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, m := range migs {
			if r, ok := at[m.version]; ok {
				out = append(out, LaneRecord{Namespace: l.Namespace, Record: r})
			} else {
				out = append(out, LaneRecord{Namespace: l.Namespace,
					Record: Record{Version: m.version, Name: m.name, Pending: true}})
			}
		}
	}
	return out, nil
}

func ensureLaneTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS plugin_schema_migrations (
		namespace  TEXT NOT NULL,
		version    BIGINT NOT NULL,
		name       TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (namespace, version)
	)`)
	return err
}

func laneApplied(ctx context.Context, pool *pgxpool.Pool, ns string) (map[int64]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM plugin_schema_migrations WHERE namespace = $1`, ns)
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

func applyLaneMigration(ctx context.Context, pool *pgxpool.Pool, ns string, m migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO plugin_schema_migrations (namespace, version, name) VALUES ($1, $2, $3)`,
		ns, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
