package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/backup"
	"github.com/cambrian-sh/core/internal/migrate"
)

// The `orchestrator backup` CLI end to end (ENG-681): a kernel bundle (configs/config.json
// naming a real, migrated, seeded database) is backed up by `backup run`, checked by
// `backup verify`, drilled by `backup drill` (restore into a scratch database + row-count
// check) and listed. Needs PG_TEST_DSN (server + CREATEDB role; its database is not
// touched) and pg_dump from PATH or inside PG_TEST_CONTAINER. Creates and drops only its
// own databases.
func TestBackupCLI_RunVerifyDrill_EndToEnd_Pg(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	container := os.Getenv("PG_TEST_CONTAINER")
	if container == "" {
		if _, err := exec.LookPath("pg_dump"); err != nil {
			t.Skip("pg_dump not on PATH and PG_TEST_CONTAINER not set")
		}
	}
	ctx := context.Background()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("cambrian_backup_cli_e2e_%d", time.Now().UnixNano()%1_000_000_000)
	admin := func(sql string) {
		c := cc.Copy()
		c.Database = "postgres"
		conn, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	admin("CREATE DATABASE " + db)
	t.Cleanup(func() {
		admin("DROP DATABASE IF EXISTS " + db + " WITH (FORCE)")
		admin("DROP DATABASE IF EXISTS " + db + "_restore_drill WITH (FORCE)")
	})
	c := cc.Copy()
	c.Database = db
	pc, _ := pgxpool.ParseConfig("")
	pc.ConnConfig = c
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Migrate(ctx, pool, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE cli_rows AS SELECT g AS id FROM generate_series(1, 321) g`); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"database": map[string]any{"host": cc.Host, "port": fmt.Sprint(cc.Port), "user": cc.User, "password": cc.Password, "dbname": db},
		"embedder": map[string]any{"dimensions": 8},
		"storage":  map[string]any{"data_dir": "data"},
		"backup":   map[string]any{"pg_container": container},
	}
	b, _ := json.Marshal(bundle)
	if err := os.WriteFile(filepath.Join(base, "configs", "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "configs", "secret.key"), []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigStoreEnv, "")

	cli := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := runBackup(ctx, append(args, "--dir", base), &out, &errb)
		return code, out.String() + errb.String()
	}

	// Without the passphrase the run refuses (secret.key exists) and writes nothing.
	t.Setenv("CAMBRIAN_BACKUP_PASSPHRASE", "")
	if code, out := cli("run", "--kind", "manual"); code == 0 || !strings.Contains(out, "CAMBRIAN_BACKUP_PASSPHRASE") {
		t.Fatalf("run without passphrase: code %d\n%s", code, out)
	}
	t.Setenv("CAMBRIAN_BACKUP_PASSPHRASE", "cli e2e")
	if code, out := cli("run", "--kind", "manual"); code != 0 {
		t.Fatalf("run: code %d\n%s", code, out)
	}
	if code, out := cli("verify"); code != 0 || !strings.Contains(out, "verified") {
		t.Fatalf("verify: code %d\n%s", code, out)
	}
	code, out := cli("drill")
	if code != 0 || !strings.Contains(out, "passed") {
		t.Fatalf("drill: code %d\n%s", code, out)
	}
	if code, out := cli("list"); code != 0 || !strings.Contains(out, "-manual") {
		t.Fatalf("list: code %d\n%s", code, out)
	}
	run, err := backup.Latest(filepath.Join(base, "backups"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run, "secret.key")); err == nil {
		t.Fatal("secret.key in the run folder")
	}
	m, _ := backup.ReadManifest(run)
	if m.SecretKey == nil || !strings.HasPrefix(m.SecretKey.Path, filepath.Join(base, "backups-keys")) {
		t.Fatalf("encrypted key not in the key root: %+v", m.SecretKey)
	}
}
