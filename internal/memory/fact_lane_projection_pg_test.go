package memory

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// factLaneScratchDB creates an empty database with the vector extension on the
// PG_TEST_DSN server, drops it at the end, and returns a config at dims for it.
func factLaneScratchDB(t *testing.T) func(dims int) *config.Config {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the Postgres-backed fact-lane projection test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PG_TEST_DSN: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("cambrian_test_eng2039_%d", time.Now().UnixNano())
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

// T-20261118-under-adr-0107-projection-read-by-id-reads: the fact lane fetches the
// source chunks of matched facts by id (injectFactHits → GetBatch) and scores each
// injected chunk by its cosine to the query. Under ADR-0107 coexistence that cosine
// must come from the ACTIVE model's vector. Before the fix GetBatch read the legacy
// column, so a chunk written since the switch had no vector and scored cosine 0,
// falling back to the fact-match score.
//
// Real path: PgVectorAdapter booted through NewPgVectorAdapter (1024 m1, then 1536
// m2 with projection write+read), the enforcing vector reader and the enforcing fact
// reader over PgFactItemReader, as kernel.NewMemoryStack composes them.
func TestPg_FactLane_InjectedChunkScoredOnTheActiveModelsVector(t *testing.T) {
	cfgAt := factLaneScratchDB(t)
	ctx := context.Background()
	unit := func(dims, i int) []float32 { v := make([]float32, dims); v[i] = 1; return v }

	first, err := postgres.NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("boot at 1024: %v", err)
	}
	first.Close()

	cfg := cfgAt(1536)
	cfg.Embedder.Model = "m2"
	cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
	cfg.Execution.Retrieval.EmbeddingProjectionRead = true
	p, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("coexistence boot at 1536: %v", err)
	}
	defer p.Close()

	// The chunk the fact was read from; its vector is the query's direction.
	const chunkID = "eng2039-fact-source"
	if err := p.SaveBatch(ctx, []*domain.Document{{ID: chunkID, Text: "the user has a dog",
		DocumentType: domain.DocTypeMnemonicFact, ActivationStrength: 0.5,
		Embedding: domain.Embedding{Vector: unit(1536, 5)}}}); err != nil {
		t.Fatalf("SaveBatch at 1536: %v", err)
	}
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := postgres.NewPgKnowledgeStore(p.Pool(), reg)
	day := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	f := domain.Fact{Subject: "user", Attribute: "pet", Value: "dog", SubjectKey: "user",
		AttributeKey: domain.NormalizeFactAttribute("pet"), ValueNorm: domain.NormalizeFactText("dog"),
		ValidFrom: &day, AssertedAt: day, Actor: "alice", SourceChunkID: chunkID, Quote: "dog"}
	item := f.ToItem("default")
	if _, _, err := ks.PutItem(ctx, item); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	// The fact's own vector is orthogonal to the query, so its fact-match score is
	// far from 1: an injected chunk scored 1.0 can only have come from the chunk's
	// active-model vector.
	factVec := unit(1536, 5)
	factVec[6] = 1
	if err := ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: item.ID, ModelID: "m2", Vector: factVec}}); err != nil {
		t.Fatalf("PutItemEmbeddings: %v", err)
	}

	facts := authz.NewEnforcingFactItemReader(postgres.NewPgFactItemReader(ks, "default", "m2", 1536), nil)
	q := NewQueryServiceWithFacts(nil, authz.NewEnforcingVectorStore(p, nil), domain.AllowAllAuthorizer{}, facts)
	q.SetRankingV2(RankingV2{FactLane: true})

	bypass := &domain.TagPredicate{Bypass: true}
	sctx := domain.WithScope(ctx, bypass)
	results, hits := q.injectFactHits(sctx, nil, unit(1536, 5), bypass, domain.DocTypeMnemonicFact)
	if len(hits) == 0 {
		t.Fatal("the fact lane found no fact")
	}
	var got *domain.SearchResult
	for i := range results {
		if results[i].Document.ID == chunkID {
			got = &results[i]
		}
	}
	if got == nil {
		t.Fatalf("source chunk not injected: %+v", results)
	}
	if n := len(got.Document.Embedding.Vector); n != 1536 {
		t.Fatalf("injected chunk embedding = %d dims, want the active model's 1536", n)
	}
	if math.Abs(got.RawScore-1) > 1e-6 || math.Abs(got.Score-1) > 1e-6 {
		t.Fatalf("injected chunk scored raw %.4f / score %.4f, want cosine 1 on the active model's vector (fact match %.4f)",
			got.RawScore, got.Score, hits.match(chunkID))
	}
}
