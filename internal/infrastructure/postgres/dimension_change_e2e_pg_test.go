package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2002 end to end, through the production boot path (NewPgVectorAdapter) on a
// scratch database of its own: a store migrated at one embedder dimension and then
// booted at another, without the ADR-0107 projection, must REFUSE to boot and leave
// the database exactly as it was — rows, schema_migrations and the column dimension —
// whether or not it holds memory documents. The retired recreate path dropped the
// corpus tables and schema_migrations and then failed re-applying 0001 on a schema
// past 0007, leaving the database unmigratable for every later boot. With the
// projection on, the same change is ADR-0107 coexistence and boots.

// dimScratchDB creates an empty database (with the vector extension, which
// NewPgVectorAdapter needs to connect) on the PG_TEST_DSN server and drops it at the
// end. It returns a config pointing at it with the embedder at dims.
func dimScratchDB(t *testing.T) func(dims int) *config.Config {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the Postgres-backed dimension-change test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PG_TEST_DSN: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("cambrian_test_dimchange_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PG_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	ext, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect scratch database: %v", err)
	}
	defer ext.Close(ctx)
	if _, err := ext.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	return func(dims int) *config.Config {
		cfg := config.DefaultConfig()
		cfg.Database.Host = u.Hostname()
		cfg.Database.Port = u.Port()
		cfg.Database.User = u.User.Username()
		cfg.Database.Password, _ = u.User.Password()
		cfg.Database.DBName = name
		cfg.Storage.AutoMigrate = true
		cfg.Embedder.Dimensions = dims
		return cfg
	}
}

type dimState struct {
	migrations, chunks, colDim int
}

func readDimState(t *testing.T, p *PgVectorAdapter) dimState {
	t.Helper()
	ctx := context.Background()
	var s dimState
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&s.migrations); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM `+TableChunks).Scan(&s.chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT atttypmod FROM pg_attribute
		WHERE attrelid = $1::regclass AND attname = 'embedding'`, TableChunks).Scan(&s.colDim); err != nil {
		t.Fatalf("probe chunks.embedding dimension: %v", err)
	}
	return s
}

func TestPg_DimensionChange_RefusesWithoutProjectionAndLeavesDatabaseIntact(t *testing.T) {
	for _, withDocs := range []bool{false, true} {
		t.Run(fmt.Sprintf("docs=%v", withDocs), func(t *testing.T) {
			// The retired opt-in must not bring the recreate back.
			t.Setenv("ALLOW_DESTRUCTIVE_DIM_MIGRATION", "1")
			cfgAt := dimScratchDB(t)
			ctx := context.Background()

			first, err := NewPgVectorAdapter(ctx, cfgAt(1024))
			if err != nil {
				t.Fatalf("fresh boot at 1024: %v", err)
			}
			if withDocs {
				vec := make([]float32, 1024)
				vec[0] = 0.1
				if err := first.Save(ctx, &domain.Document{
					ID: "dimchange-doc-1", Text: "a memory a dimension change must not destroy",
					DocumentType: domain.DocTypeMemory, Embedding: domain.Embedding{Vector: vec},
					ActivationStrength: 0.1,
				}); err != nil {
					t.Fatalf("Save: %v", err)
				}
			}
			before := readDimState(t, first)
			first.Close()
			if before.colDim != 1024 || before.migrations == 0 {
				t.Fatalf("fresh store = %+v, want a migrated store at 1024", before)
			}

			_, err = NewPgVectorAdapter(ctx, cfgAt(1536))
			if err == nil {
				t.Fatal("booting a 1024 store at 1536 without the projection must refuse")
			}
			for _, want := range []string{"REFUSING", "embedding_projection", "1024", "1536"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q must name %q", err, want)
				}
			}

			again, err := NewPgVectorAdapter(ctx, cfgAt(1024))
			if err != nil {
				t.Fatalf("the store must still boot at its own dimension after the refusal: %v", err)
			}
			defer again.Close()
			if after := readDimState(t, again); after != before {
				t.Fatalf("the refused boot changed the database: before %+v, after %+v", before, after)
			}
		})
	}
}

func TestPg_DimensionChange_ProjectionCoexistenceBootsAndKeepsRows(t *testing.T) {
	cfgAt := dimScratchDB(t)
	ctx := context.Background()

	first, err := NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("fresh boot at 1024: %v", err)
	}
	vec := make([]float32, 1024)
	vec[0] = 0.1
	if err := first.Save(ctx, &domain.Document{
		ID: "dimchange-doc-2", Text: "a memory kept across a model switch",
		DocumentType: domain.DocTypeMemory, Embedding: domain.Embedding{Vector: vec}, ActivationStrength: 0.1,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before := readDimState(t, first)
	first.Close()

	cfg := cfgAt(1536)
	cfg.Embedder.Model = "dimchange-test-model"
	cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
	cfg.Execution.Retrieval.EmbeddingProjectionRead = true
	coexist, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("a dimension change with the projection on is ADR-0107 coexistence and must boot: %v", err)
	}
	defer coexist.Close()
	if after := readDimState(t, coexist); after != before {
		t.Fatalf("coexistence changed the legacy store: before %+v, after %+v", before, after)
	}
	if got, err := coexist.GetByID(ctx, "dimchange-doc-2"); err != nil || got == nil {
		t.Fatalf("the row must survive the model switch: %v (%v)", got, err)
	}
}
