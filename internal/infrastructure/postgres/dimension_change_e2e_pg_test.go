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

// ENG-2038 (owner decision 2026-11-18, option C): with the projection on, a
// dimension change still REFUSES to boot while the descriptor tables (tools, skills,
// agent_profiles) keep their embedding columns at the old dimension — they have no
// projection, so their vectors would be dropped and their dense search would fail.
// The refusal names the tables and the way forward and changes nothing; the store
// still boots at its own dimension, and a model change at the SAME dimension still
// coexists.
func TestPg_DimensionChange_ProjectionRefusesWhileDescriptorColumnsAreAtTheOldDimension(t *testing.T) {
	cfgAt := dimScratchDB(t)
	ctx := context.Background()

	first, err := NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("fresh boot at 1024: %v", err)
	}
	vec := make([]float32, 1024)
	vec[0] = 0.1
	if err := first.Save(ctx, &domain.Document{
		ID: "dimchange-doc-2", Text: "a memory kept across a refused model switch",
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
	_, err = NewPgVectorAdapter(ctx, cfg)
	if err == nil {
		t.Fatal("a dimension change must refuse while the descriptor columns are at the old dimension")
	}
	for _, want := range []string{"REFUSING", "tools (1024)", "skills (1024)", "agent_profiles (1024)",
		"embedder.dimensions back to 1024", "new database", "ENG-2038"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must name %q", err, want)
		}
	}

	// Nothing changed: the store boots at its own dimension, rows and schema intact.
	again, err := NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("the store must still boot at its own dimension after the refusal: %v", err)
	}
	if after := readDimState(t, again); after != before {
		t.Fatalf("the refused boot changed the database: before %+v, after %+v", before, after)
	}
	again.Close()

	// A model change at the SAME dimension coexists.
	same := cfgAt(1024)
	same.Embedder.Model = "dimchange-test-model"
	same.Execution.Retrieval.EmbeddingProjectionWrite = true
	same.Execution.Retrieval.EmbeddingProjectionRead = true
	sameDim, err := NewPgVectorAdapter(ctx, same)
	if err != nil {
		t.Fatalf("a same-dimension model change with the projection on must boot: %v", err)
	}
	defer sameDim.Close()
	if got, err := sameDim.GetByID(ctx, "dimchange-doc-2"); err != nil || got == nil {
		t.Fatalf("the row must survive the model switch: %v (%v)", got, err)
	}
}

// The guard reads the DESCRIPTOR columns, not the chunks column: once tools, skills
// and agent_profiles carry untyped embedding columns (no dimension to mismatch), the
// same dimension change is ADR-0107 coexistence and boots with the rows intact.
func TestPg_DimensionChange_ProjectionCoexistsWhenDescriptorColumnsDoNotMismatch(t *testing.T) {
	cfgAt := dimScratchDB(t)
	ctx := context.Background()
	first, err := NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("fresh boot at 1024: %v", err)
	}
	vec := make([]float32, 1024)
	vec[0] = 0.1
	if err := first.Save(ctx, &domain.Document{ID: "dimchange-doc-3", Text: "kept",
		DocumentType: domain.DocTypeMemory, Embedding: domain.Embedding{Vector: vec}, ActivationStrength: 0.1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, table := range descriptorVectorTables {
		rows, err := first.pool.Query(ctx, `SELECT indexname FROM pg_indexes
			WHERE tablename = $1 AND indexdef ILIKE '%(embedding%'`, table)
		if err != nil {
			t.Fatal(err)
		}
		var idx []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			idx = append(idx, n)
		}
		rows.Close()
		for _, n := range idx {
			if _, err := first.pool.Exec(ctx, "DROP INDEX "+pgx.Identifier{n}.Sanitize()); err != nil {
				t.Fatalf("drop %s: %v", n, err)
			}
		}
		if _, err := first.pool.Exec(ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+
			" ALTER COLUMN embedding TYPE vector"); err != nil {
			t.Fatalf("untype %s.embedding: %v", table, err)
		}
	}
	before := readDimState(t, first)
	first.Close()

	cfg := cfgAt(1536)
	cfg.Embedder.Model = "dimchange-test-model"
	cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
	cfg.Execution.Retrieval.EmbeddingProjectionRead = true
	coexist, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("with no descriptor column at another dimension the change is coexistence and must boot: %v", err)
	}
	defer coexist.Close()
	if after := readDimState(t, coexist); after != before {
		t.Fatalf("coexistence changed the legacy store: before %+v, after %+v", before, after)
	}
}

// ENG-2036 end to end, as a model switch at the SAME dimension (since ENG-2038 a
// dimension change refuses to boot): a store with model m1 at 1024 booted with
// model m2 at 1024 and the projection on keeps the chunk lane and the fact lane
// working for m2, and the m1 vectors stay readable for a switch back. The fact lane
// needs no coexistence of its own: memory_facts (0020) was dropped by 0021, and fact
// items carry their vectors in knowledge_item_embeddings (0022), an untyped
// projection keyed by (item, model). The stores are composed as app.go composes
// them: the knowledge store on the adapter's pool, the fact reader at
// embedder.model/dimensions.
func TestPg_ModelSwitch_SameDimension_ChunkAndFactLanes(t *testing.T) {
	cfgAt := dimScratchDB(t)
	ctx := context.Background()
	unit := func(dims, i int) []float32 { v := make([]float32, dims); v[i] = 1; return v }
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	fact := func(value, chunk string) domain.KnowledgeItem {
		day := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
		f := domain.Fact{Subject: "user", Attribute: "pet", Value: value, SubjectKey: "user",
			AttributeKey: domain.NormalizeFactAttribute("pet"), ValueNorm: domain.NormalizeFactText(value),
			ValidFrom: &day, AssertedAt: day, Actor: "alice", SourceChunkID: chunk, Quote: value}
		return f.ToItem("default")
	}
	projected := func(dims int, model string) *config.Config {
		cfg := cfgAt(dims)
		cfg.Embedder.Model = model
		cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
		cfg.Execution.Retrieval.EmbeddingProjectionRead = true
		return cfg
	}
	bypass := &domain.TagPredicate{Bypass: true}

	// Before the switch: model m1 at 1024 writes a chunk and a fact.
	c1 := projected(1024, "m1")
	first, err := NewPgVectorAdapter(ctx, c1)
	if err != nil {
		t.Fatalf("fresh boot at 1024: %v", err)
	}
	if err := first.Save(ctx, &domain.Document{ID: "eng2036-chunk-old", Text: "the user had a cat",
		DocumentType: domain.DocTypeMemory, Embedding: domain.Embedding{Vector: unit(1024, 3)}, ActivationStrength: 0.1}); err != nil {
		t.Fatalf("Save at 1024: %v", err)
	}
	ks1 := NewPgKnowledgeStore(first.Pool(), reg)
	oldFact := fact("cat", "eng2036-chunk-old")
	if _, _, err := ks1.PutItem(ctx, oldFact); err != nil {
		t.Fatalf("PutItem at 1024: %v", err)
	}
	if err := ks1.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: oldFact.ID, ModelID: "m1", Vector: unit(1024, 3)}}); err != nil {
		t.Fatalf("PutItemEmbeddings m1@1024: %v", err)
	}
	first.Close()

	// After the switch: m2 at 1024, coexistence.
	c2 := projected(1024, "m2")
	p, err := NewPgVectorAdapter(ctx, c2)
	if err != nil {
		t.Fatalf("coexistence boot at 1024: %v", err)
	}
	defer p.Close()

	// Chunk lane: a write at 1024 lands in the projection and dense search finds it
	// with the active model's vector hydrated.
	if err := p.SaveBatch(ctx, []*domain.Document{{ID: "eng2036-chunk-new", Text: "the user has a dog",
		DocumentType: domain.DocTypeMemory, Embedding: domain.Embedding{Vector: unit(1024, 5)}, ActivationStrength: 0.1}}); err != nil {
		t.Fatalf("SaveBatch at 1024 under coexistence: %v", err)
	}
	res, err := p.Search(ctx, unit(1024, 5), domain.SearchOptions{TopK: 5, Scope: bypass})
	if err != nil {
		t.Fatalf("chunk Search at 1024: %v", err)
	}
	if len(res) != 1 || res[0].Document.ID != "eng2036-chunk-new" || len(res[0].Document.Embedding.Vector) != 1024 {
		t.Fatalf("chunk search = %+v, want only the m2 chunk with a 1024-dim vector from the projection", res)
	}

	// Fact lane: an m2 fact written at 1024 is found by the reader app.go builds.
	ks2 := NewPgKnowledgeStore(p.Pool(), reg)
	newFact := fact("dog", "eng2036-chunk-new")
	if _, _, err := ks2.PutItem(ctx, newFact); err != nil {
		t.Fatalf("PutItem at 1024: %v", err)
	}
	if err := ks2.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: newFact.ID, ModelID: "m2", Vector: unit(1024, 5)}}); err != nil {
		t.Fatalf("PutItemEmbeddings m2@1024 under coexistence: %v", err)
	}
	r2 := NewPgFactItemReader(ks2, "default", c2.Embedder.Model, c2.Embedder.Dimensions)
	hits, err := r2.SearchFacts(ctx, unit(1024, 5), domain.FactSearchOptions{TopK: 5, Scope: bypass})
	if err != nil {
		t.Fatalf("SearchFacts m2@1024: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != string(newFact.ID) || hits[0].Score < 0.99 {
		t.Fatalf("fact search m2 = %+v, want only the m2 fact", hits)
	}

	// Switch back: the m1 vectors are untouched and still answer at 1024.
	r1 := NewPgFactItemReader(ks2, "default", "m1", 1024)
	hits, err = r1.SearchFacts(ctx, unit(1024, 3), domain.FactSearchOptions{TopK: 5, Scope: bypass})
	if err != nil {
		t.Fatalf("SearchFacts m1@1024: %v", err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != string(oldFact.ID) {
		t.Fatalf("fact search m1 = %+v, want only the m1 fact", hits)
	}
}
