package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// ENG-681: destructive migrations need a snapshot first.

func TestIsDestructive_MarkerOnItsOwnLine(t *testing.T) {
	cases := map[string]bool{
		"-- cambrian:destructive\nDROP TABLE x;":                    true,
		"  -- cambrian:destructive drops the legacy table\nDROP x;": true,
		"-- cambrian:destructive: reason\n":                         true,
		"-- cambrian:not-destructive: function body only\n":         false,
		"SELECT 1; -- cambrian:destructive":                         false, // not on its own line
		"-- cambrian:destructiveness is discussed here":             false,
		"DROP TABLE x;": false, // the marker, not the SQL, decides
	}
	for sql, want := range cases {
		if got := IsDestructive(sql); got != want {
			t.Errorf("IsDestructive(%q) = %v, want %v", sql, got, want)
		}
	}
}

// lintDestructive matches data-losing statements, case-insensitively, in SQL with the
// line comments stripped.
var lintDestructive = regexp.MustCompile(`(?i)\b(DROP\s+TABLE|DROP\s+COLUMN|DROP\s+SCHEMA|DROP\s+DATABASE|TRUNCATE|DELETE\s+FROM|ALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE)\b`)

func stripLineComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestMigrations_DestructiveStatementsAreClassified is what keeps the explicit marker
// from being forgotten: every core migration whose SQL contains a data-losing statement
// must carry DestructiveMarker or NotDestructiveMarker (with a reason) — or, for files
// shipped before the marker existed, be classified in shippedClassification.
func TestMigrations_DestructiveStatementsAreClassified(t *testing.T) {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		version, _, err := parseName(path.Base(e))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := migrationFS.ReadFile(e)
		sql := string(raw)
		hit := lintDestructive.FindString(stripLineComments(sql))
		if hit == "" {
			continue
		}
		if _, ok := shippedClassification[version]; ok {
			continue
		}
		hasNot := false
		for _, line := range strings.Split(sql, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, NotDestructiveMarker) && strings.TrimSpace(strings.TrimPrefix(l, NotDestructiveMarker)) != "" {
				hasNot = true
			}
		}
		if !IsDestructive(sql) && !hasNot {
			t.Errorf("%s contains %q but carries neither %q nor %q <reason>: classify it (ENG-681)",
				path.Base(e), hit, DestructiveMarker, NotDestructiveMarker)
		}
	}
}

func TestShippedClassification_OnlyShippedVersions(t *testing.T) {
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	known := map[int64]migration{}
	for _, m := range migs {
		known[m.version] = m
	}
	for v := range shippedClassification {
		if v > lastShippedUnmarked {
			t.Errorf("shippedClassification lists %d > %d: new migrations carry the marker in the file", v, lastShippedUnmarked)
		}
		if _, ok := known[v]; !ok {
			t.Errorf("shippedClassification lists unknown version %d", v)
		}
	}
	for _, v := range []int64{7, 17, 21, 28, 45, 47} {
		if !known[v].destructive {
			t.Errorf("migration %04d should load as destructive", v)
		}
	}
	if known[1].destructive || known[2].destructive {
		t.Error("0001/0002 must not load as destructive")
	}
}

func TestGuardDestructive_Decisions(t *testing.T) {
	ctx := context.Background()
	if err := guardDestructive(ctx, "db", Policy{}, nil); err != nil {
		t.Fatalf("nothing destructive pending: %v", err)
	}
	err := guardDestructive(ctx, "db", Policy{}, []string{"0028_delete_scenes"})
	if !errors.Is(err, ErrSnapshotRequired) || !strings.Contains(err.Error(), "0028_delete_scenes") || !strings.Contains(err.Error(), "--skip-snapshot") {
		t.Fatalf("no snapshotter: want ErrSnapshotRequired naming the migration and the override, got %v", err)
	}
	failing := Policy{Snapshot: func(context.Context, string, []string) (string, error) { return "", fmt.Errorf("pg_dump missing") }}
	if err := guardDestructive(ctx, "db", failing, []string{"x"}); !errors.Is(err, ErrSnapshotRequired) || !strings.Contains(err.Error(), "pg_dump missing") {
		t.Fatalf("failing snapshot: want refusal carrying the cause, got %v", err)
	}
	called := 0
	ok := Policy{Snapshot: func(_ context.Context, db string, d []string) (string, error) {
		called++
		return "/backups/x", nil
	}}
	if err := guardDestructive(ctx, "db", ok, []string{"x"}); err != nil || called != 1 {
		t.Fatalf("working snapshot: err=%v called=%d", err, called)
	}
	override := Policy{AllowDestructiveWithoutSnapshot: true, Snapshot: ok.Snapshot}
	if err := guardDestructive(ctx, "db", override, []string{"x"}); err != nil || called != 1 {
		t.Fatalf("override must proceed without snapshotting: err=%v called=%d", err, called)
	}
}

// The integration half: a database at 0027 with 0028 (destructive) pending.
func TestBoot_DestructiveMigrationNeedsSnapshot_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	maxApplied := func() int64 {
		var v int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	applyThrough(ctx, t, pool, 27)

	// No snapshotter: refused, nothing applied.
	err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto})
	if !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("want ErrSnapshotRequired, got %v", err)
	}
	if v := maxApplied(); v != 27 {
		t.Fatalf("refused boot applied migrations: max version %d, want 27", v)
	}

	// Failing snapshot: refused, nothing applied.
	failing := func(context.Context, string, []string) (string, error) { return "", errors.New("disk full") }
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto, Snapshot: failing}); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("failing snapshot: want ErrSnapshotRequired, got %v", err)
	}
	if v := maxApplied(); v != 27 {
		t.Fatalf("failed snapshot applied migrations: max version %d", v)
	}

	// Working snapshot: called once, before anything is applied, naming every pending destructive migration.
	var seen []string
	snap := func(_ context.Context, db string, d []string) (string, error) {
		if v := maxApplied(); v != 27 {
			t.Errorf("snapshot ran after migrations were applied (max %d)", v)
		}
		seen = append(seen, d...)
		return "snap", nil
	}
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto, Snapshot: snap}); err != nil {
		t.Fatalf("boot with snapshot: %v", err)
	}
	if got := strings.Join(seen, ","); !strings.Contains(got, "0028_delete_scenes") || !strings.Contains(got, "0045_delete_neural_traces") {
		t.Fatalf("snapshot saw %q, want 0028 and 0045 among them", got)
	}
	if v := maxApplied(); v < 45 {
		t.Fatalf("boot with snapshot did not migrate: max %d", v)
	}
}

func TestBoot_FreshDatabaseNeedsNoSnapshot_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	// Every migration (destructive ones included) is pending, but there is no history.
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto}); err != nil {
		t.Fatalf("fresh database must migrate without a snapshot: %v", err)
	}
}

func TestBoot_OverrideAppliesWithoutSnapshot_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	applyThrough(ctx, t, pool, 27)
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto, AllowDestructiveWithoutSnapshot: true}); err != nil {
		t.Fatalf("override: %v", err)
	}
}

func TestBootLanes_DestructiveLaneMigrationNeedsSnapshot_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	v1 := Lane{Namespace: "q2test", FS: laneFS(map[string]string{"0001_t.sql": "CREATE TABLE q2_lane_t (id int);"})}
	if err := MigrateLanes(ctx, pool, []Lane{v1}); err != nil {
		t.Fatal(err)
	}
	v2 := Lane{Namespace: "q2test", FS: laneFS(map[string]string{
		"0001_t.sql":    "CREATE TABLE q2_lane_t (id int);",
		"0002_drop.sql": "-- cambrian:destructive\nDROP TABLE q2_lane_t;",
	})}
	if err := BootLanes(ctx, pool, Policy{Mode: ModeAuto}, []Lane{v2}); !errors.Is(err, ErrSnapshotRequired) ||
		!strings.Contains(err.Error(), "q2test/0002_drop") {
		t.Fatalf("want refusal naming q2test/0002_drop, got %v", err)
	}
	called := false
	snap := func(context.Context, string, []string) (string, error) { called = true; return "s", nil }
	if err := BootLanes(ctx, pool, Policy{Mode: ModeAuto, Snapshot: snap}, []Lane{v2}); err != nil || !called {
		t.Fatalf("with snapshot: err=%v called=%v", err, called)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM plugin_schema_migrations WHERE namespace = 'q2test'`)
}
