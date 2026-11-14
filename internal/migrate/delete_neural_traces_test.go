package migrate

import (
	"strings"
	"testing"
)

// Tests for 0045_delete_neural_traces.sql (owner decision 2026-11-14, ENG-1462:
// neural traces are deleted completely).

const deleteNeuralTracesVersion int64 = 45

func TestLoadMigrations_Includes0045DeleteNeuralTraces(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != deleteNeuralTracesVersion {
			continue
		}
		if m.name != "delete_neural_traces" {
			t.Fatalf("migration 45 is %q, want delete_neural_traces", m.name)
		}
		for _, s := range []string{
			"DELETE FROM document_edges",
			"DELETE FROM chunk_embeddings",
			"DELETE FROM chunks WHERE document_type = 'neural_trace'",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0045 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DROP ") || strings.Contains(l, "ALTER ") || strings.Contains(l, "TRUNCATE") {
				t.Errorf("0045 must delete rows only: %q", line)
			}
		}
		return
	}
	t.Fatal("0045_delete_neural_traces is not embedded")
}

// Trace rows and anything hanging off them go; every other row stays. Replaying
// the file is a no-op. Postgres-backed (splitTestPool skips without a DSN).
func TestDeleteNeuralTraces_RemovesTracesOnlyAndIsIdempotent(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })
	applyThrough(ctx, t, pool, deleteNeuralTracesVersion-1)

	for _, s := range []string{
		`INSERT INTO chunks (id, text, document_type) VALUES
			('trace-lease-1-aa', 'PROMPT: secret context RESPONSE: x', 'neural_trace'),
			('trace-run:0:0-bb', 'thought', 'neural_trace'),
			('fact-1', 'a fact', 'mnemonic_fact'),
			('fact-2', 'another fact', 'mnemonic_fact')`,
		`INSERT INTO chunk_embeddings (chunk_id, model_id, dims, embedding) VALUES
			('trace-lease-1-aa', 'm', 4, '[1,0,0,0]'),
			('fact-1', 'm', 4, '[0,1,0,0]')`,
		`INSERT INTO document_edges (source_id, target_id, edge_type, weight) VALUES
			('fact-1', 'trace-lease-1-aa', 'co_activated', 0.4),
			('fact-1', 'fact-2', 'co_activated', 0.4)`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}

	applyThrough(ctx, t, pool, deleteNeuralTracesVersion)

	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks WHERE document_type = 'neural_trace'`); n != 0 {
		t.Errorf("trace chunks left: %d", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks`); n != 2 {
		t.Errorf("chunks = %d, want 2 (the facts)", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunk_embeddings`); n != 1 {
		t.Errorf("embeddings = %d, want 1 (fact-1's)", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM document_edges`); n != 1 {
		t.Errorf("edges = %d, want 1 (fact-1 → fact-2)", n)
	}

	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == deleteNeuralTracesVersion {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("replaying 0045: %v", err)
			}
		}
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks`); n != 2 {
		t.Errorf("replay changed the chunks: %d", n)
	}
}
