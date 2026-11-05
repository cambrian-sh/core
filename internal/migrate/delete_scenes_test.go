package migrate

import (
	"strings"
	"testing"
)

// Tests for 0028_delete_scenes.sql (owner decision 2026-11-05,
// cambrian-knowledge/decisions/scenes-cancelled.md).

const deleteScenesVersion int64 = 28

func TestLoadMigrations_Includes0028DeleteScenes(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != deleteScenesVersion {
			continue
		}
		if m.name != "delete_scenes" {
			t.Fatalf("migration 28 is %q, want delete_scenes", m.name)
		}
		for _, s := range []string{
			"DELETE FROM document_edges",
			"'discussed_in', 'engaged'",
			"DELETE FROM chunk_embeddings",
			"DELETE FROM chunks WHERE document_type = 'mnemonic_scene'",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0028 lacks %q", s)
			}
		}
		// Rows only: the file must not drop a table or column.
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DROP ") || strings.Contains(l, "ALTER ") || strings.Contains(l, "TRUNCATE") {
				t.Errorf("0028 must delete rows only: %q", line)
			}
		}
		return
	}
	t.Fatal("0028_delete_scenes is not embedded")
}

// The scenes, their embeddings and their scene-only or scene-touching edges go;
// everything else — other chunks, their embeddings, edges between them, the episode
// parent — stays. Replaying the file is a no-op.
func TestDeleteScenes_RemovesScenesOnlyAndIsIdempotent(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })
	applyThrough(ctx, t, pool, deleteScenesVersion-1)

	seed := []string{
		`INSERT INTO chunks (id, text, document_type) VALUES
			('scene-p1', 'plan goal: x', 'mnemonic_scene'),
			('f1-scene', 'old step scene', 'mnemonic_scene'),
			('fact-1', 'a fact', 'mnemonic_fact'),
			('fact-2', 'another fact', 'mnemonic_fact'),
			('action-1', 'write_file → ok', 'mnemonic_action')`,
		`INSERT INTO chunk_embeddings (chunk_id, model_id, dims, embedding) VALUES
			('scene-p1', 'm', 4, '[1,0,0,0]'),
			('fact-1', 'm', 4, '[0,1,0,0]')`,
		`INSERT INTO document_edges (source_id, target_id, edge_type, weight) VALUES
			('fact-1', 'scene-p1', 'discussed_in', 0.7),
			('fact-1', 'gone-scene', 'discussed_in', 0.7),
			('scene-p1', 'file:a.md', 'engaged', 0.5),
			('action-1', 'f1-scene', 'co_activated', 0.4),
			('fact-1', 'fact-2', 'co_activated', 0.4),
			('fact-2', 'fact-1', 'follows', 0.5),
			('fact-1', 'sec-1', 'part_of', 1.0)`,
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}

	applyThrough(ctx, t, pool, deleteScenesVersion)

	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks WHERE document_type = 'mnemonic_scene'`); n != 0 {
		t.Errorf("scene chunks left: %d", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks`); n != 3 {
		t.Errorf("non-scene chunks = %d, want 3 (two facts, one action)", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunk_embeddings`); n != 1 {
		t.Errorf("embeddings = %d, want 1 (fact-1's)", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM document_edges WHERE edge_type IN ('discussed_in', 'engaged')`); n != 0 {
		t.Errorf("scene-only edge types left: %d", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM document_edges`); n != 3 {
		t.Errorf("edges = %d, want 3 (co_activated, follows and part_of between surviving rows)", n)
	}

	// Replay: the file's own SQL on an already-cleaned database changes nothing.
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == deleteScenesVersion {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("replaying 0028: %v", err)
			}
		}
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM document_edges`); n != 3 {
		t.Errorf("replay changed the edges: %d", n)
	}
}
