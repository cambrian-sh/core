package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"

	"google.golang.org/grpc/metadata"
)

// optInRecordingSearcher records whether the fact-lane call carried the agentic
// synthesis opt-in, and returns a control row so the strip path is exercised.
type optInRecordingSearcher struct {
	mockMemorySearcher
	sawOptIn bool
}

func (m *optInRecordingSearcher) Search(ctx context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.sawOptIn = memory.AgenticSynthesisWanted(ctx)
	return []domain.SearchResult{
		{Document: domain.Document{ID: memory.AgenticControlID, Text: "synth", Metadata: map[string]any{memory.AgenticStatusKey: "answer"}}, Score: 2},
		{Document: domain.Document{ID: "doc-a", Text: "fact"}, Score: 0.9},
	}, nil
}

// T-20261102-agentic-synthesis-llm-call-is-computed-for: x-agentic-control is the
// opt-in for the loop's synthesis, threaded on ctx; without it the searcher is
// told not to synthesize and the response carries evidence only.
func TestQueryMemory_AgenticControlHeaderThreadsSynthesisOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		md      metadata.MD
		want    bool
		wantLen int
	}{
		{"plain agent", metadata.Pairs("x-agent-id", "agent-a"), false, 1},
		{"opted in", metadata.Pairs("x-agent-id", "agent-a", "x-agentic-control", "1"), true, 2},
	} {
		s := &optInRecordingSearcher{}
		resp, err := makeQueryMemoryServer(s).QueryMemory(metadata.NewIncomingContext(context.Background(), tc.md), &pb.MemoryRequest{Query: "q"})
		if err != nil {
			t.Fatal(err)
		}
		if s.sawOptIn != tc.want {
			t.Errorf("%s: searcher saw opt-in=%v, want %v", tc.name, s.sawOptIn, tc.want)
		}
		if len(resp.Results) != tc.wantLen {
			t.Errorf("%s: got %d results, want %d", tc.name, len(resp.Results), tc.wantLen)
		}
	}
}
