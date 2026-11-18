package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pgvector extension before the pool (T-20261118-kernel-image-run-without-a-mounted-config-json,
// ENG-2061).
//
// 0001_baseline.sql creates the extension, but the kernel's pool registers the vector
// types on every new connection (pgxvector.RegisterTypes in AfterConnect), so on a
// fresh database without the extension no pool connection can be made and the boot
// failed on the runner's first read with "vector type not found" — the baseline that
// would have created the extension never ran. EnsureVectorExtension closes that gap:
// it runs on a plain connection before the pool exists and either creates the
// extension or refuses with the SQL the operator must run.
//
// Creating it is the same schema change the baseline makes, so it is permitted
// exactly when the boot policy would apply the baseline: Mode auto on a database not
// in protected_dbnames, or a protected / refuse_pending boot with allow_pending. With
// the runner off (storage.auto_migrate=false or mode off) the kernel writes no schema
// and refuses instead. It runs under the migration lock (lock.go): two binaries
// racing CREATE EXTENSION IF NOT EXISTS can still collide on pg_extension's unique
// index, and a waiting boot re-checks after the holder finished.
//
// Rejected: CREATE EXTENSION in AfterConnect (a DDL attempt per pooled connection,
// outside the lock and the policy); registering the vector types lazily (every
// query path would have to tolerate an unregistered type on a live pool).

// VectorExtensionSQL is the statement an operator runs when the kernel may not create
// the extension itself.
const VectorExtensionSQL = "CREATE EXTENSION IF NOT EXISTS vector;"

// ErrVectorExtensionMissing is returned (wrapped) when the database has no pgvector
// extension and this boot may not, or could not, create it.
var ErrVectorExtensionMissing = errors.New("the pgvector extension is missing")

// EnsureVectorExtension makes sure the database cc points at has the pgvector
// extension. autoMigrate is storage.auto_migrate; p is the boot policy. cc is used
// for its own short-lived connections and is not modified.
func EnsureVectorExtension(ctx context.Context, cc *pgx.ConnConfig, autoMigrate bool, p Policy) error {
	conn, err := pgx.ConnectConfig(ctx, cc.Copy())
	if err != nil {
		return fmt.Errorf("migrate: connect to check the pgvector extension: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }() // best effort: a read-only session

	db, present, err := vectorExtensionPresent(ctx, conn)
	if err != nil || present {
		return err
	}
	mode := p.Mode
	if mode == "" {
		mode = ModeAuto
	}
	switch {
	case !autoMigrate || mode == ModeOff:
		return refuseMissingExtension(db, "the migration runner is off (storage.auto_migrate=false or database.migrations.mode=off)")
	case p.protects(db) && !p.AllowPending:
		return refuseMissingExtension(db, "database.migrations.protected_dbnames lists it")
	case mode == ModeRefusePending && !p.AllowPending:
		return refuseMissingExtension(db, "database.migrations.mode is refuse_pending")
	}

	release, err := acquireMigrationLockConfig(ctx, cc.Copy(), p.LockWait)
	if err != nil {
		return err
	}
	defer release()
	// Re-check under the lock: the binary we waited for may have created it.
	if _, present, err = vectorExtensionPresent(ctx, conn); err != nil || present {
		return err
	}
	if _, err := conn.Exec(ctx, VectorExtensionSQL); err != nil {
		return fmt.Errorf("%w in database %q and this boot could not create it: %w. %s",
			ErrVectorExtensionMissing, db, err, extensionRemedy(err))
	}
	slog.Info("migrate: created the pgvector extension (0001 baseline prerequisite)", "database", db)
	return nil
}

func vectorExtensionPresent(ctx context.Context, conn *pgx.Conn) (db string, present bool, err error) {
	err = conn.QueryRow(ctx, `SELECT current_database(),
		EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&db, &present)
	if err != nil {
		return "", false, fmt.Errorf("migrate: check the pgvector extension: %w", err)
	}
	return db, present, nil
}

func refuseMissingExtension(db, reason string) error {
	return fmt.Errorf("%w in database %q, and this boot does not change the schema (%s). "+
		"Nothing was written. Run as a role allowed to create extensions (a superuser, or the "+
		"database owner where pgvector is trusted): %s",
		ErrVectorExtensionMissing, db, reason, VectorExtensionSQL)
}

// extensionRemedy names what the operator does for the usual CREATE EXTENSION failures.
func extensionRemedy(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "42501": // insufficient_privilege
			return "The configured database.user may not create extensions; run as a superuser (or the database owner where pgvector is trusted): " + VectorExtensionSQL
		case "0A000", "58P01": // extension not available / control file missing
			return "pgvector is not installed on this Postgres server; use a server with pgvector " +
				"(e.g. the pgvector/pgvector image or the postgresql-<version>-pgvector package), then: " + VectorExtensionSQL
		}
	}
	return "Run as a role allowed to create extensions: " + VectorExtensionSQL
}
