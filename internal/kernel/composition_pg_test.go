package kernel

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/fakemodels"
)

// usageModel answers with fixed provider usage on a trailing chunk.
type usageModel struct{ prompt, compl int }

func (u usageModel) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: "x", IsFinal: true}
	ch <- domain.StreamChunk{IsFinal: true, UsagePromptTokens: u.prompt, UsageCompletionTokens: u.compl,
		UsageTotalTokens: u.prompt + u.compl}
	close(ch)
	return ch, nil
}

// pgTestConfig points a default config at PG_TEST_DSN (skips without it). Use a
// scratch database of your own: the adapter auto-migrates it.
func pgTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the Postgres-backed composition test")
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
	return cfg
}

// ENG-1504 end to end: the composition the boot path performs (app/app.go) — a real
// Postgres adapter bound into a MemoryBackend, the memory stack built from it, the
// Postgres session store passed to the server as its run and checkpoint stores, and
// one spend meter handed to the gateway's constructor and to ProvideServer — yields
// every capability with no runtime probe, and the session ceiling is enforced on an
// agent stream through the composed gateway.
func TestPg_Composition_EveryCapabilityTypedAndSessionCeilingEnforced(t *testing.T) {
	cfg := pgTestConfig(t)
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)

	backend := NewMemoryBackend(vec)
	if m := backend.Missing(); len(m) != 0 {
		t.Fatalf("production backend lacks %v", m)
	}
	mem := NewMemoryStack(backend, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	if mem.Agent.ExperienceStore == nil {
		t.Error("Experience capability did not reach the memory agent")
	}
	if mem.ChunkTripletsBatcher == nil {
		t.Error("Graph/ChunkTriplets capability did not reach the stack (no triplet batcher)")
	}
	if mem.DocumentStore == nil {
		t.Error("Documents capability did not yield the ADR-0093 enforcing document store")
	}
	// The lexical half of hybrid retrieval runs against the real schema.
	if _, err := backend.Lexical.LexicalSearch(domain.WithScope(ctx, domain.ScopeSystem), "eng1504 probe",
		domain.SearchOptions{TopK: 3, Scope: domain.ScopeSystem}); err != nil {
		t.Errorf("LexicalSearch on the typed backend: %v", err)
	}

	sessions, err := postgres.NewPgSessionStore(ctx, vec.Pool())
	if err != nil {
		t.Fatalf("NewPgSessionStore: %v", err)
	}
	provider, err := llm.NewProvider(config.LLMProviderConfig{
		Default:    "g",
		Generators: []config.GeneratorConfig{{ID: "g", Provider: "openai", Model: "m", Endpoint: "http://127.0.0.1:1", TimeoutMs: 1000}},
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	exec := cfg.Execution
	exec.Session.MaxSessionTokens = 100
	meter := llm.NewSpendMeter(exec.Session.MaxSessionTokens, provider.Ledger(), nil)
	gw := subnetwork.NewLLMGateway(exec, meter)
	gw.SetStepModels(fakemodels.New().Set("llm:fake", usageModel{prompt: 120, compl: 30}))
	gw.SetDefaultModelID("llm:fake")

	var runs domain.RunStore = sessions
	var checkpoints executer.CheckpointStore = sessions
	srv := ProvideServer(exec, mem, &AwarenessStack{}, &MetabolismStack{}, nil, provider, nil, gw, nil, nil, nil, nil, nil,
		runs, checkpoints, meter)
	if srv.Runs != runs || srv.Checkpoints != checkpoints {
		t.Fatalf("server run/checkpoint stores = %T/%T, want the Postgres session store passed in", srv.Runs, srv.Checkpoints)
	}
	if srv.SpendMeter != meter || gw.SpendMeter() != meter {
		t.Fatal("the server and the gateway must share the one composed spend meter")
	}

	// ADR-0131: the first agent stream books 150 tokens against a 100-token ceiling;
	// the next call in that session is refused.
	sctx := domain.WithSessionID(ctx, domain.SessionID("eng1504-ceiling"))
	stream := func() error {
		lease, err := gw.Acquire(sctx, domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			return err
		}
		defer func() { _, _ = gw.Complete(sctx, lease) }()
		out := make(chan domain.StreamChunk, 8)
		return gw.StreamChunks(sctx, lease, domain.UserPrompt("p"), out)
	}
	if err := stream(); err != nil {
		t.Fatalf("first stream under the ceiling: %v", err)
	}
	if err := stream(); !errors.Is(err, llm.ErrSpendCeiling) {
		t.Fatalf("second stream over the session ceiling: got %v, want ErrSpendCeiling", err)
	}
}
