package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The ADR-0107 per-model partial HNSW index on chunk_embeddings.
//
// Dense reads on the projection filter `ce.model_id = '<model>'` and order by
// `(ce.embedding::vector(<dims>)) <=> $q`, so each model needs its own partial
// index whose expression and predicate match that SQL VERBATIM, or the planner
// never uses it (ADR-0107 D2; the read path keeps the model id a literal for this
// reason, see filterExpressions). Until ENG-277 only cmd/embed-backfill created
// it, so a fresh projection-read store searched chunk_embeddings with no ANN index.
// This file is the ONE definition of that DDL: the adapter ensures it at boot and
// cmd/embed-backfill calls the same function.

var projectionIndexSlug = regexp.MustCompile(`[^a-z0-9]+`)

// ProjectionIndexName is the per-model index name: every run of characters
// outside [a-z0-9] becomes "_", so each model's index is its own object. The fold
// is exactly cmd/embed-backfill's original one, so indexes it already built are
// found by name and not duplicated.
func ProjectionIndexName(model string) string {
	return "chunk_embeddings_" + projectionIndexSlug.ReplaceAllString(model, "_") + "_hnsw"
}

// projectionIndexDDL is the CREATE INDEX statement for model at dims. CONCURRENTLY
// so a deployment that adds it to a populated table does not block writes (it
// cannot run inside a transaction; callers Exec it on a pool). The model id is a
// quoted SQL literal — a partial index predicate cannot take a parameter.
func projectionIndexDDL(model string, dims int) string {
	return fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON chunk_embeddings
	 USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
	 WHERE model_id = '%s'`, pgx.Identifier{ProjectionIndexName(model)}.Sanitize(), dims, strings.ReplaceAll(model, "'", "''"))
}

// projectionDB is the slice of pgxpool.Pool the index ensure needs.
type projectionDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnsureProjectionIndex creates model's partial HNSW index on chunk_embeddings if
// it does not exist. An INVALID index (left by a CONCURRENTLY build that failed
// part-way) is dropped and rebuilt: IF NOT EXISTS would otherwise keep it, and the
// planner ignores an invalid index forever. Idempotent; on an existing valid index
// it is two catalog reads.
func EnsureProjectionIndex(ctx context.Context, db projectionDB, model string, dims int) error {
	if model == "" || dims <= 0 {
		return fmt.Errorf("projection index: model and dims are required (model=%q dims=%d)", model, dims)
	}
	name := ProjectionIndexName(model)
	var valid bool
	err := db.QueryRow(ctx, `
		SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1`, name).Scan(&valid)
	switch {
	case err == nil && valid:
		return nil
	case err == nil && !valid:
		slog.Warn("ADR-0107: projection HNSW index is INVALID (an interrupted concurrent build); rebuilding", "index", name)
		if _, derr := db.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+pgx.Identifier{name}.Sanitize()); derr != nil {
			return fmt.Errorf("drop invalid index %s: %w", name, derr)
		}
	case err != pgx.ErrNoRows:
		return fmt.Errorf("projection index %s: catalog read: %w", name, err)
	}
	start := time.Now()
	if _, err := db.Exec(ctx, projectionIndexDDL(model, dims)); err != nil {
		return fmt.Errorf("create index %s: %w", name, err)
	}
	slog.Info("ADR-0107: projection HNSW index ensured", "index", name, "model_id", model, "dims", dims, "took", time.Since(start))
	return nil
}
