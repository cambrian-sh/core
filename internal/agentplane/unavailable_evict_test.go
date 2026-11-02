package agentplane

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errClient is an AgentServiceClient whose Execute fails with code.
type errClient struct {
	pb.AgentServiceClient
	code codes.Code
}

func (c errClient) Execute(context.Context, *pb.Handoff, ...grpc.CallOption) (*pb.Handoff, error) {
	return nil, status.Error(c.code, "boom")
}

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: an Unavailable Execute
// (the agent died) drops the cached client so the next call re-boots; any other
// error keeps it.
func TestCallAgent_UnavailableDropsCachedClient(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		kept bool
	}{{codes.Unavailable, false}, {codes.Internal, true}} {
		a := New(&countingDialer{})
		a.RegisterAgentClient("kg_extractor_agent", errClient{code: tc.code}, nil)
		if _, err := a.CallAgent(context.Background(), "kg_extractor_agent", &domain.Handoff{}, ""); status.Code(err) != tc.code {
			t.Fatalf("%s: err = %v", tc.code, err)
		}
		_, cerr := a.getAgentClient("kg_extractor_agent")
		if (cerr == nil) != tc.kept {
			t.Fatalf("%s: cached client kept=%v, want %v", tc.code, cerr == nil, tc.kept)
		}
	}
}
