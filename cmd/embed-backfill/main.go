// embed-backfill populates the chunk_embeddings projection (ADR-0107 D4) and
// creates the model's partial HNSW index. Its wall-clock IS the "projection
// rebuild RTO" row of the substrate's performance contract — run it, and the
// numbers it prints go in DECISIONS.md.
//
// v1 backfills the ACTIVE model by copying chunks.embedding: the column IS that
// model's projection today, so copying is the exact rebuild — byte-identical
// vectors, which is what makes stage 3b's "recall unchanged" gate meaningful.
// Re-embedding under a DIFFERENT model (stage 3c) needs an embedder client and
// arrives with the second model.
//
// Idempotent: rows are inserted ON CONFLICT DO NOTHING, the index with IF NOT
// EXISTS; running it twice is a fast no-op.
//
// Usage:
//
//	embed-backfill "postgres://user:pw@host:5432/db" -model bge-large -dims 1024
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embed-backfill:", err)
		os.Exit(1)
	}
}

var indexSlug = regexp.MustCompile(`[^a-z0-9]+`)

func run() error {
	fs := flag.NewFlagSet("embed-backfill", flag.ExitOnError)
	model := fs.String("model", "", "model id the projection rows are recorded under (required; must match embedder.model)")
	dims := fs.Int("dims", 0, "vector dimensions for this model (required; must match embedder.dimensions)")
	skipIndex := fs.Bool("skip-index", false, "backfill rows only; do not create the partial HNSW index")
	reembed := fs.Bool("reembed", false, "embed chunk TEXT under -model via the embedder endpoint (stage 3c: a SECOND model) instead of copying the legacy column")
	endpoint := fs.String("endpoint", "http://localhost:11434", "ollama-compatible endpoint for -reembed")
	batch := fs.Int("batch", 32, "texts per embed request in -reembed mode")
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: embed-backfill <dsn> -model <id> -dims <n> [-skip-index]")
	}
	dsn := os.Args[1]
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *model == "" || *dims <= 0 {
		return fmt.Errorf("-model and -dims are required (they are the projection's identity, ADR-0107 D3)")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Coverage before, so the report can say what the run actually did.
	var chunksWithEmbedding, alreadyProjected int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE embedding IS NOT NULL`).Scan(&chunksWithEmbedding); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chunk_embeddings WHERE model_id = $1`, *model).Scan(&alreadyProjected); err != nil {
		return err
	}

	start := time.Now()
	var inserted int64
	if *reembed {
		n, err := reembedRows(ctx, pool, *endpoint, *model, *dims, *batch)
		if err != nil {
			return err
		}
		inserted = n
	} else {
		tag, err := pool.Exec(ctx, `
			INSERT INTO chunk_embeddings (chunk_id, model_id, model_version, dims, embedding)
			SELECT id, $1, '', $2, embedding FROM chunks WHERE embedding IS NOT NULL
			ON CONFLICT (chunk_id, model_id) DO NOTHING`, *model, *dims)
		if err != nil {
			return fmt.Errorf("backfill rows: %w", err)
		}
		inserted = tag.RowsAffected()
	}
	rowsDur := time.Since(start)

	var indexDur time.Duration
	if !*skipIndex {
		// The index name embeds the model so each model's index is its own
		// object; the cast + WHERE must match the read path's SQL VERBATIM
		// (ADR-0107 D2) or the planner never uses it.
		name := "chunk_embeddings_" + indexSlug.ReplaceAllString(*model, "_") + "_hnsw"
		istart := time.Now()
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON chunk_embeddings
			 USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
			 WHERE model_id = '%s'`, name, *dims, *model)); err != nil {
			return fmt.Errorf("create index %s: %w", name, err)
		}
		indexDur = time.Since(istart)
	}

	var projected int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chunk_embeddings WHERE model_id = $1`, *model).Scan(&projected); err != nil {
		return err
	}

	fmt.Printf("model=%s dims=%d reembed=%t\n", *model, *dims, *reembed)
	fmt.Printf("chunks_with_embedding=%d already_projected=%d inserted=%d now_projected=%d\n",
		chunksWithEmbedding, alreadyProjected, inserted, projected)
	fmt.Printf("rows_wall=%s index_wall=%s total_wall=%s\n", rowsDur, indexDur, rowsDur+indexDur)
	if projected < chunksWithEmbedding {
		fmt.Printf("WARNING: coverage incomplete (%d of %d) — enabling embedding_projection_read now would hide the difference from retrieval\n",
			projected, chunksWithEmbedding)
	}
	return nil
}

// reembedRows embeds chunk TEXT under `model` via an ollama-compatible
// /api/embed endpoint and inserts projection rows for chunks the model does
// not cover yet. Restart-safe: the not-covered predicate makes every run pick
// up where the last one stopped.
func reembedRows(ctx context.Context, pool *pgxpool.Pool, endpoint, model string, dims, batch int) (int64, error) {
	type chunkRow struct{ id, text string }
	var total int64
	client := &http.Client{Timeout: 120 * time.Second}
	for {
		rows, err := pool.Query(ctx, `
			SELECT c.id, c.text FROM chunks c
			WHERE c.text <> '' AND NOT EXISTS (
				SELECT 1 FROM chunk_embeddings ce
				WHERE ce.chunk_id = c.id AND ce.model_id = $1)
			ORDER BY c.id LIMIT $2`, model, batch)
		if err != nil {
			return total, err
		}
		var pending []chunkRow
		for rows.Next() {
			var r chunkRow
			if err := rows.Scan(&r.id, &r.text); err != nil {
				rows.Close()
				return total, err
			}
			pending = append(pending, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		if len(pending) == 0 {
			return total, nil
		}

		texts := make([]string, len(pending))
		for i, r := range pending {
			// Bounded input: a chunk longer than the model's context makes the
			// whole request fail. Truncation only affects the backfilled vector
			// for pathological chunks, and is preferable to skipping them —
			// an unprojected chunk is invisible through the projection.
			if len(r.text) > 8000 {
				r.text = r.text[:8000]
			}
			texts[i] = r.text
		}
		embs, err := embedBatch(ctx, client, endpoint, model, texts)
		if err != nil {
			// Batches fail as a unit; retry one-by-one so a single poisoned
			// text costs one row (reported), not the whole run.
			embs = make([][]float32, len(texts))
			for i, t := range texts {
				one, oerr := embedBatch(ctx, client, endpoint, model, []string{t})
				if oerr != nil || len(one) != 1 {
					return total, fmt.Errorf("chunk %s: %v", pending[i].id, oerr)
				}
				embs[i] = one[0]
			}
		}
		out := struct{ Embeddings [][]float32 }{Embeddings: embs}
		if len(out.Embeddings) != len(pending) {
			return total, fmt.Errorf("embed response carried %d vectors for %d inputs", len(out.Embeddings), len(pending))
		}
		for i, vec := range out.Embeddings {
			if len(vec) != dims {
				return total, fmt.Errorf("model %s returned %d dims, expected %d — fix -dims or the model", model, len(vec), dims)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO chunk_embeddings (chunk_id, model_id, model_version, dims, embedding)
				VALUES ($1, $2, '', $3, $4)
				ON CONFLICT (chunk_id, model_id) DO NOTHING`,
				pending[i].id, model, dims, pgvector.NewVector(vec)); err != nil {
				return total, err
			}
			total++
		}
		fmt.Printf("  reembedded %d…\n", total)
	}
}

// embedBatch calls the ollama-compatible /api/embed and surfaces the server's
// own error text — a silent zero-vector response is undebuggable.
func embedBatch(ctx context.Context, client *http.Client, endpoint, model string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("embedder: %s", out.Error)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed response carried %d vectors for %d inputs", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}
