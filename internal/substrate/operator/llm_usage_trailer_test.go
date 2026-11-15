package operator_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/llmusage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ENG-534: the operator plane's memory handlers report the kernel's own LLM
// calls in the cambrian-llm-usage trailer, as the agent plane does. The fakes
// below book usage the way the ADR-0131 SpendMeter does — onto the tally the
// handler put on ctx — so a handler that opens no tally yields no trailer.

// trailerStream captures grpc.SetTrailer without a network round trip.
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

func (s *trailerStream) usage(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	vals := s.trailer.Get(llmusage.Trailer)
	if len(vals) != 1 {
		t.Fatalf("trailer %s = %v, want exactly one value", llmusage.Trailer, vals)
	}
	return vals[0]
}

func bookUsage(ctx context.Context, purpose string) {
	if tally := domain.LLMUsageTallyFrom(ctx); tally != nil {
		tally.Add(purpose, domain.LLMUsage{Calls: 1, InputTokens: 100, OutputTokens: 10})
	}
}

type meteredMemory struct{}

func (meteredMemory) SearchSystem(ctx context.Context, _ string) ([]domain.SearchResult, error) {
	bookUsage(ctx, "memory")
	return nil, nil
}

type meteredAnswerer struct{}

func (meteredAnswerer) AnswerSystem(ctx context.Context, _ string) (string, string, []domain.SearchResult, error) {
	bookUsage(ctx, "agent_step")
	return "answer", "ok", nil, nil
}

type meteredIngestor struct{}

func (meteredIngestor) Ingest(ctx context.Context, _ operator.IngestRequest) (string, error) {
	bookUsage(ctx, "facts")
	return "doc-1", nil
}

func TestOperatorMemoryHandlers_SendLLMUsageTrailer(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(nil, nil, meteredMemory{})
	svc.SetMemoryAnswerer(meteredAnswerer{})
	svc.SetMemoryIngestor(meteredIngestor{})

	cases := []struct {
		name, want string
		call       func(ctx context.Context) error
	}{
		{"QueryMemory", "memory.calls#=1", func(ctx context.Context) error {
			_, err := svc.QueryMemory(ctx, &pb.QueryMemoryRequest{Query: "q"})
			return err
		}},
		{"AnswerMemory", "agent_step.calls#=1", func(ctx context.Context) error {
			_, err := svc.AnswerMemory(ctx, &pb.AnswerMemoryRequest{Query: "q"})
			return err
		}},
		{"IngestMemory", "facts.calls#=1", func(ctx context.Context) error {
			_, err := svc.IngestMemory(ctx, &pb.IngestMemoryOpRequest{CommandId: "c1", Reason: "r", Text: "hello"})
			return err
		}},
	}
	for _, tc := range cases {
		st := &trailerStream{}
		ctx := grpc.NewContextWithServerTransportStream(opCtx(), st)
		if err := tc.call(ctx); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := st.usage(t)
		if !strings.HasPrefix(got, "calls#=1,input#=100,output#=10,") || !strings.Contains(got, tc.want) {
			t.Fatalf("%s trailer = %q, want total 1 call and %q", tc.name, got, tc.want)
		}
	}
}
