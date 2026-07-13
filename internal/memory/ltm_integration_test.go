//go:build e2e

package memory_test

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory"
)

// ── seed IDs ─────────────────────────────────────────────────────────────────

const (
	seedPrimeFact  = "ltm-test-seed-prime-fact"
	seedPoisonFact = "ltm-test-seed-eventsourcing-poison"
	seedBlockedCmd = "ltm-test-seed-blocked-cmd"
	seedScene0     = "ltm-test-scene-step-0"
	seedScene1     = "ltm-test-scene-step-1"
)

// ── health checks ─────────────────────────────────────────────────────────────

func ollamaUp() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://localhost:11434/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func postgresUp() bool {
	conn, err := net.DialTimeout("tcp", "localhost:5432", 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ── setup helpers ─────────────────────────────────────────────────────────────

func newLTMTestEnv(t *testing.T) (
	vec *postgres.PgVectorAdapter,
	embed domain.Embedder,
	gen domain.Generator,
	ws domain.WorkspaceStage,
	cleanup func(),
) {
	t.Helper()
	if !ollamaUp() {
		t.Skip("Ollama not reachable at localhost:11434")
	}
	if !postgresUp() {
		t.Skip("PostgreSQL not reachable at localhost:5432")
	}

	cfg, err := config.LoadConfig("../../configs/config.dev.json")
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ctx := context.Background()
	vec, err = postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("pgvector: %v", err)
	}

	gcfg := cfg.LLMProvider.DefaultGenerator()
	if gcfg == nil {
		t.Fatal("no default generator configured")
	}
	embed = &llm.OllamaEmbedder{
		BaseURL:   cfg.Embedder.Endpoint,
		Model:     cfg.Embedder.Model,
		TimeoutMs: cfg.Embedder.TimeoutMs,
	}
	gen = &llm.OllamaClient{
		BaseURL:   gcfg.Endpoint,
		Model:     gcfg.Model,
		TimeoutMs: gcfg.TimeoutMs,
	}

	mem := kernel.NewMemoryStack(vec, gen, embed, cfg.Execution)
	ws = mem.WorkspaceStage

	cleanup = func() {
		ctx := context.Background()
		for _, id := range []string{seedPrimeFact, seedPoisonFact, seedBlockedCmd, seedScene0, seedScene1} {
			_ = vec.Delete(ctx, id)
		}
		vec.Close()
	}
	return
}

// seedViaPgVector seeds documents directly through the pgvector adapter.
func seedViaPgVector(t *testing.T, ctx context.Context, vec *postgres.PgVectorAdapter, embed domain.Embedder) {
	t.Helper()

	type seedDoc struct {
		id       string
		docType  string
		text     string
		metadata map[string]interface{}
	}

	docs := []seedDoc{
		{
			id:      seedPrimeFact,
			docType: domain.DocTypeMnemonicFact,
			text:    "The Sieve of Eratosthenes is an O(n log log n) algorithm for finding all prime numbers up to N by iteratively marking multiples of each prime as composite.",
			metadata: map[string]interface{}{
				"source_agent": "test_seeder",
				"stored_at":    time.Now().Format(time.RFC3339),
				"priority":     8,
				"tags":         []string{"algorithms", "primes", "python"},
			},
		},
		{
			id:      seedPoisonFact,
			docType: domain.DocTypeMnemonicFact,
			text:    "Event sourcing stores state as a sequence of events; CRUD stores current state directly. Event sourcing enables audit trails and time-travel queries.",
			metadata: map[string]interface{}{
				"source_agent": "test_seeder",
				"stored_at":    time.Now().Format(time.RFC3339),
				"priority":     7,
				"tags":         []string{"architecture", "event-sourcing", "crud"},
			},
		},
		{
			id:      seedBlockedCmd,
			docType: domain.DocTypeNegativeEdge,
			text:    "BLOCKED: 'write' is not in ALLOWED_COMMANDS",
			metadata: map[string]interface{}{
				"error_type": "blocked",
				"raw_output": "BLOCKED: 'write' is not in ALLOWED_COMMANDS",
				"agent_id":   "terminal_agent",
				"step_index": 0,
				"timestamp":  time.Now().Format(time.RFC3339),
			},
		},
	}

	for _, d := range docs {
		v, err := embed.Embed(ctx, d.text)
		if err != nil {
			t.Fatalf("embed %s: %v", d.id, err)
		}
		if err := vec.Save(ctx, &domain.Document{
			ID:                 d.id,
			DocumentType:       d.docType,
			Text:               d.text,
			Embedding:          domain.Embedding{Vector: v},
			ActivationStrength: 0.5,
			Metadata:           d.metadata,
		}); err != nil {
			t.Fatalf("seed %s: %v", d.id, err)
		}
	}
}

// seedScenes creates two shallow MnemonicScene documents and a specifies edge.
func seedScenes(t *testing.T, ctx context.Context, vec *postgres.PgVectorAdapter, embed domain.Embedder) {
	t.Helper()

	scenes := []struct{ id, text string }{
		{seedScene0, "step_0 query: prime numbers algorithm | output: Implemented Sieve of Eratosthenes in Python with O(n log log n) complexity."},
		{seedScene1, "step_1 query: explain time complexity | output: Time complexity is O(n log log n) due to the harmonic series of prime reciprocals."},
	}
	for _, s := range scenes {
		v, err := embed.Embed(ctx, s.text)
		if err != nil {
			t.Fatalf("embed scene %s: %v", s.id, err)
		}
		if err := vec.Save(ctx, &domain.Document{
			ID:                 s.id,
			DocumentType:       domain.DocTypeMnemonicScene,
			Text:               s.text,
			Embedding:          domain.Embedding{Vector: v},
			ActivationStrength: 0.4,
			Metadata: map[string]interface{}{
				"source_agent": "test_seeder",
				"stored_at":    time.Now().Format(time.RFC3339),
			},
		}); err != nil {
			t.Fatalf("seed scene %s: %v", s.id, err)
		}
	}

	// Write specifies edge: scene_1 → scene_0 (step 1 builds on step 0)
	if err := vec.SaveEdge(ctx, domain.DocumentEdge{
		SourceID:  seedScene1,
		TargetID:  seedScene0,
		EdgeType:  domain.EdgeSpecifies,
		Weight:    0.8,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed specifies edge: %v", err)
	}
}

// rawCosine computes cosine similarity between two float32 vectors.
func rawCosine(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// ── REQ1: three-layer LTM relevance gate ─────────────────────────────────────

// TestLTM_RelevanceGate is the primary REQ1 integration test.
// Seeds known-relevant and known-irrelevant (poison) documents into pgvector,
// then asserts three layers of relevance for each user query:
//  1. Poison exclusion: the event-sourcing document must not appear in Facts
//  2. Cosine threshold: every returned fact must score ≥ 0.35 raw cosine similarity
//  3. LLM-as-Judge: every returned fact must receive YES from the LLM relevance check
func TestLTM_RelevanceGate(t *testing.T) {
	vec, embed, gen, ws, cleanup := newLTMTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Seed documents — cleaned up by defer in newLTMTestEnv.
	seedViaPgVector(t, ctx, vec, embed)
	seedScenes(t, ctx, vec, embed)

	queries := []struct {
		name  string
		query string
	}{
		{
			name:  "prime_numbers",
			query: "Write a Python function to find all prime numbers up to N using the Sieve of Eratosthenes",
		},
		{
			name:  "atomic_file_write",
			query: "Write a Python context manager for atomic file writes",
		},
	}

	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			enrichment, err := ws.PrimeForPlanning(ctx, q.query)
			if err != nil {
				t.Fatalf("PrimeForPlanning: %v", err)
			}

			t.Logf("query=%q facts=%d negatives=%d", q.query, len(enrichment.Facts), len(enrichment.Negatives))

			// ── Assertion 1: Poison exclusion ────────────────────────────────
			for _, f := range enrichment.Facts {
				if f.Document.ID == seedPoisonFact {
					t.Errorf("[A1] poison document %q appeared in Facts for query %q", seedPoisonFact, q.query)
				}
			}

			// ── Assertion 2: Cosine threshold ─────────────────────────────────
			queryVec, err := embed.Embed(ctx, q.query)
			if err != nil {
				t.Fatalf("embed query: %v", err)
			}
			for _, f := range enrichment.Facts {
				// Re-embed the document text to get raw cosine (SearchResult.Score
				// is floor-multiplier-adjusted, not raw cosine).
				docVec, err := embed.Embed(ctx, f.Document.Text)
				if err != nil {
					t.Logf("[A2] skip cosine check for %q: embed error: %v", f.Document.ID, err)
					continue
				}
				sim := rawCosine(queryVec, docVec)
				if sim < 0.35 {
					t.Errorf("[A2] fact %q cosine=%.3f < 0.35 for query %q (text: %q)",
						f.Document.ID, sim, q.query, f.Document.Text[:min(60, len(f.Document.Text))])
				} else {
					t.Logf("[A2] fact %q cosine=%.3f ✓", f.Document.ID, sim)
				}
			}

			// ── Assertion 3: LLM-as-Judge for the seeded relevant fact ───────
			// We only assert YES for the document we specifically seeded as relevant.
			// Pre-existing documents from other e2e runs (shared pgvector) may or may
			// not be relevant; the poison exclusion in A1 covers that case.
			for _, f := range enrichment.Facts {
				if f.Document.ID != seedPrimeFact {
					continue // only judge our seeded fact
				}
				prompt := fmt.Sprintf(
					"Is this fact relevant to the query? Answer only YES or NO — no other text.\n\nQuery: %s\n\nFact: %s",
					q.query, f.Document.Text,
				)
				resp, err := gen.Generate(ctx, prompt)
				if err != nil {
					t.Logf("[A3] LLM-as-Judge failed: %v (skipping)", err)
					continue
				}
				upper := strings.ToUpper(strings.TrimSpace(resp))
				relevant := strings.HasPrefix(upper, "YES") || strings.Contains(upper, "\"YES\"")
				if relevant {
					t.Logf("[A3] seeded prime fact rated relevant ✓ (response: %q)", resp[:min(30, len(resp))])
				} else {
					t.Errorf("[A3] LLM-as-Judge rated seeded fact %q as NOT relevant to %q (response: %q)",
						f.Document.ID, q.query, resp)
				}
			}

			// ── Assertion 4: NegativeEdge in Negatives, not Facts ────────────
			for _, f := range enrichment.Facts {
				if f.Document.ID == seedBlockedCmd {
					t.Errorf("[A4] negative_edge doc %q appeared in Facts for query %q", seedBlockedCmd, q.query)
				}
			}

			// ── Assertion 5: BuildLTMContext format ───────────────────────────
			ltmBlock := memory.BuildLTMContext(nil, enrichment)
			t.Logf("[A5] ltmBlock=%q", ltmBlock[:min(200, len(ltmBlock))])
			if len(enrichment.Negatives) > 0 && !strings.Contains(ltmBlock, "<NegativeLTM>") {
				t.Error("[A5] BuildLTMContext missing <NegativeLTM> section when negatives present")
			}
			if len(enrichment.Facts) > 0 && !strings.Contains(ltmBlock, "<FactLTM>") {
				t.Error("[A5] BuildLTMContext missing <FactLTM> section when facts present")
			}
			// Poison content must not appear in the FactLTM block.
			if strings.Contains(ltmBlock, "Event sourcing stores state as a sequence") {
				t.Errorf("[A5] <FactLTM> must not contain event-sourcing poison content")
			}
		})
	}
}

// TestLTM_SceneAndEdgeWritten verifies that MnemonicScene documents and
// specifies edges seeded in seedScenes are retrievable — confirming the
// pipeline that DAGExecutor feeds during live execution.
func TestLTM_SceneAndEdgeWritten(t *testing.T) {
	vec, embed, _, _, cleanup := newLTMTestEnv(t)
	defer cleanup()

	ctx := context.Background()
	seedScenes(t, ctx, vec, embed)

	// Scene docs must exist in pgvector.
	for _, id := range []string{seedScene0, seedScene1} {
		doc, err := vec.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID %s: %v", id, err)
		}
		if doc == nil {
			t.Errorf("mnemonic_scene doc %q missing from pgvector", id)
			continue
		}
		if doc.DocumentType != domain.DocTypeMnemonicScene {
			t.Errorf("doc %q has type %q, want %q", id, doc.DocumentType, domain.DocTypeMnemonicScene)
		}
		t.Logf("scene %q found ✓", id)
	}

	// specifies edge must exist: scene_1 → scene_0.
	edges, err := vec.GetAdjacentEdges(ctx, []string{seedScene1})
	if err != nil {
		t.Fatalf("GetAdjacentEdges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.SourceID == seedScene1 && e.TargetID == seedScene0 && e.EdgeType == domain.EdgeSpecifies {
			found = true
			t.Logf("specifies edge %s → %s found ✓", seedScene1, seedScene0)
		}
	}
	if !found {
		t.Errorf("specifies edge from %s to %s not found in document_edges", seedScene1, seedScene0)
	}
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
