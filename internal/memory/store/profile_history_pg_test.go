package store

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// TestPg_GetPreviousProfile_ThroughTheEnforcingStore proves the listing works
// against the real schema: profiles live in agent_profiles (ADR-0093), which
// QueryByMetadata never reads, so this is the test that would have failed for
// the ticket's first sketch. The store is built exactly as NewMemoryStack builds
// it — raw adapter → EnforcingVectorStore → EnforcingStoreWriter — so the read
// passes the fail-closed scope check (AGENTS.md §2.1/§2.4).
//
// Requires PG_TEST_DSN (handoff §0 recipe: port 5433). Skipped otherwise.
func TestPg_GetPreviousProfile_ThroughTheEnforcingStore(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping Postgres profile-history test")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PG_TEST_DSN: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Database.Host = u.Hostname()
	cfg.Database.Port = u.Port()
	cfg.Database.User = u.User.Username()
	cfg.Database.Password, _ = u.User.Password()
	cfg.Database.DBName = u.Path[1:]
	cfg.Embedder.Dimensions = 1536

	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	defer vec.Close()
	write := authz.NewEnforcingStoreWriter(authz.NewEnforcingVectorStore(vec, slog.Default()), nil, slog.Default())
	ps := NewProfileStore(write)

	agent := "test-prevprofile-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		dctx := domain.WithScope(context.Background(), domain.ScopeSystem)
		for _, h := range []string{"h-old", "h-mid", "h-new"} {
			_ = vec.Delete(dctx, "profile:"+agent+":"+h)
		}
	})
	emb := make([]float32, 1536)
	emb[0] = 1
	for i, h := range []string{"h-old", "h-mid", "h-new"} {
		if err := ps.SaveProfile(ctx, agent, h, emb, domain.AgentProfile{AgentID: agent, SourceHash: h, SuccessRate: float64(i+1) / 10}); err != nil {
			t.Fatalf("SaveProfile %s: %v", h, err)
		}
		time.Sleep(20 * time.Millisecond) // distinct created_at
	}

	got, err := ps.GetPreviousProfile(ctx, agent, "h-new")
	if err != nil {
		t.Fatalf("GetPreviousProfile: %v", err)
	}
	if got == nil || got.SourceHash != "h-mid" {
		t.Fatalf("got %+v, want the h-mid profile (newest non-current)", got)
	}
	if none, err := ps.GetPreviousProfile(ctx, "test-prevprofile-nobody", "h"); err != nil || none != nil {
		t.Fatalf("unknown agent: got %+v err=%v, want nil, nil", none, err)
	}

	// The same nil-vector Search path serves GetJudicialRecords, which failed on
	// every Postgres call until T-20261101-pgvectoradapter-search-with-a-nil-query-vector.
	md := map[string]interface{}{"agent_id": agent, "source_hash": "h-new"}
	if err := ps.Save(ctx, "test critique: cite sources", emb, md); err != nil {
		t.Fatalf("Save judicial record: %v", err)
	}
	t.Cleanup(func() {
		_ = vec.Delete(domain.WithScope(context.Background(), domain.ScopeSystem), judicialRecordID("test critique: cite sources", md))
	})
	recs, err := ps.GetJudicialRecords(ctx, agent, "h-new", 3)
	if err != nil || len(recs) != 1 || recs[0] != "test critique: cite sources" {
		t.Fatalf("GetJudicialRecords = %q err=%v, want the one critique", recs, err)
	}
}
