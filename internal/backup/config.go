package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// Paths are the resolved locations of one deployment's backup.
type Paths struct {
	Root    string // backup.dir, absolute
	KeyRoot string // backup.key_dir, absolute
}

// ResolvePaths resolves the backup block against the kernel's base directory: a relative
// dir is under baseDir, and an empty key_dir is the sibling "<dir>-keys".
func ResolvePaths(b config.BackupConfig, baseDir string) (Paths, error) {
	abs := func(p string) (string, error) {
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		return filepath.Abs(p)
	}
	root, err := abs(b.Dir)
	if err != nil {
		return Paths{}, fmt.Errorf("backup.dir: %w", err)
	}
	key := strings.TrimSpace(b.KeyDir)
	if key == "" {
		key = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-keys")
	}
	keyRoot, err := abs(key)
	if err != nil {
		return Paths{}, fmt.Errorf("backup.key_dir: %w", err)
	}
	if within(keyRoot, root) {
		return Paths{}, fmt.Errorf("backup.key_dir %s is inside backup.dir %s; the encrypted secret.key must be kept apart from the dumps", keyRoot, root)
	}
	return Paths{Root: root, KeyRoot: keyRoot}, nil
}

// PGFromConfig is the kernel database connection plus backup.pg_container.
func PGFromConfig(cfg *config.Config) PG {
	return PG{Host: cfg.Database.Host, Port: cfg.Database.Port, User: cfg.Database.User,
		Password: cfg.Database.Password, DBName: cfg.Database.DBName, Container: cfg.Backup.PGContainer}
}

// DrillDBName is backup.drill_dbname, defaulting to "<database.dbname>_restore_drill".
func DrillDBName(cfg *config.Config) string {
	if n := strings.TrimSpace(cfg.Backup.DrillDBName); n != "" {
		return n
	}
	return cfg.Database.DBName + "_restore_drill"
}

// RetentionFromConfig is backup.retention.
func RetentionFromConfig(b config.BackupConfig) Retention {
	return Retention{Daily: b.Retention.Daily, Weekly: b.Retention.Weekly, Monthly: b.Retention.Monthly}
}

// PreMigrationSnapshotter returns the internal/migrate Policy.Snapshot for this
// deployment: a database-only run of kind pre-migration under backup.dir, named for the
// destructive migrations it precedes. It refuses when the connected database is not the
// configured one (the snapshot must be of the database about to change).
func PreMigrationSnapshotter(cfg *config.Config) func(ctx context.Context, db string, destructive []string) (string, error) {
	return func(ctx context.Context, db string, destructive []string) (string, error) {
		if !strings.EqualFold(db, cfg.Database.DBName) {
			return "", fmt.Errorf("connected database %q is not database.dbname %q", db, cfg.Database.DBName)
		}
		paths, err := ResolvePaths(cfg.Backup, config.ResolveBaseDir())
		if err != nil {
			return "", err
		}
		dir, _, err := Run(ctx, Options{
			Root: paths.Root, KeyRoot: paths.KeyRoot, Kind: KindPreMigration,
			Reason: "before destructive migration(s): " + strings.Join(destructive, ", "),
			PG:     PGFromConfig(cfg),
		})
		if err != nil {
			if dir != "" {
				_ = os.RemoveAll(dir) // a failed snapshot leaves no half folder behind
			}
			return "", err
		}
		return dir, nil
	}
}

// SnapshotBeforeRestore takes the pre-restore snapshot of the live database.
func SnapshotBeforeRestore(ctx context.Context, cfg *config.Config, paths Paths, reason string, now func() time.Time) (string, error) {
	dir, _, err := Run(ctx, Options{Root: paths.Root, KeyRoot: paths.KeyRoot, Kind: KindPreRestore,
		Reason: reason, PG: PGFromConfig(cfg), Now: now})
	if err != nil && dir != "" {
		_ = os.RemoveAll(dir)
	}
	return dir, err
}
