package app

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2068 end to end (AGENTS.md §2.6): the receipt QueryID a booted kernel
// emits is derived from the request, never from a clock. Through the real boot
// path (bootstrapKernel over a scratch Postgres database, the plugin decision
// observer from Options — where premium's receipts consumer sits), the same
// recall asked twice (a retry, or the same request after a restart) emits the
// same QueryID; a different session or question emits another; a seeded
// boundary handle (the MCP endpoint's) is still the prefix.

type queryIDObserver struct {
	mu  sync.Mutex
	ids []string
}

func (o *queryIDObserver) ObserveRetrieval(d domain.RetrievalDecision) {
	o.mu.Lock()
	o.ids = append(o.ids, d.QueryID)
	o.mu.Unlock()
}

func (o *queryIDObserver) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.ids
	o.ids = nil
	return out
}

func TestPg_E2E_ReceiptQueryIDIsDerivedFromTheRequest(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	// The single pass is enough here and needs no retrieval/reranker agent.
	r := &cfg.Execution.Retrieval
	r.RerankerEnabled, r.AgenticRetrievalEnabled, r.AgenticDecomposeEnabled = false, false, false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	obs := &queryIDObserver{}
	k, err := bootstrapKernel(ctx, cfg, lis, Options{DecisionObserver: obs}, nil)
	if err != nil {
		t.Fatalf("bootstrapKernel: %v", err)
	}
	defer shutdownWithin(t, k, 30*time.Second)
	qs := k.Memory.QueryService

	recall := func(ctx context.Context, query string) string {
		t.Helper()
		if _, err := qs.Search(ctx, query, "agent:e2e"); err != nil {
			t.Fatalf("Search: %v", err)
		}
		ids := obs.take()
		if len(ids) != 1 {
			t.Fatalf("decisions = %v, want one", ids)
		}
		return ids[0]
	}
	sess := domain.WithSessionID(ctx, "conv-eng2068")
	first := recall(sess, "where is the quarterly report")
	retry := recall(sess, "where is the quarterly report")
	t.Logf("first %s, retry %s", first, retry)
	if first != retry {
		t.Fatalf("a retried request must land under the same QueryID: %s vs %s", first, retry)
	}
	if want := domain.DerivedQueryCorrelation(sess, "agent:e2e", "where is the quarterly report") + "-h1"; first != want {
		t.Fatalf("QueryID %s, want %s (session + principal + question, hop 1)", first, want)
	}
	if other := recall(domain.WithSessionID(ctx, "conv-other"), "where is the quarterly report"); other == first {
		t.Fatal("another session must not share the QueryID")
	}
	if other := recall(sess, "who owns the quarterly report"); other == first {
		t.Fatal("another question must not share the QueryID")
	}
	// A seeded boundary handle stays the prefix (ADR-0126 D6).
	if got := recall(domain.WithQueryCorrelation(sess, "mcp-e2e"), "where is the quarterly report"); got != "mcp-e2e-h1" {
		t.Fatalf("seeded handle: QueryID %s, want mcp-e2e-h1", got)
	}
	// No clock in it: nothing but the derived shape.
	if !strings.HasPrefix(first, "q-") || len(first) != len("q-")+32+len("-h1") {
		t.Fatalf("QueryID %q is not q-<32 hex>-h<n>", first)
	}
}
