package chaos_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	"github.com/cambrian-sh/core/internal/testing/chaos"
)

// Per-PR chaos scenarios (`make chaos`). Each one wires a Faulty* decorator into
// the REAL consumer and asserts that consumer's recovery contract; the comment on
// each names the production line it guards, i.e. the line whose removal turns the
// test red. A scenario that would stay green with the recovery code deleted is
// not a chaos test (T-20261031-per-pr-chaos-gate-is-vacuous-the-four-scenarios).

// ============================================================================
// Scenario 1 — Gatekeeper survives a Layer-2 (pgvector interview index) outage.
//
// Guards gatekeeper.go:290-297: the embed error and the InterviewSearcher error
// are each "warn and skip Layer 2", so the Declaration (L1) slate is returned
// instead of an empty slate or an error. The task carries NO
// RequiredCapabilities on purpose: with L1 enforced the "keep the L1 set" rule
// further down would mask a deleted warn-and-skip; with no requirements, an
// error treated as "zero matches" would empty the slate and fail this test.
// ============================================================================

func chaosGatekeeper(t *testing.T, emb domain.Embedder, searcher domain.InterviewSearcher) *gatekeeper.Gatekeeper {
	t.Helper()
	return gatekeeper.NewGatekeeper(newChaosRegistry(10), config.ExecutionConfig{
		Gatekeeper: config.GatekeeperConfig{
			GatekeeperW1:               0.4,
			GatekeeperW2:               0.4,
			GatekeeperW3:               0.2,
			ColdStartPenaltyMultiplier: 0.6,
			GatekeeperMaxCandidates:    10,
		},
	}, gatekeeper.WithEmbedder(emb), gatekeeper.WithSearcher(searcher))
}

func TestChaos_GatekeeperFallsBackOnDbTimeout(t *testing.T) {
	task := func() *domain.DispatchTask {
		return &domain.DispatchTask{Description: "execute a task that needs cognitive reasoning"}
	}

	t.Run("searcher returns DeadlineExceeded", func(t *testing.T) {
		logs := chaos.CaptureLogs(t)
		emb := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20})
		searcher := chaos.NewFaultyInterviewSearcher(nil, chaos.FaultConfig{Error: context.DeadlineExceeded})
		gk := chaosGatekeeper(t, emb, searcher)

		candidates, err := gk.FindCandidates(context.Background(), task())
		if err != nil {
			t.Fatalf("FindCandidates must not error on a Layer-2 outage: %v", err)
		}
		if len(candidates) != 10 {
			t.Fatalf("expected the full L1 slate (10) when Layer 2 fails, got %d", len(candidates))
		}
		if searcher.Calls() != 1 {
			t.Fatalf("Layer 2 was not attempted (searcher calls=%d); the scenario did not reach gatekeeper.go:295", searcher.Calls())
		}
		if logs.Count(slog.LevelWarn, "InterviewSearcher failed") != 1 {
			t.Fatal("the Layer-2 skip must be logged at WARN (gatekeeper.go:297), not silent")
		}
	})

	t.Run("searcher blocks until the caller's deadline", func(t *testing.T) {
		emb := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20})
		// Delay far beyond the deadline: the searcher only returns when ctx expires,
		// which is what a wedged Postgres connection looks like from the Gatekeeper.
		searcher := chaos.NewFaultyInterviewSearcher(nil, chaos.FaultConfig{Delay: 30 * time.Second})
		gk := chaosGatekeeper(t, emb, searcher)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		candidates, err := gk.FindCandidates(ctx, task())
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("FindCandidates must not error when the searcher times out: %v", err)
		}
		if len(candidates) != 10 {
			t.Fatalf("expected the full L1 slate (10) after a searcher timeout, got %d", len(candidates))
		}
		if elapsed > 5*time.Second {
			t.Fatalf("FindCandidates did not honour the deadline: took %s", elapsed)
		}
	})

	t.Run("embedder itself times out", func(t *testing.T) {
		logs := chaos.CaptureLogs(t)
		emb := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{Error: context.DeadlineExceeded})
		searcher := chaos.NewFaultyInterviewSearcher(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20})
		gk := chaosGatekeeper(t, emb, searcher)

		candidates, err := gk.FindCandidates(context.Background(), task())
		if err != nil {
			t.Fatalf("FindCandidates must not error on an embed timeout: %v", err)
		}
		if len(candidates) != 10 {
			t.Fatalf("expected the full L1 slate (10) when the embed fails, got %d", len(candidates))
		}
		if searcher.Calls() != 0 {
			t.Fatal("Layer 2 search must be skipped when the embedding is unavailable")
		}
		if logs.Count(slog.LevelWarn, "embed task description failed") != 1 {
			t.Fatal("the embed failure must be logged at WARN (gatekeeper.go:292)")
		}
	})
}

// ============================================================================
// Scenario 2 — LLM Provider under a failing / rate-limited / stalled endpoint.
//
// The real wiring is used end to end: NewProvider → GeneratorRegistry → the real
// OllamaClient over sharedLLMTransport (the 429/5xx retry ladder) → the
// healthGenerator → CircuitBreaker, with resolveModel as the failover ladder and
// concurrencyGenerator as the global slot. The only test double is the HTTP
// endpoint, whose fault schedule is a FaultyGenerator.
//
// Guards:
//   - transport.go RoundTrip: `isRetryableStatus` retry loop and the
//     `attempt >= llmMaxRateLimitRetries` give-up
//   - circuit_breaker.go recordLocked: `consecutiveFailures >= b.threshold` → open
//   - failover.go resolveModel rung 3: `healthy(defaultID)` gate
//   - circuit_breaker.go Healthy: `Sub(openedAt) >= cooldown` half-open
//   - provider.go concurrencyGenerator.Generate: `defer func() { <-c.sem }()`
//   - ollama_client.go:37-38: TimeoutMs bounds a stalled generation
// ============================================================================

// faultyOllamaServer is an /api/generate endpoint whose outcome per request is
// decided by fg: an error becomes failStatus (with Retry-After: 0 so the
// transport's retry ladder, when it applies, does not sleep); a stall is honoured
// against the request context, so a client timeout releases the handler.
func faultyOllamaServer(t *testing.T, fg *chaos.FaultyGenerator, failStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out, err := fg.Generate(r.Context(), req.Prompt)
		if err != nil {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"`+err.Error()+`"}`, failStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"response": out})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chaosProvider(t *testing.T, endpoint string, threshold, cooldownMs, timeoutMs int) *llm.Provider {
	t.Helper()
	p, err := llm.NewProvider(config.LLMProviderConfig{
		Default: "chaos-ollama",
		Generators: []config.GeneratorConfig{
			{ID: "chaos-ollama", Provider: "ollama", Model: "chaos", Endpoint: endpoint, TimeoutMs: timeoutMs},
		},
		Health:         config.HealthConfig{FailureThreshold: threshold, CooldownMs: cooldownMs},
		MaxConcurrency: 1, // one global slot: a leaked slot blocks the next call
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func acquirePlanner(t *testing.T, p *llm.Provider) domain.Generator {
	t.Helper()
	gen, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposePlanner})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return gen
}

// A transient 5xx burst is absorbed by the transport retry ladder: the caller
// sees ONE successful call and the breaker never learns of it.
func TestChaos_LLMTransientErrorsAbsorbedByRetryLadder(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{FailCount: 2, Error: chaos.ErrInjected})
	srv := faultyOllamaServer(t, fg, http.StatusServiceUnavailable)
	p := chaosProvider(t, srv.URL, 1, 60_000, 5000)

	out, err := acquirePlanner(t, p).Generate(context.Background(), "p")
	if err != nil || out != "ok" {
		t.Fatalf("two transient 503s must be retried transparently, got %q, %v", out, err)
	}
	if fg.Calls() != 3 {
		t.Fatalf("expected 3 endpoint attempts (2 failures + 1 success), got %d", fg.Calls())
	}
	if got := p.BreakerState("chaos-ollama"); got != "closed" {
		t.Fatalf("an absorbed transient must not count against the breaker, got %s", got)
	}
}

// A rate limit that never clears exhausts the ladder: the error surfaces after
// exactly 1 + llmMaxRateLimitRetries attempts and the breaker records it.
func TestChaos_LLMRateLimit_ExhaustsRetries(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{Error: chaos.ErrInjected}) // fails forever
	srv := faultyOllamaServer(t, fg, http.StatusTooManyRequests)
	p := chaosProvider(t, srv.URL, 1, 60_000, 5000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := acquirePlanner(t, p).Generate(ctx, "p")
	if err == nil {
		t.Fatal("a permanent 429 must surface as an error once retries are exhausted")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retries did not give up on their own (hit the test deadline): %v", err)
	}
	const wantAttempts = 6 // 1 + llmMaxRateLimitRetries (transport.go)
	if fg.Calls() != wantAttempts {
		t.Fatalf("expected %d attempts before giving up, got %d", wantAttempts, fg.Calls())
	}
	if got := p.BreakerState("chaos-ollama"); got != "open" {
		t.Fatalf("an exhausted rate limit is a health failure; breaker=%s", got)
	}
}

// Non-transient failures (HTTP 404 "model not found" — not retried by the
// transport) trip the breaker at the threshold; the ladder refuses the open
// generator; failed calls release their concurrency slot; a half-open probe after
// cooldown closes the circuit.
func TestChaos_LLMProviderBreakerOpensAndRecovers(t *testing.T) {
	const (
		threshold  = 2
		cooldownMs = 200
	)
	// Fail the first 3 calls, then succeed: 2 trip the breaker, the 3rd is the
	// slot-release probe on an already-acquired generator, the 4th is the
	// half-open probe that closes the circuit.
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{FailCount: 3, Error: chaos.ErrInjected})
	srv := faultyOllamaServer(t, fg, http.StatusNotFound)
	p := chaosProvider(t, srv.URL, threshold, cooldownMs, 5000)
	req := domain.LLMRequest{Purpose: domain.PurposePlanner}

	gen := acquirePlanner(t, p)

	// Failure 1: below threshold, still closed.
	if _, err := gen.Generate(context.Background(), "p1"); err == nil {
		t.Fatal("expected the first injected failure to surface")
	}
	if got := p.BreakerState("chaos-ollama"); got != "closed" {
		t.Fatalf("after 1 failure (threshold %d) breaker should be closed, got %s", threshold, got)
	}

	// Failure 2: threshold reached → OPEN, and the ladder has no healthy rung.
	if _, err := gen.Generate(context.Background(), "p2"); err == nil {
		t.Fatal("expected the second injected failure to surface")
	}
	if got := p.BreakerState("chaos-ollama"); got != "open" {
		t.Fatalf("after %d consecutive failures breaker must be open, got %s", threshold, got)
	}
	if _, err := p.Acquire(context.Background(), req); !errors.Is(err, llm.ErrNoHealthyModel) {
		t.Fatalf("Acquire with the only generator open must return ErrNoHealthyModel, got %v", err)
	}

	// Slot release: both failed calls must have returned their concurrency slot.
	// With MaxConcurrency=1 a leaked slot makes this call block until ctx expires
	// and never reach the server (fg.Calls stays 2).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = gen.Generate(ctx, "p3")
	cancel()
	if fg.Calls() != 3 {
		t.Fatalf("third call never reached the endpoint (calls=%d): a failed call leaked its concurrency slot", fg.Calls())
	}

	// Cooldown → half-open: Acquire is permitted again, and the probe succeeds
	// (fg has recovered), which closes the breaker.
	var probe domain.Generator
	chaos.WaitUntil(t, 3*time.Second, "half-open Acquire after cooldown", func() bool {
		g, err := p.Acquire(context.Background(), req)
		if err != nil {
			return false
		}
		probe = g
		return true
	})
	out, err := probe.Generate(context.Background(), "p4")
	if err != nil || out != "ok" {
		t.Fatalf("half-open probe should succeed after recovery, got %q, %v", out, err)
	}
	if got := p.BreakerState("chaos-ollama"); got != "closed" {
		t.Fatalf("a successful half-open probe must close the breaker, got %s", got)
	}
	if fg.Calls() != 4 {
		t.Fatalf("expected exactly 4 endpoint calls, got %d", fg.Calls())
	}
}

func TestChaos_LLMProviderStalledGeneratorIsBoundedByTimeout(t *testing.T) {
	// The endpoint stalls far longer than the generator's timeout_ms.
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20, Delay: 30 * time.Second})
	srv := faultyOllamaServer(t, fg, http.StatusInternalServerError)
	p := chaosProvider(t, srv.URL, 1, 60_000, 200)

	start := time.Now()
	_, err := acquirePlanner(t, p).Generate(context.Background(), "slow")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a stalled generation must surface as an error, not an empty success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout_ms=200 was not honoured: Generate took %s (ollama_client.go httpClient.Timeout)", elapsed)
	}
	// The timeout is a health signal: the breaker must see it (ADR-0042 D4).
	if got := p.BreakerState("chaos-ollama"); got != "open" {
		t.Fatalf("a timed-out call must count as a failure (threshold 1), breaker=%s", got)
	}
}

// ============================================================================
// Scenario 3 — IngestionManager on a vector store that fails mid-document.
//
// Guards ingestion_chunks.go `if err := im.agent.Manager.SaveBatch(ctx, docs); err != nil
// { return 0, fmt.Errorf("ingestion manager: save chunk batch: %w", err) }` and
// ingestion_manager.go ProcessSync's `if err != nil { return entityID, err }`:
// the store failure must reach the caller wrapped, not be logged-and-swallowed
// into a "success" with no content.
// ============================================================================

// memStore is the in-memory inner store behind FaultyVectorStore: it records
// what actually landed, so the test can assert on the persisted state.
type memStore struct {
	mu   sync.Mutex
	docs map[string]*domain.Document
}

func newMemStore() *memStore { return &memStore{docs: map[string]*domain.Document{}} }

func (m *memStore) Save(_ context.Context, doc *domain.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[doc.ID] = doc
	return nil
}

func (m *memStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	for _, d := range docs {
		if err := m.Save(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (m *memStore) byType(dt string) []*domain.Document {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*domain.Document
	for _, d := range m.docs {
		if d.DocumentType == dt {
			out = append(out, d)
		}
	}
	return out
}

func (m *memStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (m *memStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.docs[id]; ok {
		return d, nil
	}
	return nil, domain.ErrDocumentNotFound
}
func (m *memStore) GetBatch(context.Context, []string) ([]domain.Document, error) { return nil, nil }
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, id)
	return nil
}
func (m *memStore) DeleteBatch(context.Context, []string) error                      { return nil }
func (m *memStore) IncrementAccess(context.Context, string) error                    { return nil }
func (m *memStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) { return nil, nil }
func (m *memStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

func chaosIngestionManager(t *testing.T, store domain.VectorStore, emb domain.Embedder) *memory.IngestionManager {
	t.Helper()
	agent := memory.NewAgent(memory.NewMemoryManager(store, emb), nil, 0.7, 5, 3, 64, 0, 0, 0)
	return memory.NewIngestionManager(nil, emb, agent, memory.IngestionConfig{}, config.ChunkerConfig{Default: "option_c"})
}

func chaosDocument() domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI:  "chaos://doc-1",
		SourceType: "file_drop",
		Title:      "Chaos document",
		Body:       "The copper listening horn was locked in the bell room.\n\nThe silver hinge map was hidden under the pier.",
		Tags:       []string{"chaos", "source_document", "chaos-doc-1"},
		Timestamp:  time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Importance: 1.0,
	}
}

func TestChaos_IngestionOnFailingVectorStore(t *testing.T) {
	inner := newMemStore()
	// The chunk SaveBatch is the FIRST store call — chunks land before the
	// source-doc entity is minted (T-20261031-processsync-leaves-the-source-doc-entity-stub)
	// — and it fails: a disk-full / connection-reset mid-document.
	fvs := chaos.NewFaultyVectorStore(inner, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrDiskFull})
	emb := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20})
	im := chaosIngestionManager(t, fvs, emb)

	entityID, err := im.ProcessSync(context.Background(), chaosDocument())
	if err == nil {
		t.Fatal("ProcessSync must surface a chunk-save failure, not report success")
	}
	if !errors.Is(err, chaos.ErrDiskFull) {
		t.Fatalf("the store error must be wrapped (%%w) so callers can match it; got %v", err)
	}
	if fvs.Calls() != 1 {
		t.Fatalf("expected SaveBatch(chunks) = 1 store call (the entity is minted only after the chunks land), got %d", fvs.Calls())
	}
	if facts := inner.byType(domain.DocTypeMnemonicFact); len(facts) != 0 {
		t.Fatalf("no chunk may be observable after the batch failed, found %d", len(facts))
	}

	t.Run("no partial document remains", func(t *testing.T) {
		// A source-doc entity without its chunks is a document the caller was
		// told FAILED but that a reader can still find. ProcessSync now mints the
		// entity AFTER the chunks land, so a failed chunk write leaves nothing.
		if entityID != "" {
			t.Fatalf("a failed ingest must not hand back an entity id, got %q", entityID)
		}
		if ents := inner.byType(domain.DocTypeMnemonicEntity); len(ents) != 0 {
			t.Fatalf("the source_doc entity stub must not survive a failed chunk SaveBatch, found %d", len(ents))
		}
	})
}

// ============================================================================
// Scenario 4 — DAGExecutor keeps the step result when the TaskEvent write fails
// or stalls (bbolt disk full / fsync wedge).
//
// Guards dag_executor.go:337-339: `if err := d.EventWriter.WriteTaskEvent(event);
// err != nil { slog.Warn("DAGExecutor: failed to write TaskEvent", …) }` — the
// write is best-effort and must neither propagate to the step result nor be
// swallowed without a WARN line.
// ============================================================================

func chaosStepFn() executer.StepFunc {
	return func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(fmt.Sprintf("r%d", i))}}, nil
	}
}

func TestChaos_DAGExecutorSurvivesEventWriterFailure(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a"}, {Query: "b", DependsOn: []int{0}}}}

	t.Run("writer fails every write", func(t *testing.T) {
		logs := chaos.CaptureLogs(t)
		fw := chaos.NewFaultyTaskEventWriter(nil, chaos.FaultConfig{Error: chaos.ErrDiskFull})
		d := &executer.DAGExecutor{EventWriter: fw}

		got, err := d.Execute(context.Background(), plan, nil, chaosStepFn())
		if err != nil {
			t.Fatalf("a TaskEvent write failure must not fail the plan: %v", err)
		}
		if got["step_0_result"] != "r0" || got["step_1_result"] != "r1" {
			t.Fatalf("step results must survive the write failure, got %v", got)
		}
		if fw.Calls() != 2 {
			t.Fatalf("expected one TaskEvent write per step (2), got %d", fw.Calls())
		}
		if n := logs.Count(slog.LevelWarn, "failed to write TaskEvent"); n != 2 {
			t.Fatalf("each failed write must be surfaced at WARN (dag_executor.go:338); got %d warnings", n)
		}
	})

	t.Run("writer stalls on every write", func(t *testing.T) {
		fw := chaos.NewFaultyTaskEventWriter(nil, chaos.FaultConfig{AfterSuccesses: 1 << 20, Delay: 100 * time.Millisecond})
		d := &executer.DAGExecutor{EventWriter: fw}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		got, err := d.Execute(ctx, plan, nil, chaosStepFn())
		if err != nil {
			t.Fatalf("a slow event writer must not fail the plan: %v", err)
		}
		if got["step_1_result"] != "r1" {
			t.Fatalf("final step result missing, got %v", got)
		}
		if el := time.Since(start); el < 200*time.Millisecond {
			t.Fatalf("the stall was not applied on the write path (took %s)", el)
		}
	})
}

// ============================================================================
// Shared fixtures
// ============================================================================

type chaosRegistry struct {
	agents []domain.AgentDefinition
}

func newChaosRegistry(n int) *chaosRegistry {
	reg := &chaosRegistry{agents: make([]domain.AgentDefinition, n)}
	for i := range n {
		reg.agents[i] = domain.AgentDefinition{
			ID:          fmt.Sprintf("agent-%04d", i),
			SourceHash:  fmt.Sprintf("hash-%04d", i),
			Description: fmt.Sprintf("agent %d performs cognitive reasoning", i),
			Trait:       domain.TraitCognitive,
		}
	}
	return reg
}

func (r *chaosRegistry) GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error) {
	out := make([]domain.AgentDefinition, len(r.agents))
	copy(out, r.agents)
	return out, nil
}

func (r *chaosRegistry) GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error) {
	return &domain.AgentManifest{SupportedFormats: []string{"text", "json"}}, nil
}
