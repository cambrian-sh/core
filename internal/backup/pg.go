package backup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PG is how to reach one Postgres database and its client tools.
type PG struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	// Container, when set, runs pg_dump / pg_restore inside this Docker container over its
	// local socket (backup.pg_container); otherwise they come from PATH.
	Container string
}

// WithDB is p pointed at another database on the same server.
func (p PG) WithDB(db string) PG { p.DBName = db; return p }

// connString is a pgx URL; the password is escaped and never logged.
func (p PG) connString(db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(p.User, p.Password),
		Host: net.JoinHostPort(p.Host, p.Port), Path: "/" + db, RawQuery: "sslmode=disable"}
	return u.String()
}

func (p PG) connect(ctx context.Context, db string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, p.connString(db))
	if err != nil {
		return nil, fmt.Errorf("connect to %s@%s:%s/%s: %w", p.User, p.Host, p.Port, db, err)
	}
	return conn, nil
}

// containerSocket is where the official postgres images listen locally.
const containerSocket = "/var/run/postgresql"

// tool builds the command for a client tool (pg_dump, pg_restore). The password travels
// in PGPASSWORD (for docker: `-e PGPASSWORD`, value taken from the client environment),
// never on a command line.
func (p PG) tool(ctx context.Context, name string, stdin bool, args ...string) (*exec.Cmd, error) {
	env := append(os.Environ(), "PGPASSWORD="+p.Password)
	if p.Container != "" {
		dargs := []string{"exec"}
		if stdin {
			dargs = append(dargs, "-i")
		}
		dargs = append(dargs, "-e", "PGPASSWORD", p.Container, name, "-h", containerSocket, "-U", p.User)
		cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
		cmd.Env = env
		return cmd, nil
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not on PATH: install the PostgreSQL client tools (same major version as the server or newer), "+
			"or set backup.pg_container to run them inside the database container", name)
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-h", p.Host, "-p", p.Port, "-U", p.User}, args...)...)
	cmd.Env = env
	return cmd, nil
}

// DumpInfo is what DumpDatabase learned at the dump's snapshot.
type DumpInfo struct {
	Database      string
	SchemaVersion int64
	Tables        []TableCount
}

// DumpDatabase writes `pg_dump -Fc` of p.DBName to out. The per-table row counts are read
// inside a REPEATABLE READ transaction whose snapshot is exported and handed to pg_dump
// (--snapshot), so the counts describe exactly the rows in the dump.
func DumpDatabase(ctx context.Context, p PG, out string) (*DumpInfo, error) {
	conn, err := p.connect(ctx, p.DBName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; ends the exported snapshot
	var snap string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snap); err != nil {
		return nil, fmt.Errorf("export snapshot: %w", err)
	}
	info := &DumpInfo{}
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&info.Database); err != nil {
		return nil, err
	}
	if info.Tables, err = countTables(ctx, tx); err != nil {
		return nil, err
	}
	if info.SchemaVersion, err = schemaVersion(ctx, tx); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd, err := p.tool(ctx, "pg_dump", false, "-d", p.DBName, "-Fc", "--snapshot="+snap)
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = f, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 64 {
		return nil, fmt.Errorf("pg_dump wrote a suspiciously small archive (%d bytes)", st.Size())
	}
	return info, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// countTables returns the exact row count of every ordinary and partitioned table in the
// user schemas, sorted by name.
func countTables(ctx context.Context, q querier) ([]TableCount, error) {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var names [][2]string
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, [2]string{s, t})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TableCount, 0, len(names))
	for _, n := range names {
		ident := pgx.Identifier{n[0], n[1]}
		var c int64
		if err := q.QueryRow(ctx, "SELECT count(*) FROM "+ident.Sanitize()).Scan(&c); err != nil {
			return nil, fmt.Errorf("count %s: %w", ident.Sanitize(), err)
		}
		out = append(out, TableCount{Table: n[0] + "." + n[1], Rows: c})
	}
	return out, nil
}

func schemaVersion(ctx context.Context, q querier) (int64, error) {
	var has bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&has); err != nil {
		return 0, err
	}
	if !has {
		return 0, nil
	}
	var v *int64
	if err := q.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

// CountTables connects to p.DBName and returns its exact table row counts.
func CountTables(ctx context.Context, p PG) ([]TableCount, error) {
	conn, err := p.connect(ctx, p.DBName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	return countTables(ctx, conn)
}

// ListArchive runs `pg_restore --list` on a dump: the archive must parse.
func (p PG) ListArchive(ctx context.Context, dump string) error {
	in, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer in.Close()
	cmd, err := p.tool(ctx, "pg_restore", true, "--list")
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, nil, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore --list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// restoreArchive loads a dump into p.DBName. clean adds --clean --if-exists (replace
// the objects in an existing database); otherwise the database must be empty.
func (p PG) restoreArchive(ctx context.Context, dump string, clean bool) error {
	in, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer in.Close()
	args := []string{"-d", p.DBName, "--no-owner", "--no-acl", "--exit-on-error"}
	if clean {
		args = append(args, "--clean", "--if-exists")
	}
	cmd, err := p.tool(ctx, "pg_restore", true, args...)
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = in, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore into %s: %w: %s", p.DBName, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// databaseExists reports whether db exists on p's server, and its comment.
func (p PG) databaseExists(ctx context.Context, db string) (bool, string, error) {
	conn, err := p.connect(ctx, "postgres")
	if err != nil {
		return false, "", err
	}
	defer conn.Close(ctx)
	var comment *string
	err = conn.QueryRow(ctx,
		`SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, db).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if comment == nil {
		return true, "", nil
	}
	return true, *comment, nil
}

// createDatabase creates db (which must not exist) and, when comment is non-empty, marks it.
func (p PG) createDatabase(ctx context.Context, db, comment string) error {
	conn, err := p.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{db}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return fmt.Errorf("create database %s: %w", db, err)
	}
	if comment != "" {
		if _, err := conn.Exec(ctx, "COMMENT ON DATABASE "+ident+" IS "+quoteLiteral(comment)); err != nil {
			return fmt.Errorf("mark database %s: %w", db, err)
		}
	}
	return nil
}

// dropDatabase drops db (terminating its connections). Callers check ownership first.
func (p PG) dropDatabase(ctx context.Context, db string) error {
	conn, err := p.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", db, err)
	}
	return nil
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
