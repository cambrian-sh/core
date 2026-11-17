package migrate

import (
	"strings"
	"testing"
)

// 0055_object_fence_class_row_tags.sql (ADR-0158 D2/D7, objects; ENG-1986):
// fence classes become (tags, parties, row_tags) triples. Not destructive.
func TestLoadMigrations_Includes0055ObjectFenceClassRowTags(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 55 {
			continue
		}
		if m.name != "object_fence_class_row_tags" {
			t.Fatalf("migration 55 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0055 deletes and rewrites nothing: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0055 must carry the not-destructive marker (every migration from 0051 on does)")
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DELETE ") || strings.Contains(l, "UPDATE ") || strings.Contains(l, "DROP TABLE") || strings.Contains(l, "DROP COLUMN") {
				t.Errorf("0055 must not delete or rewrite rows: %q", line)
			}
		}
		return
	}
	t.Fatal("0055_object_fence_class_row_tags is not embedded")
}

// On a database whose dictionary already holds pairs, 0055 keeps every pair under
// its id as the triple (tags, parties, '{}') — so object rows' vis_classes stay
// valid — moves the unique key to the triple, and is idempotent.
func TestObjectFenceClassRowTags0055_ExistingPairsKeepTheirIds_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	applyThrough(ctx, t, pool, 54)
	if _, err := pool.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties) VALUES ('{ops}', '{}'), ('{deal:x,sales}', '{p1,p2}')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	type pair struct {
		id            int32
		tags, parties string
	}
	read := func(q string) []pair {
		rows, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.id, &p.tags, &p.parties); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	before := read(`SELECT id, tags::text, parties::text FROM object_fence_classes ORDER BY id`)

	applyThrough(ctx, t, pool, 55)

	after := read(`SELECT id, tags::text, parties::text FROM object_fence_classes WHERE row_tags = '{}' ORDER BY id`)
	if len(after) != len(before) || len(before) != 2 {
		t.Fatalf("pairs %v became %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("a pair changed id or content: %v -> %v", before[i], after[i])
		}
	}
	// The key is the triple: the same pair with row tags is a new class, the same
	// triple twice is refused.
	if _, err := pool.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties, row_tags) VALUES ('{ops}', '{}', '{hr}')`); err != nil {
		t.Fatalf("a pair with row tags is a distinct class: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties, row_tags) VALUES ('{ops}', '{}', '{hr}')`); err == nil {
		t.Fatal("a duplicate triple was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties) VALUES ('{ops}', '{}')`); err == nil {
		t.Fatal("a duplicate pair (row_tags '{}') was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE object_fence_classes SET row_tags = NULL WHERE id = 1`); err == nil {
		t.Fatal("row_tags accepted NULL")
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conname = 'object_fence_classes_pair_unique'`); n != 0 {
		t.Fatal("the old pair key is still there")
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM pg_indexes WHERE indexname = 'object_fence_classes_row_tagged'`); n != 1 {
		t.Fatal("the row-tagged probe index is missing")
	}
	// ON CONFLICT on the triple — the maintainer's intern statement — works.
	if _, err := pool.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties, row_tags) VALUES ('{ops}', '{}', '{hr}')
		ON CONFLICT (tags, parties, row_tags) DO NOTHING`); err != nil {
		t.Fatalf("ON CONFLICT on the triple: %v", err)
	}
	// Re-running is a no-op.
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if m.version == 55 {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("0055 is not idempotent: %v", err)
			}
		}
	}
}
