package postgres

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Embedding-dimension failure modes (R9 in the debugging playbook;
// T-20261031-documented-production-failure-modes-have-no). Two guards exist
// after the 5882-chunk wipe incident: a zero embedder dimension refuses to
// boot (NewPgVectorAdapter), and a dimension mismatch on a populated table
// refuses the destructive recreate (ensureSchema). The first two tests need no
// database; the third is PG_TEST_DSN-gated like the other TestPg_* tests.

// closedPort returns a loopback port nothing listens on, so a connection
// attempt fails immediately (ECONNREFUSED) instead of hanging.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return strconv.Itoa(port)
}

func unreachableDBConfig(t *testing.T, dims int) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database.Host = "127.0.0.1"
	cfg.Database.Port = closedPort(t)
	cfg.Database.User = "chaos"
	cfg.Database.Password = "chaos"
	cfg.Database.DBName = "chaos"
	cfg.Storage.AutoMigrate = true
	cfg.Embedder.Dimensions = dims
	return cfg
}

// Guards pgvector_adapter.go NewPgVectorAdapter:
// `if dims == 0 { return nil, fmt.Errorf("embedder dimensions not configured …") }`.
// The refusal must be the dimension error itself, not a downstream connection
// or schema error — a crash-boot with a half-loaded config is exactly when the
// database may also be flapping, and the guard has to win that race.
func TestFailureMode_NewPgVectorAdapter_RefusesZeroEmbedderDimensions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := NewPgVectorAdapter(ctx, unreachableDBConfig(t, 0))
	if err == nil {
		t.Fatal("NewPgVectorAdapter must refuse to boot with embedder.dimensions==0")
	}
	if !strings.Contains(err.Error(), "embedder.dimensions==0") {
		t.Fatalf("the refusal must name the misconfiguration, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refusing") {
		t.Fatalf("the refusal must read as a deliberate refusal, got: %v", err)
	}
}

// Guards pgvector_adapter.go RunMigrations:
// `if dim == 0 { return fmt.Errorf("embedder.dimensions must be set (non-zero) …") }`
// — the `migrate up` CLI path has the same guard and must not even connect.
func TestFailureMode_RunMigrations_RefusesZeroEmbedderDimensions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := RunMigrations(ctx, unreachableDBConfig(t, 0))
	if err == nil {
		t.Fatal("RunMigrations must refuse with embedder.dimensions==0")
	}
	if !strings.Contains(err.Error(), "must be set (non-zero)") {
		t.Fatalf("expected the dimension guard, got: %v", err)
	}
}

// Guards pgvector_adapter.go ensureSchema:
// `if memDocs > 0 && !allowDestructive { return fmt.Errorf("REFUSING to recreate …") }`.
// A populated chunks table at one dimension plus a config at another must
// refuse to boot and leave the rows untouched. The destructive opt-in branch
// (ALLOW_DESTRUCTIVE_DIM_MIGRATION=1) is deliberately NOT exercised: it would
// wipe the shared PG_TEST_DSN database for every other test.
func TestPg_FailureMode_DimensionMismatchRefusesDestructiveRecreate(t *testing.T) {
	adapter, cleanup := newTestAdapter(t) // skips without PG_TEST_DSN
	defer cleanup()
	ctx := context.Background()

	var colDim int
	if err := adapter.pool.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = $1::regclass AND attname = 'embedding'`, TableChunks).Scan(&colDim); err != nil {
		t.Fatalf("probe chunks.embedding dimension: %v", err)
	}
	if colDim <= 0 {
		t.Fatalf("chunks.embedding has no typed dimension (atttypmod=%d); cannot stage a mismatch", colDim)
	}

	const id = "test-dimguard-001"
	vec := make([]float32, colDim)
	vec[0] = 0.1
	if err := adapter.Save(ctx, &domain.Document{
		ID:                 id,
		Text:               "a memory document that a dimension migration would destroy",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          domain.Embedding{Vector: vec},
		ActivationStrength: 0.1,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Cleanup(func() { _, _ = adapter.pool.Exec(ctx, "DELETE FROM "+TableChunks+" WHERE id = $1", id) })

	if os.Getenv("ALLOW_DESTRUCTIVE_DIM_MIGRATION") == "1" {
		t.Skip("ALLOW_DESTRUCTIVE_DIM_MIGRATION=1 is set in this environment; the guard is opted out")
	}

	mismatched := &PgVectorAdapter{pool: adapter.pool, dim: colDim * 2, autoMigrate: true}
	err := mismatched.ensureSchema(ctx)
	if err == nil {
		t.Fatal("ensureSchema must REFUSE a dimension mismatch while memory documents exist")
	}
	if !strings.Contains(err.Error(), "REFUSING to recreate") {
		t.Fatalf("expected the destructive-recreate refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ALLOW_DESTRUCTIVE_DIM_MIGRATION=1") {
		t.Fatalf("the refusal must name the explicit opt-in, got: %v", err)
	}

	// The corpus must be exactly as it was.
	got, err := adapter.GetByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("the refused migration must leave the rows in place, GetByID: %v (%v)", got, err)
	}
	var stillDim int
	if err := adapter.pool.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = $1::regclass AND attname = 'embedding'`, TableChunks).Scan(&stillDim); err != nil || stillDim != colDim {
		t.Fatalf("chunks.embedding dimension changed from %d to %d (err=%v)", colDim, stillDim, err)
	}
}
