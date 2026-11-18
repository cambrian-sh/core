package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-1320 end to end: the recall lanes are composed by kernel.NewMemoryStack and
// kernel.ComposeLateRetrieval on the real boot path (bootstrapKernel over a scratch
// Postgres database and a stand-in embedder). With every lane switched on, the booted
// kernel's QueryService holds every lane, the late lanes hold dispatchers over the
// metabolism stack's agent transport, the plugin decision observer is the one from
// Options.

type recordingObserver struct {
	mu sync.Mutex
	n  int
}

func (o *recordingObserver) ObserveRetrieval(domain.RetrievalDecision) {
	o.mu.Lock()
	o.n++
	o.mu.Unlock()
}

func (o *recordingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// fakeOllamaEmbedder answers /api/embeddings with a fixed unit vector of dims.
func fakeOllamaEmbedder(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := make([]float32, dims)
		v[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": v})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func retrievalScratchConfig(t *testing.T) *config.Config {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the full-boot retrieval wiring test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PG_TEST_DSN: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("cambrian_test_eng1320_%d", time.Now().UnixNano())
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
	if _, err := ext.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	ext.Close(ctx)

	cfg := config.DefaultConfig()
	cfg.Database.Host = u.Hostname()
	cfg.Database.Port = u.Port()
	cfg.Database.User = u.User.Username()
	cfg.Database.Password, _ = u.User.Password()
	cfg.Database.DBName = name
	cfg.Storage.AutoMigrate = true
	cfg.Storage.DataDir = t.TempDir()
	cfg.Storage.DBName = "eng1320.db"
	// No sandbox Python env download from a unit test (ADR-0157 D7).
	cfg.Execution.Agents.Python.BuildOnBoot = false
	emb := fakeOllamaEmbedder(t, cfg.Embedder.Dimensions)
	cfg.Embedder.Endpoint = emb.URL

	r := &cfg.Execution.Retrieval
	r.KG2RAGEnabled, r.QueryEntitySeedingEnabled, r.AnchorConstraintEnabled = true, true, true
	r.BlendEnabled, r.StageACalibrated, r.TemporalIntentEnabled = true, true, true
	r.HybridSearchEnabled, r.NeighborWindowEnabled, r.ChronologicalContextEnabled = true, true, true
	r.DocumentCompletionEnabled, r.DocumentCompletionDocuments, r.DocumentCompletionMaxChunks = true, 2, 8
	r.StructureGraphEnabled = true
	r.RerankerEnabled, r.AgenticRetrievalEnabled, r.AgenticDecomposeEnabled = true, true, true
	return cfg
}

// field reads one unexported QueryService field for the assertions below.
func field(qs any, name string) reflect.Value {
	return reflect.ValueOf(qs).Elem().FieldByName(name)
}

// fieldAny returns an unexported interface field's dynamic value (test-only read).
func fieldAny(qs any, name string) any {
	f := field(qs, name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
}

func TestPg_BootComposesEveryRetrievalLane(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	obs := &recordingObserver{}
	k, err := bootstrapKernel(ctx, cfg, lis, Options{DecisionObserver: obs}, nil)
	if err != nil {
		t.Fatalf("bootstrapKernel: %v", err)
	}
	// The real Shutdown on a kernel that was bootstrapped but never started: it
	// used to block forever in ChunkTripletsBatcher.Stop
	// (T-20261118-kernel-shutdown-hangs-forever-when-the-kernel), so a hang fails
	// the test instead of the run's timeout.
	defer shutdownWithin(t, k, 30*time.Second)

	qs := k.Memory.QueryService
	for _, name := range []string{"chunkTriplets", "rankSignals", "lexical", "sectionStore", "reranker", "planner", "decisionObs"} {
		if field(qs, name).IsNil() {
			t.Errorf("lane %s is not wired on the booted kernel", name)
		}
	}
	for _, name := range []string{"queryEntitySeed", "anchorConstraint", "neighborWindow", "chronoContext", "agenticEnabled", "decompEnabled"} {
		if !field(qs, name).Bool() {
			t.Errorf("lane %s is off on the booted kernel", name)
		}
	}
	if qs.CurrentBlendWeights() == (memory.BlendWeights{}) {
		t.Error("the Stage-A blend is off on the booted kernel")
	}
	if got, ok := fieldAny(qs, "decisionObs").(*recordingObserver); !ok || got != obs {
		t.Errorf("decision observer = %#v, want the one from Options", fieldAny(qs, "decisionObs"))
	}
	if field(qs, "docCompletion").FieldByName("enabled").Bool() != true {
		t.Error("document completion is off on the booted kernel")
	}
	if !field(qs, "rankingV2").FieldByName("Calibrated").Bool() || !field(qs, "rankingV2").FieldByName("TemporalIntent").Bool() {
		t.Error("ranking v2 switches did not reach the booted kernel")
	}
	// The late lanes: dispatchers over the metabolism stack's agent transport.
	rr, ok := fieldAny(qs, "reranker").(*subnetwork.RerankerDispatcher)
	if !ok || rr.Caller != k.Metabolism.AgentTransport || rr.AgentID != "reranker_agent" {
		t.Errorf("reranker = %#v, want a RerankerDispatcher over the kernel's agent transport", fieldAny(qs, "reranker"))
	}
	pl, ok := fieldAny(qs, "planner").(*subnetwork.RetrievalDispatcher)
	if !ok || pl.Caller != k.Metabolism.AgentTransport || pl.AgentID != "retrieval_agent" {
		t.Errorf("planner = %#v, want a RetrievalDispatcher over the kernel's agent transport", fieldAny(qs, "planner"))
	}
}

// shutdownWithin runs the kernel's real Shutdown and fails the test when it does
// not return within d.
func shutdownWithin(t *testing.T, k *Kernel, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.Shutdown(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Kernel.Shutdown did not return within %s", d)
	}
}
