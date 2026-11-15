package network

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/llmusage"
)

// trailerStream captures what a handler sets with grpc.SetTrailer, so a unit test
// can read the trailer without a network round trip.
type trailerStream struct {
	mu      sync.Mutex
	trailer metadata.MD
}

func (s *trailerStream) Method() string               { return "/test" }
func (s *trailerStream) SetHeader(metadata.MD) error  { return nil }
func (s *trailerStream) SendHeader(metadata.MD) error { return nil }
func (s *trailerStream) SetTrailer(md metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trailer = metadata.Join(s.trailer, md)
	return nil
}

func (s *trailerStream) get(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trailer.Get(key)
}

// usageFields parses the llmusage.Trailer grammar into a map.
func usageFields(t *testing.T, v string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed trailer pair %q in %q", kv, v)
		}
		out[k] = val
	}
	return out
}

// streamingAgentCaller plays the retrieval_agent: it generates through the
// gateway under the lease the dispatcher injected, on a FRESH ctx — as the real
// agent does over its own GenerateViaModelStream RPC — so the only link back to
// the QueryMemory that started the chain is the lease.
type streamingAgentCaller struct{ gw *SubstrateLLMGateway }

func (c *streamingAgentCaller) CallAgent(_ context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	out := make(chan domain.StreamChunk, 8)
	if err := c.gw.StreamChunks(context.Background(), domain.LeaseID(h.Context["_session_token_id"]), domain.UserPrompt("decompose"), out); err != nil {
		return nil, err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`{"sub_questions":["a"],"refs":[]}`)}}, nil
}

// decomposingSearcher runs one agentic-retrieval dispatch per Search, like the
// default-on decomposition does.
type decomposingSearcher struct {
	mockMemorySearcher
	d *RetrievalDispatcher
}

func (s *decomposingSearcher) Search(ctx context.Context, q, _ string) ([]domain.SearchResult, error) {
	if _, _, err := s.d.Decompose(ctx, q); err != nil {
		return nil, err
	}
	return nil, nil
}

// QueryMemory reports the kernel's own LLM calls for the recall in the
// cambrian-llm-usage trailer — including the retrieval_agent's calls, which reach
// the gateway over another RPC and are attributed through the lease.
func TestQueryMemory_TrailerCountsRetrievalAgentCalls(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 40, compl: 10, cacheRead: 8})
	d := &RetrievalDispatcher{Caller: &streamingAgentCaller{gw: gw}, Gateway: gw}
	srv := &Server{MemorySearcher: &decomposingSearcher{d: d}}

	st := &trailerStream{}
	ctx := grpc.NewContextWithServerTransportStream(ctxWithAgentID("asker"), st)
	if _, err := srv.QueryMemory(ctx, &pb.MemoryRequest{Query: "who and when?"}); err != nil {
		t.Fatalf("QueryMemory: %v", err)
	}
	vals := st.get(llmusage.Trailer)
	if len(vals) != 1 {
		t.Fatalf("trailer %s = %v, want one value", llmusage.Trailer, vals)
	}
	f := usageFields(t, vals[0])
	want := map[string]string{
		"calls#": "1", "input#": "40", "output#": "10", "cache_read#": "8", "estimated#": "0",
		"agent_step:retrieval_agent.calls#": "1", "agent_step:retrieval_agent.input#": "40",
	}
	for k, v := range want {
		if f[k] != v {
			t.Fatalf("trailer %s = %q, want %q (trailer %q)", k, f[k], v, vals[0])
		}
	}
}

// A recall that makes no LLM call still sends the trailer: calls#=0 is an
// observed zero, which is what "0 AI calls per search" has to be proved with.
func TestQueryMemory_TrailerReportsObservedZero(t *testing.T) {
	t.Parallel()
	srv := &Server{MemorySearcher: &mockMemorySearcher{}}
	st := &trailerStream{}
	ctx := grpc.NewContextWithServerTransportStream(ctxWithAgentID("asker"), st)
	if _, err := srv.QueryMemory(ctx, &pb.MemoryRequest{Query: "q"}); err != nil {
		t.Fatalf("QueryMemory: %v", err)
	}
	vals := st.get(llmusage.Trailer)
	if len(vals) != 1 || !strings.HasPrefix(vals[0], "calls#=0,") {
		t.Fatalf("trailer = %v, want an observed zero", vals)
	}
}

// factingProcessor plays the ingestion pipeline with the fact lane on and
// sync_ingest_wait_for_facts: the extraction runs on a detached ctx in another
// goroutine and ProcessSync waits for it, as IngestionManager.waitForFacts does.
type factingProcessor struct{ meter *llm.SpendMeter }

func (p *factingProcessor) ProcessSync(ctx context.Context, _ domain.ExternalDocument) (string, error) {
	done := make(chan struct{})
	job := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		p.meter.Record(job, string(domain.PurposeFacts), "", llm.TokenUsage{PromptTokens: 300, CompletionTokens: 60, TotalTokens: 360})
		p.meter.Record(job, string(domain.PurposeFacts), "", llm.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})
	}()
	<-done
	return "source_doc:x", nil
}

func TestIngestMemory_TrailerCountsFactCalls(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	srv := &Server{IngestionProcessor: &factingProcessor{meter: meter}}
	st := &trailerStream{}
	ctx := grpc.NewContextWithServerTransportStream(ctxWithAgentID("writer"), st)
	if _, err := srv.IngestMemory(ctx, &pb.IngestMemoryRequest{Text: "hello", SessionId: "s1"}); err != nil {
		t.Fatalf("IngestMemory: %v", err)
	}
	vals := st.get(llmusage.Trailer)
	if len(vals) != 1 {
		t.Fatalf("trailer = %v, want one value", vals)
	}
	f := usageFields(t, vals[0])
	if f["calls#"] != "2" || f["input#"] != "400" || f["output#"] != "80" || f["facts.calls#"] != "2" {
		t.Fatalf("trailer = %q, want 2 facts calls 400/80", vals[0])
	}
	if len(st.get(IngestTimingTrailer)) != 1 {
		t.Fatal("the ingest-timing trailer must still be sent")
	}
}
