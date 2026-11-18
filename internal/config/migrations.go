package config

import (
	"fmt"
	"strings"
)

// MigrationsConfig is the boot-time migration guard, config block database.migrations
// (ADR-0064 amendment 2026-11-04, T-20261104-kernel-binaries-auto-apply-migrations-to-the).
//
// Boot used to apply every pending migration to whatever database the config named. On
// 2026-11-03 a smoke-test build booted on the owner's working database by mistake and
// moved its schema forward, after which every older binary refused it as "ahead". The
// guard lets an operator say which databases a boot may not migrate on its own.
//
// It is read by postgres.NewPgVectorAdapter (every kernel and every cmd/ tool that opens
// the vector store) and handed to migrate.Boot. The explicit `migrate up` subcommand
// honours ProtectedDBNames as well (since 2026-11-06,
// T-20261106-orchestrator-migrate-up-ignores-database): it refuses a protected database
// with pending changes unless AllowPending is set for that one command; it ignores Mode.
// storage.auto_migrate=false still turns the boot runner off entirely and wins over Mode.
// Both paths also take a mandatory snapshot (the `backup` block) before applying a
// migration marked destructive to a database with history, and refuse if it fails,
// unless AllowDestructiveWithoutSnapshot is set (ENG-681).
type MigrationsConfig struct {
	// Mode: "auto" (default — apply pending migrations at boot, the historical
	// behaviour), "refuse_pending" (boot only when nothing is pending; otherwise exit
	// before any write, naming the pending changes), or "off" (skip the runner at boot).
	Mode string `json:"mode"`
	// ProtectedDBNames are databases treated as refuse_pending whatever Mode says.
	// Matched case-insensitively against the database actually connected. Default empty.
	// The owner protects the working database with e.g. ["cambrian_db"] in configs/config.json.
	ProtectedDBNames []string `json:"protected_dbnames"`
	// AllowPending is the explicit one-boot opt-in that lets a guarded boot apply its
	// pending changes: CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true. Do not leave it
	// set in a file — that disables the guard permanently.
	AllowPending bool `json:"allow_pending"`
	// AllowDestructiveWithoutSnapshot is the explicit operator override that lets the
	// runner apply a DESTRUCTIVE migration (internal/migrate destructive.go) to a database
	// with history WITHOUT the mandatory pre-migration snapshot (ENG-681). Like
	// AllowPending it is meant for one command —
	// CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_DESTRUCTIVE_WITHOUT_SNAPSHOT=true or
	// `migrate up --skip-snapshot` — not for a file.
	AllowDestructiveWithoutSnapshot bool `json:"allow_destructive_without_snapshot"`
	// LockWaitSeconds is how long a boot or `migrate up` waits for the migration lock
	// while another binary migrates the same database (internal/migrate lock.go,
	// ENG-2029) before it fails with "timed out waiting for the migration lock".
	// Default 900 (15 minutes: the holder may be building an index concurrently on a
	// large table); 0 also means the runner's default; negative is refused.
	LockWaitSeconds int `json:"lock_wait_seconds"`
}

// MigrationModes are the accepted values of database.migrations.mode. The runner
// (internal/migrate.ParseMode) accepts the same set; config does not import the runner.
var MigrationModes = []string{"auto", "refuse_pending", "off"}

func (m MigrationsConfig) validate() []string {
	var errs []string
	if m.LockWaitSeconds < 0 {
		errs = append(errs, fmt.Sprintf("database.migrations.lock_wait_seconds %d must be >= 0 (0 = the runner's default)",
			m.LockWaitSeconds))
	}
	mode := strings.ToLower(strings.TrimSpace(m.Mode))
	if mode == "" {
		return errs
	}
	for _, ok := range MigrationModes {
		if mode == ok {
			return errs
		}
	}
	return append(errs, fmt.Sprintf("database.migrations.mode %q is not one of %s",
		m.Mode, strings.Join(MigrationModes, " | ")))
}
