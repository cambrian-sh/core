package migrate

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// Plugin migration lanes (T-20261113-premium-has-no-schema-migration-chain-41-create).

func laneFS(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range files {
		out[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return out
}

func TestValidateLane(t *testing.T) {
	ok := laneFS(map[string]string{"0001_baseline.sql": "SELECT 1;", "0002_more.sql": "SELECT 2;"})
	if err := ValidateLane(Lane{Namespace: "premium", FS: ok}); err != nil {
		t.Fatalf("a well-formed lane was refused: %v", err)
	}
	for name, l := range map[string]Lane{
		"reserved namespace": {Namespace: "core", FS: ok},
		"bad namespace":      {Namespace: "Premium!", FS: ok},
		"empty namespace":    {Namespace: "", FS: ok},
		"no files":           {Namespace: "premium", FS: laneFS(map[string]string{"README.md": "x"})},
		"bad file name":      {Namespace: "premium", FS: laneFS(map[string]string{"baseline.sql": "x"})},
		"duplicate version":  {Namespace: "premium", FS: laneFS(map[string]string{"0001_a.sql": "x", "0001_b.sql": "y"})},
		"zero version":       {Namespace: "premium", FS: laneFS(map[string]string{"0000_a.sql": "x"})},
		"nil fs":             {Namespace: "premium"},
	} {
		if err := ValidateLane(l); err == nil {
			t.Errorf("%s: lane accepted", name)
		}
	}
}

func TestLoadLane_OrdersByVersionAndKeepsSQLVerbatim(t *testing.T) {
	migs, err := loadLane(Lane{Namespace: "premium", FS: laneFS(map[string]string{
		"0010_ten.sql": "SELECT ${EMBEDDING_DIM};", "0002_two.sql": "SELECT 2;",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 || migs[0].version != 2 || migs[1].version != 10 || migs[1].name != "ten" {
		t.Fatalf("order/names: %+v", migs)
	}
	if migs[1].sql != "SELECT ${EMBEDDING_DIM};" {
		t.Fatalf("a lane's SQL must not be substituted (no embedding dim for plugins): %q", migs[1].sql)
	}
}

func TestLaneDigest_DistinguishesContent(t *testing.T) {
	a := Lane{Namespace: "premium", FS: laneFS(map[string]string{"0001_baseline.sql": "SELECT 1;"})}
	b := Lane{Namespace: "premium", FS: laneFS(map[string]string{"0001_baseline.sql": "SELECT 1;"})}
	c := Lane{Namespace: "premium", FS: laneFS(map[string]string{"0001_baseline.sql": "SELECT 2;"})}
	da, _ := LaneDigest(a)
	db, _ := LaneDigest(b)
	dc, _ := LaneDigest(c)
	if da != db || da == dc {
		t.Fatalf("digest: same content %q vs %q, different content %q", da, db, dc)
	}
}

func TestRefuseLaneAhead(t *testing.T) {
	migs := []migration{{version: 1, name: "baseline"}}
	if err := refuseLaneAhead("premium", migs, map[int64]bool{1: true}); err != nil {
		t.Fatal(err)
	}
	err := refuseLaneAhead("premium", migs, map[int64]bool{1: true, 7: true})
	if err == nil || !strings.Contains(err.Error(), "premium version 7") {
		t.Fatalf("an applied version this binary does not know must refuse: %v", err)
	}
}

// Postgres: a lane applies into its own ledger namespace, is idempotent, reports
// status, and a protected database refuses before writing (PG_TEST_DSN; the
// shared disposable database — the lead runs these).
func TestPg_BootLanes_AppliesStatusAndGuard(t *testing.T) {
	ctx, pool, db := guardPool(t)
	lane := Lane{Namespace: "testlane", FS: laneFS(map[string]string{
		"0001_baseline.sql": "CREATE TABLE IF NOT EXISTS lane_probe (id TEXT PRIMARY KEY);",
		"0002_column.sql":   "ALTER TABLE lane_probe ADD COLUMN IF NOT EXISTS note TEXT;",
	})}

	// Protected: refused, nothing written (not even the ledger).
	err := BootLanes(ctx, pool, Policy{ProtectedDBNames: []string{db}}, []Lane{lane})
	if !errors.Is(err, ErrPendingRefused) || !strings.Contains(err.Error(), "testlane/0001_baseline") {
		t.Fatalf("want a refusal naming the pending lane migration, got %v", err)
	}
	if relationExists(ctx, t, pool, "plugin_schema_migrations") || relationExists(ctx, t, pool, "lane_probe") {
		t.Fatal("a refused lane boot wrote to the database")
	}

	if err := BootLanes(ctx, pool, Policy{}, []Lane{lane}); err != nil {
		t.Fatal(err)
	}
	if err := BootLanes(ctx, pool, Policy{}, []Lane{lane}); err != nil {
		t.Fatalf("second boot must be a no-op: %v", err)
	}
	recs, err := StatusLanes(ctx, pool, []Lane{lane})
	if err != nil || len(recs) != 2 || recs[0].Pending || recs[1].Pending || recs[1].Namespace != "testlane" {
		t.Fatalf("status: %+v %v", recs, err)
	}
	// Core's ledger is untouched by a lane.
	if relationExists(ctx, t, pool, "schema_migrations") {
		t.Fatal("a plugin lane wrote core's schema_migrations")
	}
	// Mode off skips lanes entirely.
	if err := BootLanes(ctx, pool, Policy{Mode: ModeOff}, []Lane{{Namespace: "other", FS: lane.FS}}); err != nil {
		t.Fatal(err)
	}
}
