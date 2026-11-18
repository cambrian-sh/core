// Package migratetest lets a plugin's Postgres tests put a scratch database at
// the plugin's schema version the way the kernel does: through core's migration
// runner, into plugin_schema_migrations under the lane's namespace. It is a
// test-support package (the net/http/httptest pattern): no production code
// imports it.
//
// Plugins register their schema with app.Registry.AddMigrations and their stores
// no longer create tables themselves
// (T-20261113-premium-has-no-schema-migration-chain-41-create), so a plugin test
// that builds a store over a database needs the lane applied first. Plugins live
// in other modules and cannot reach internal/migrate; this is the narrow door,
// and it opens onto the same runner — not a second way to apply migrations.
package migratetest

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/migrate"
)

// ApplyLane validates the lane exactly as Registry.AddMigrations does and applies
// every pending migration of it to pool, with no boot Policy (a test database is
// neither protected nor snapshotted). Already-applied versions are skipped, and a
// database carrying a version the lane does not know is refused, as at boot.
func ApplyLane(ctx context.Context, pool *pgxpool.Pool, namespace string, fsys fs.FS) error {
	lane := migrate.Lane{Namespace: namespace, FS: fsys}
	if err := migrate.ValidateLane(lane); err != nil {
		return err
	}
	return migrate.MigrateLanes(ctx, pool, []migrate.Lane{lane})
}
