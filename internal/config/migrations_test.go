package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// database.migrations (T-20261104-kernel-binaries-auto-apply-migrations-to-the): the
// default is the historical behaviour, the file names the protected databases, and the
// one-boot opt-in arrives through the environment.
func writeMigrationsConfig(t *testing.T, database string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"database":{"host":"h","user":"u","password":"p","dbname":"d"` + database + `},"server":{"port":"50051"}}`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrationsConfig_DefaultIsAutoUnprotected(t *testing.T) {
	cfg, err := config.LoadConfig(writeMigrationsConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Database.Migrations
	if m.Mode != "auto" || len(m.ProtectedDBNames) != 0 || m.AllowPending {
		t.Fatalf("default migrations config = %+v; want auto, nothing protected, no opt-in", m)
	}
}

func TestMigrationsConfig_FileProtectsAndEnvOptsIn(t *testing.T) {
	t.Setenv("CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING", "true")
	cfg, err := config.LoadConfig(writeMigrationsConfig(t,
		`,"migrations":{"mode":"refuse_pending","protected_dbnames":["cambrian_db"]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Database.Migrations
	if m.Mode != "refuse_pending" || len(m.ProtectedDBNames) != 1 || m.ProtectedDBNames[0] != "cambrian_db" || !m.AllowPending {
		t.Fatalf("migrations config = %+v", m)
	}
}

func TestMigrationsConfig_RejectsUnknownMode(t *testing.T) {
	_, err := config.LoadConfig(writeMigrationsConfig(t, `,"migrations":{"mode":"sometimes"}`))
	if err == nil || !strings.Contains(err.Error(), "database.migrations.mode") {
		t.Fatalf("want a validation error naming database.migrations.mode, got %v", err)
	}
}

// database.migrations.lock_wait_seconds (ENG-2029): defaults to 15 minutes, takes a
// file value, and refuses a negative one.
func TestMigrationsConfig_LockWaitSeconds(t *testing.T) {
	cfg, err := config.LoadConfig(writeMigrationsConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Database.Migrations.LockWaitSeconds; got != 900 {
		t.Fatalf("default lock_wait_seconds = %d, want 900", got)
	}
	cfg, err = config.LoadConfig(writeMigrationsConfig(t, `,"migrations":{"lock_wait_seconds":60}`))
	if err != nil || cfg.Database.Migrations.LockWaitSeconds != 60 {
		t.Fatalf("file lock_wait_seconds: %+v %v", cfg, err)
	}
	_, err = config.LoadConfig(writeMigrationsConfig(t, `,"migrations":{"lock_wait_seconds":-1}`))
	if err == nil || !strings.Contains(err.Error(), "database.migrations.lock_wait_seconds") {
		t.Fatalf("want a validation error naming database.migrations.lock_wait_seconds, got %v", err)
	}
}
